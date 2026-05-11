package app

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ssimpson89/easystream/internal/devices"
	"github.com/ssimpson89/easystream/internal/ffmpeg"
	"github.com/ssimpson89/easystream/internal/hls"
	"github.com/ssimpson89/easystream/internal/preview"
	"github.com/ssimpson89/easystream/internal/quality"
	"github.com/ssimpson89/easystream/internal/schedule"
	"github.com/ssimpson89/easystream/internal/youtube"
)

type ServerConfig struct {
	Addr            string
	WebFS           fs.FS
	Logger          *log.Logger
	StatusPollEvery time.Duration
	YTAuth          *youtube.Auth
	ScheduleStore   *schedule.Store
	HLSServer       *hls.Server
	DataDir         string
}

type Server struct {
	addr       string
	httpServer *http.Server
	supervisor *ffmpeg.Supervisor
	adaptive   *ffmpeg.AdaptiveController
	preview    *preview.Server
	hlsServer  *hls.Server
	devScanner *devices.Scanner
	ytAuth     *youtube.Auth
	ytClient   *youtube.Client
	schedStore *schedule.Store
	scheduler  *schedule.Scheduler
	logger     *log.Logger
	configPath string // disk persistence for stream config

	mu                sync.Mutex
	config            ffmpeg.Config
	destinationMode   string // UI hint: which destination tab is active
	activeBroadcastID string // YouTube broadcast bound to the current stream
	activeStreamID    string // YouTube stream resource bound to the current broadcast
	streamHealth      streamHealthSnapshot
}

// streamHealthSnapshot is the latest result of polling the destination
// (currently YouTube) for the bound stream's health.
type streamHealthSnapshot struct {
	StreamStatus  string    `json:"streamStatus,omitempty"`  // active|created|error|inactive|ready
	HealthStatus  string    `json:"healthStatus,omitempty"`  // good|ok|bad|noData
	Issues        []string  `json:"issues,omitempty"`
	LastUpdate    time.Time `json:"lastUpdate,omitempty"`
	Source        string    `json:"source,omitempty"` // "youtube" | "" if not available
	HasBroadcast  bool      `json:"hasBroadcast"`
}

// persistedConfig is a subset of ffmpeg.Config we save across restarts,
// plus a few UI-only fields (active destination tab) so the UI restores
// exactly as the user left it. HLSDir and Binary are recomputed at
// startup; Network is fixed.
type persistedConfig struct {
	PresetID        string            `json:"presetId"`
	OutputMode      ffmpeg.OutputMode `json:"outputMode"`
	IngestURL       string            `json:"ingestUrl"`
	StreamName      string            `json:"streamName"`
	Input           ffmpeg.Input      `json:"input"`
	DestinationMode string            `json:"destinationMode,omitempty"` // "scheduled" | "now" | "manual"
}

func loadPersistedConfig(path string) (persistedConfig, error) {
	var p persistedConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(data, &p)
	return p, err
}

func savePersistedConfig(path string, cfg ffmpeg.Config) error {
	p := persistedConfig{
		PresetID:   cfg.Preset.ID,
		OutputMode: cfg.OutputMode,
		IngestURL:  cfg.IngestURL,
		StreamName: cfg.StreamName,
		Input:      cfg.Input,
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func NewServer(cfg ServerConfig) *Server {
	supervisor := ffmpeg.NewSupervisor(cfg.Logger, ffmpeg.SupervisorConfig{})
	prev := preview.NewServer(cfg.Logger)

	var ytClient *youtube.Client
	if cfg.YTAuth != nil {
		ytClient = &youtube.Client{Auth: cfg.YTAuth}
	}

	defaultCfg := ffmpeg.DefaultConfig()
	if cfg.HLSServer != nil {
		defaultCfg.HLSDir = cfg.HLSServer.Dir()
	}

	// Load persisted stream config (stream key, preset, input, destination
	// tab) from disk so it survives restarts. Falls back to defaults if
	// missing/corrupt.
	configPath := ""
	destinationMode := "scheduled" // default tab
	if cfg.DataDir != "" {
		configPath = filepath.Join(cfg.DataDir, "stream-config.json")
		if persisted, err := loadPersistedConfig(configPath); err == nil {
			if persisted.PresetID != "" {
				if preset, ok := quality.ByID(persisted.PresetID); ok {
					defaultCfg.Preset = preset
				}
			}
			if persisted.OutputMode != "" {
				defaultCfg.OutputMode = persisted.OutputMode
			}
			if persisted.IngestURL != "" {
				defaultCfg.IngestURL = persisted.IngestURL
			}
			if persisted.StreamName != "" {
				defaultCfg.StreamName = persisted.StreamName
			}
			if persisted.Input.Kind != "" {
				defaultCfg.Input = persisted.Input
			}
			if persisted.DestinationMode != "" {
				destinationMode = persisted.DestinationMode
			}
			cfg.Logger.Printf("loaded persisted stream config from %s", configPath)
		}
	}

	devScanner := devices.NewScanner(defaultCfg.Binary)
	adaptive := ffmpeg.NewAdaptiveController(supervisor, ffmpeg.DefaultAdaptiveConfig(), cfg.Logger)

	server := &Server{
		addr:            cfg.Addr,
		supervisor:      supervisor,
		adaptive:        adaptive,
		preview:         prev,
		hlsServer:       cfg.HLSServer,
		devScanner:      devScanner,
		ytAuth:          cfg.YTAuth,
		ytClient:        ytClient,
		schedStore:      cfg.ScheduleStore,
		logger:          cfg.Logger,
		configPath:      configPath,
		config:          defaultCfg,
		destinationMode: destinationMode,
	}
	supervisor.SetOnRestart(adaptive.OnRestart)
	adaptive.Start()

	// Initialize preview with the default config so it knows the input source.
	prev.UpdateConfig(defaultCfg)

	// Create scheduler if we have both YouTube and schedule store.
	if cfg.ScheduleStore != nil {
		var ytCtrl schedule.YouTubeController
		if ytClient != nil && cfg.YTAuth != nil {
			ytCtrl = &ytControllerAdapter{client: ytClient, auth: cfg.YTAuth}
		}
		server.scheduler = schedule.NewScheduler(
			cfg.ScheduleStore,
			&streamControllerAdapter{server: server},
			ytCtrl,
			cfg.Logger,
		)
		server.scheduler.Start()
	}

	// Start the health poller. When a YouTube broadcast is bound to the
	// active stream, we poll YouTube every 15s for streamStatus/healthStatus
	// so the UI can show "Receiving" / "noData" / "bad" indicators.
	go server.runHealthPoller()

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(cfg.WebFS)))

	// Stream control.
	mux.HandleFunc("GET /api/status", server.handleStatus)
	mux.HandleFunc("GET /api/presets", server.handlePresets)
	mux.HandleFunc("GET /api/config", server.handleConfig)
	mux.HandleFunc("POST /api/config", server.handleConfigUpdate)
	mux.HandleFunc("POST /api/start", server.handleStart)
	mux.HandleFunc("POST /api/stop", server.handleStop)
	mux.HandleFunc("POST /api/adaptive", server.handleAdaptiveToggle)
	mux.HandleFunc("POST /api/extend", server.handleExtend)

	// Devices.
	mux.HandleFunc("GET /api/devices", server.handleDevices)

	// Preview.
	mux.Handle("GET /api/preview", prev)

	// HLS output.
	if cfg.HLSServer != nil {
		mux.Handle("/hls/", cfg.HLSServer)
	}

	// YouTube auth.
	mux.HandleFunc("GET /api/youtube/auth/status", server.handleYTAuthStatus)
	mux.HandleFunc("GET /api/youtube/auth/url", server.handleYTAuthURL)
	mux.HandleFunc("GET /api/youtube/auth/callback", server.handleYTAuthCallback)
	mux.HandleFunc("POST /api/youtube/auth/logout", server.handleYTLogout)

	// YouTube broadcasts.
	mux.HandleFunc("GET /api/youtube/broadcasts", server.handleYTBroadcasts)
	mux.HandleFunc("POST /api/youtube/go-live-now", server.handleGoLiveNow)
	mux.HandleFunc("POST /api/youtube/complete", server.handleCompleteBroadcast)

	// Schedules.
	mux.HandleFunc("GET /api/schedules", server.handleListSchedules)
	mux.HandleFunc("POST /api/schedules", server.handleCreateSchedule)
	mux.HandleFunc("DELETE /api/schedules/{id}", server.handleDeleteSchedule)

	// Overrides.
	mux.HandleFunc("GET /api/overrides", server.handleListOverrides)
	mux.HandleFunc("POST /api/overrides", server.handleCreateOverride)
	mux.HandleFunc("DELETE /api/overrides/{id}", server.handleDeleteOverride)

	// Upcoming events.
	mux.HandleFunc("GET /api/events", server.handleListEvents)

	server.httpServer = &http.Server{
		Addr:              cfg.Addr,
		Handler:           logRequests(cfg.Logger, mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return server
}

func (s *Server) Addr() string {
	return s.addr
}

func (s *Server) ListenAndServe() error {
	return s.httpServer.ListenAndServe()
}

func (s *Server) Close() {
	if s.scheduler != nil {
		s.scheduler.Stop()
	}
	if s.adaptive != nil {
		s.adaptive.Stop()
	}
}

// --- Stream control handlers ---

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	config := s.config
	health := s.streamHealth
	broadcastID := s.activeBroadcastID
	destMode := s.destinationMode
	s.mu.Unlock()

	streamStatus := s.supervisor.Status()
	confidence := computeConfidence(streamStatus, health, broadcastID, destMode)

	result := map[string]any{
		"stream":       streamStatus,
		"config":       s.configResponse(config),
		"presets":      quality.Selectable(),
		"platform":     ffmpeg.PlatformBackend(),
		"health":       health,
		"confidence":   confidence,
		"activeBroadcastId": broadcastID,
	}
	if s.adaptive != nil {
		result["adaptive"] = s.adaptive.State()
	}
	if s.ytAuth != nil {
		result["youtube"] = s.ytAuth.AuthStatus()
	}
	if s.scheduler != nil {
		result["scheduler"] = s.scheduler.Status()
	}
	if s.schedStore != nil {
		result["nextEvents"] = s.schedStore.NextEvents(5, time.Now().UTC())
	}
	writeJSON(w, http.StatusOK, result)
}

// confidenceIndicator is a traffic-light view of one stream characteristic
// for the operator's "is the broadcast actually working?" panel.
type confidenceIndicator struct {
	Label  string `json:"label"`
	Status string `json:"status"` // "green" | "yellow" | "red" | "unknown" | "off"
	Detail string `json:"detail,omitempty"`
}

func computeConfidence(stream ffmpeg.Status, health streamHealthSnapshot, broadcastID, destMode string) []confidenceIndicator {
	out := []confidenceIndicator{}

	// 1. Encoder is sending.
	enc := confidenceIndicator{Label: "Encoder"}
	switch stream.State {
	case ffmpeg.StateRunning:
		enc.Status = "green"
		enc.Detail = "Sending data"
	case ffmpeg.StateDegraded:
		enc.Status = "yellow"
		enc.Detail = "Stalled or backed up"
	case ffmpeg.StateRestarting, ffmpeg.StateStarting:
		enc.Status = "yellow"
		enc.Detail = "Reconnecting"
	case ffmpeg.StateFailed:
		enc.Status = "red"
		enc.Detail = "Stopped on error"
	default:
		enc.Status = "off"
		enc.Detail = "Idle"
	}
	out = append(out, enc)

	// 2. Audio detected.
	aud := confidenceIndicator{Label: "Audio"}
	if stream.State != ffmpeg.StateRunning && stream.State != ffmpeg.StateDegraded {
		aud.Status = "off"
	} else if stream.AudioRMSAt.IsZero() {
		aud.Status = "unknown"
		aud.Detail = "Waiting for level..."
	} else if !stream.AudioDetectedAt.IsZero() && time.Since(stream.AudioDetectedAt) < 5*time.Second {
		aud.Status = "green"
		aud.Detail = fmt.Sprintf("%.0f dB RMS", stream.AudioRMSdB)
	} else {
		aud.Status = "red"
		aud.Detail = "Silence detected — check mic / source"
	}
	out = append(out, aud)

	// 3. Destination receiving (YouTube-side health, when available).
	dest := confidenceIndicator{Label: "Destination"}
	switch {
	case stream.State != ffmpeg.StateRunning && stream.State != ffmpeg.StateDegraded:
		dest.Status = "off"
	case broadcastID == "" || health.Source == "":
		// Manual RTMP / no YT API — we can only infer from encoder
		dest.Status = "unknown"
		dest.Detail = "No platform feedback (manual RTMP)"
	case health.HealthStatus == "good":
		dest.Status = "green"
		dest.Detail = "YouTube: good"
	case health.HealthStatus == "ok":
		dest.Status = "green"
		dest.Detail = "YouTube: ok"
	case health.HealthStatus == "bad":
		dest.Status = "yellow"
		dest.Detail = "YouTube reports issues"
		if len(health.Issues) > 0 {
			dest.Detail = "YouTube: " + health.Issues[0]
		}
	case health.HealthStatus == "noData":
		dest.Status = "red"
		dest.Detail = "YouTube not receiving"
	default:
		dest.Status = "unknown"
		dest.Detail = "Checking..."
	}
	out = append(out, dest)

	return out
}

func (s *Server) handlePresets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, quality.Selectable())
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	config := s.config
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, s.configResponse(config))
}

func (s *Server) handleConfigUpdate(w http.ResponseWriter, r *http.Request) {
	var patch struct {
		FFmpegBinary *string            `json:"ffmpegBinary"`
		PresetID     *string            `json:"presetId"`
		OutputMode   *ffmpeg.OutputMode `json:"outputMode"`
		IngestURL    *string            `json:"ingestUrl"`
		StreamName   *string            `json:"streamName"`
		Input        *ffmpeg.Input      `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.mu.Lock()
	if patch.FFmpegBinary != nil && *patch.FFmpegBinary != "" {
		s.config.Binary = *patch.FFmpegBinary
	}
	if patch.PresetID != nil && *patch.PresetID != "" {
		preset, ok := quality.ByID(*patch.PresetID)
		if !ok {
			s.mu.Unlock()
			writeError(w, http.StatusBadRequest, "unknown quality preset")
			return
		}
		s.config.Preset = preset
	}
	if patch.OutputMode != nil {
		s.config.OutputMode = *patch.OutputMode
	}
	if patch.IngestURL != nil {
		s.config.IngestURL = strings.TrimSpace(*patch.IngestURL)
	}
	if patch.StreamName != nil {
		s.config.StreamName = strings.TrimSpace(*patch.StreamName)
	}
	if patch.Input != nil {
		s.config.Input = *patch.Input
	}

	s.preview.UpdateConfig(s.config)
	config := s.config
	s.mu.Unlock()
	if s.configPath != "" {
		if err := savePersistedConfig(s.configPath, config); err != nil {
			s.logger.Printf("failed to persist stream config: %v", err)
		}
	}
	writeJSON(w, http.StatusOK, s.configResponse(config))
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	config := s.config
	s.mu.Unlock()

	// Clean old HLS segments before starting a new stream.
	if config.OutputMode == ffmpeg.OutputHLS && s.hlsServer != nil {
		_ = s.hlsServer.Clean()
	}

	// Release the capture device from the preview before the main stream
	// claims it. On macOS, only one process can hold a camera at a time.
	if s.preview != nil && config.Input.Kind != ffmpeg.InputTestVideo {
		s.preview.Block()
	}

	if err := s.supervisor.Start(config); err != nil {
		// Unblock so the preview can resume if start failed.
		if s.preview != nil {
			s.preview.Unblock()
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.adaptive != nil {
		s.adaptive.OnStreamStart(config.Preset.ID)
	}
	writeJSON(w, http.StatusAccepted, s.supervisor.Status())
}

// runHealthPoller polls YouTube for the bound stream's health every 15s
// while we have an active broadcast. Updates s.streamHealth so the UI
// confidence indicators reflect what YouTube actually sees.
func (s *Server) runHealthPoller() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.pollStreamHealth()
	}
}

func (s *Server) pollStreamHealth() {
	s.mu.Lock()
	streamID := s.activeStreamID
	hasBroadcast := s.activeBroadcastID != ""
	s.mu.Unlock()

	// Only poll when we have a bound stream and YT auth.
	if streamID == "" || s.ytClient == nil || s.ytAuth == nil || !s.ytAuth.IsAuthenticated() {
		s.mu.Lock()
		s.streamHealth = streamHealthSnapshot{HasBroadcast: hasBroadcast}
		s.mu.Unlock()
		return
	}

	health, err := s.ytClient.GetStreamHealth(streamID)
	if err != nil {
		s.logger.Printf("health poll: %v", err)
		return
	}
	s.mu.Lock()
	s.streamHealth = streamHealthSnapshot{
		StreamStatus: health.StreamStatus,
		HealthStatus: health.HealthStatus,
		Issues:       health.Issues,
		LastUpdate:   time.Now().UTC(),
		Source:       "youtube",
		HasBroadcast: true,
	}
	s.mu.Unlock()
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if s.scheduler != nil {
		s.scheduler.StopActive()
	}
	s.supervisor.Stop()
	if s.adaptive != nil {
		s.adaptive.OnStreamStop()
	}
	if s.preview != nil {
		s.preview.Unblock()
	}
	// If a YouTube broadcast was bound to this stream, transition it to
	// "complete" so viewers see "stream ended" instead of "reconnecting"
	// followed by a YouTube-side timeout.
	s.mu.Lock()
	broadcastID := s.activeBroadcastID
	s.activeBroadcastID = ""
	s.activeStreamID = ""
	s.streamHealth = streamHealthSnapshot{}
	s.mu.Unlock()
	if broadcastID != "" && s.ytClient != nil && s.ytAuth.IsAuthenticated() {
		go func(id string) {
			if err := s.ytClient.TransitionBroadcast(id, "complete"); err != nil {
				s.logger.Printf("stop: complete broadcast %s: %v", id, err)
			} else {
				s.logger.Printf("stop: broadcast %s transitioned to complete", id)
			}
		}(broadcastID)
	}
	writeJSON(w, http.StatusOK, s.supervisor.Status())
}

// handleExtend bumps the auto-stop time of the currently-active scheduled event
// by N minutes (default 15). For services that run long.
func (s *Server) handleExtend(w http.ResponseWriter, r *http.Request) {
	if s.scheduler == nil {
		writeError(w, http.StatusBadRequest, "scheduler not available")
		return
	}
	var body struct {
		Minutes int `json:"minutes"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // tolerate empty body — defaults to 15
	endsAt, err := s.scheduler.Extend(body.Minutes)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"endsAt": endsAt})
}

func (s *Server) handleAdaptiveToggle(w http.ResponseWriter, r *http.Request) {
	if s.adaptive == nil {
		writeError(w, http.StatusBadRequest, "adaptive controller not available")
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.adaptive.SetEnabled(body.Enabled)
	writeJSON(w, http.StatusOK, s.adaptive.State())
}

// --- Device discovery handler ---

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("refresh") == "1" {
		s.devScanner.Invalidate()
	}
	writeJSON(w, http.StatusOK, s.devScanner.Scan())
}

// --- YouTube auth handlers ---

func (s *Server) handleYTAuthStatus(w http.ResponseWriter, r *http.Request) {
	if s.ytAuth == nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false, "authenticated": false})
		return
	}
	writeJSON(w, http.StatusOK, s.ytAuth.AuthStatus())
}

func (s *Server) handleYTAuthURL(w http.ResponseWriter, r *http.Request) {
	if s.ytAuth == nil {
		writeError(w, http.StatusBadRequest, "YouTube credentials not configured. Set YOUTUBE_CLIENT_ID and YOUTUBE_CLIENT_SECRET environment variables.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": s.ytAuth.AuthURL()})
}

func (s *Server) handleYTAuthCallback(w http.ResponseWriter, r *http.Request) {
	if s.ytAuth == nil {
		http.Error(w, "YouTube not configured", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	errParam := r.URL.Query().Get("error")

	if errParam != "" {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<html><body><h2>YouTube login cancelled</h2><p>` + errParam + `</p><p>You can close this tab.</p><script>window.close()</script></body></html>`))
		return
	}
	if code == "" {
		http.Error(w, "missing code parameter", http.StatusBadRequest)
		return
	}
	if err := s.ytAuth.Exchange(code, state); err != nil {
		http.Error(w, "Authentication failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<html><body><h2>YouTube connected!</h2><p>You can close this tab and return to EasyStream.</p><script>window.close()</script></body></html>`))
}

func (s *Server) handleYTLogout(w http.ResponseWriter, r *http.Request) {
	if s.ytAuth != nil {
		_ = s.ytAuth.Logout()
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}

// --- YouTube broadcast handlers ---

func (s *Server) handleYTBroadcasts(w http.ResponseWriter, r *http.Request) {
	if s.ytClient == nil || !s.ytAuth.IsAuthenticated() {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	broadcasts, err := s.ytClient.ListUpcomingBroadcasts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, broadcasts)
}

func (s *Server) handleGoLiveNow(w http.ResponseWriter, r *http.Request) {
	if s.ytClient == nil || !s.ytAuth.IsAuthenticated() {
		writeError(w, http.StatusBadRequest, "not authenticated with YouTube")
		return
	}

	var body struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Privacy     string `json:"privacy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Title == "" {
		body.Title = "Live Stream"
	}
	if body.Privacy == "" {
		body.Privacy = "unlisted"
	}

	s.mu.Lock()
	config := s.config
	s.mu.Unlock()

	// Create broadcast.
	broadcast, err := s.ytClient.CreateBroadcast(body.Title, body.Description, time.Now(), body.Privacy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create broadcast: "+err.Error())
		return
	}

	// Ensure stream.
	streamTitle := "EasyStream - " + config.Preset.Name
	stream, err := s.ytClient.EnsureStream(streamTitle, config.Preset.Resolution(), config.Preset.FPS)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create stream: "+err.Error())
		return
	}

	// Bind.
	if err := s.ytClient.BindBroadcast(broadcast.ID, stream.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "bind broadcast: "+err.Error())
		return
	}

	// Configure FFmpeg with stream ingest details and start.
	s.mu.Lock()
	s.config.IngestURL = stream.IngestURL
	s.config.StreamName = stream.StreamKey
	s.activeBroadcastID = broadcast.ID
	s.activeStreamID = stream.ID
	startConfig := s.config
	s.mu.Unlock()

	if s.preview != nil && startConfig.Input.Kind != ffmpeg.InputTestVideo {
		s.preview.Block()
	}
	if err := s.supervisor.Start(startConfig); err != nil {
		s.mu.Lock()
		s.activeBroadcastID = ""
		s.activeStreamID = ""
		s.mu.Unlock()
		if s.preview != nil {
			s.preview.Unblock()
		}
		writeError(w, http.StatusBadRequest, "start ffmpeg: "+err.Error())
		return
	}

	// Transition in background.
	go func() {
		adapter := &ytControllerAdapter{client: s.ytClient, auth: s.ytAuth}
		for i := 0; i < 30; i++ {
			time.Sleep(5 * time.Second)
			if err := adapter.TransitionBroadcast(broadcast.ID, "testing"); err != nil {
				s.logger.Printf("go-live-now: transition to testing attempt %d: %v", i+1, err)
				continue
			}
			s.logger.Printf("go-live-now: broadcast %s in testing", broadcast.ID)
			break
		}
		time.Sleep(10 * time.Second)
		for i := 0; i < 10; i++ {
			if err := adapter.TransitionBroadcast(broadcast.ID, "live"); err != nil {
				s.logger.Printf("go-live-now: transition to live attempt %d: %v", i+1, err)
				time.Sleep(5 * time.Second)
				continue
			}
			s.logger.Printf("go-live-now: broadcast %s is LIVE", broadcast.ID)
			return
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"broadcast": broadcast,
		"stream":    stream,
	})
}

func (s *Server) handleCompleteBroadcast(w http.ResponseWriter, r *http.Request) {
	if s.ytClient == nil || !s.ytAuth.IsAuthenticated() {
		writeError(w, http.StatusBadRequest, "not authenticated with YouTube")
		return
	}

	var body struct {
		BroadcastID string `json:"broadcastId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Stop FFmpeg first.
	s.supervisor.Stop()
	if s.preview != nil {
		s.preview.Unblock()
	}

	if body.BroadcastID != "" {
		if err := s.ytClient.TransitionBroadcast(body.BroadcastID, "complete"); err != nil {
			s.logger.Printf("complete broadcast: %v", err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "completed"})
}

// --- Schedule handlers ---

func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	if s.schedStore == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, s.schedStore.Schedules())
}

func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	if s.schedStore == nil {
		writeError(w, http.StatusBadRequest, "schedule storage not configured")
		return
	}
	var sched schedule.Schedule
	if err := json.NewDecoder(r.Body).Decode(&sched); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := s.schedStore.CreateSchedule(sched)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	if s.schedStore == nil {
		writeError(w, http.StatusBadRequest, "schedule storage not configured")
		return
	}
	id := r.PathValue("id")
	if err := s.schedStore.DeleteSchedule(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- Override handlers ---

func (s *Server) handleListOverrides(w http.ResponseWriter, r *http.Request) {
	if s.schedStore == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, s.schedStore.Overrides())
}

func (s *Server) handleCreateOverride(w http.ResponseWriter, r *http.Request) {
	if s.schedStore == nil {
		writeError(w, http.StatusBadRequest, "schedule storage not configured")
		return
	}
	var o schedule.Override
	if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := s.schedStore.CreateOverride(o)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) handleDeleteOverride(w http.ResponseWriter, r *http.Request) {
	if s.schedStore == nil {
		writeError(w, http.StatusBadRequest, "schedule storage not configured")
		return
	}
	id := r.PathValue("id")
	if err := s.schedStore.DeleteOverride(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- Upcoming events ---

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	if s.schedStore == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, s.schedStore.NextEvents(20, time.Now().UTC()))
}

// --- Helpers ---

func (s *Server) configResponse(config ffmpeg.Config) map[string]any {
	outputMode := string(config.OutputMode)
	if outputMode == "" {
		outputMode = "rtmp"
	}
	result := map[string]any{
		"ffmpegBinary": config.Binary,
		"input":        config.Input,
		"preset":       config.Preset,
		"outputMode":   outputMode,
		"ingestUrl":    config.IngestURL,
		"hasStreamKey": config.StreamName != "",
		"network":      config.Network,
	}
	if s.hlsServer != nil {
		result["hlsUrl"] = "http://" + s.addr + "/hls/stream.m3u8"
	}
	return result
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func logRequests(logger *log.Logger, next http.Handler) http.Handler {
	if logger == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			logger.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

// --- Adapter types to satisfy scheduler interfaces ---

type streamControllerAdapter struct {
	server *Server
}

func (a *streamControllerAdapter) StartWithIngest(presetID, ingestURL, streamKey, broadcastID, streamID string) error {
	a.server.mu.Lock()
	if presetID != "" {
		if preset, ok := quality.ByID(presetID); ok {
			a.server.config.Preset = preset
		}
	}
	a.server.config.IngestURL = ingestURL
	a.server.config.StreamName = streamKey
	a.server.activeBroadcastID = broadcastID
	a.server.activeStreamID = streamID
	config := a.server.config
	a.server.mu.Unlock()
	if a.server.preview != nil && config.Input.Kind != ffmpeg.InputTestVideo {
		a.server.preview.Block()
	}
	if err := a.server.supervisor.Start(config); err != nil {
		a.server.mu.Lock()
		a.server.activeBroadcastID = ""
		a.server.activeStreamID = ""
		a.server.mu.Unlock()
		if a.server.preview != nil {
			a.server.preview.Unblock()
		}
		return err
	}
	return nil
}

func (a *streamControllerAdapter) StopStream() {
	a.server.supervisor.Stop()
	if a.server.preview != nil {
		a.server.preview.Unblock()
	}
	// Caller (scheduler) already calls TransitionBroadcast itself; just clear local state.
	a.server.mu.Lock()
	a.server.activeBroadcastID = ""
	a.server.activeStreamID = ""
	a.server.streamHealth = streamHealthSnapshot{}
	a.server.mu.Unlock()
}

func (a *streamControllerAdapter) IsStreaming() bool {
	status := a.server.supervisor.Status()
	return status.State == ffmpeg.StateRunning || status.State == ffmpeg.StateDegraded || status.State == ffmpeg.StateStarting
}

type ytControllerAdapter struct {
	client *youtube.Client
	auth   *youtube.Auth
}

func (a *ytControllerAdapter) IsAuthenticated() bool {
	return a.auth.IsAuthenticated()
}

func (a *ytControllerAdapter) CreateBroadcast(title, description string, scheduledStart time.Time, privacy string) (string, error) {
	b, err := a.client.CreateBroadcast(title, description, scheduledStart, privacy)
	if err != nil {
		return "", err
	}
	return b.ID, nil
}

func (a *ytControllerAdapter) EnsureStream(presetID string) (streamID, ingestURL, streamKey string, err error) {
	preset, ok := quality.ByID(presetID)
	if !ok {
		preset = quality.Default()
	}
	title := "EasyStream - " + preset.Name
	stream, err := a.client.EnsureStream(title, preset.Resolution(), preset.FPS)
	if err != nil {
		return "", "", "", err
	}
	return stream.ID, stream.IngestURL, stream.StreamKey, nil
}

func (a *ytControllerAdapter) BindBroadcast(broadcastID, streamID string) error {
	return a.client.BindBroadcast(broadcastID, streamID)
}

func (a *ytControllerAdapter) TransitionBroadcast(broadcastID, status string) error {
	return a.client.TransitionBroadcast(broadcastID, status)
}
