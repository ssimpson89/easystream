package app

import (
	"context"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
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

// Server wires the EasyStream HTTP API + background controllers together.
// Handler methods live in handlers_*.go files in this package.
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
	intentPath string // disk persistence for operator intent (crash resume)

	mu                sync.Mutex
	config            ffmpeg.Config
	activeBroadcastID string // YouTube broadcast bound to the current stream
	activeStreamID    string // YouTube stream resource bound to the current broadcast
	streamHealth      streamHealthSnapshot
	destinationBad    int

	// transitionCancel cancels the in-flight YouTube broadcast transition
	// goroutine when the user stops or starts a different broadcast.
	transitionMu     sync.Mutex
	transitionCancel context.CancelFunc

	// healthPollerStop signals the background health poller to exit on
	// Shutdown. Without this, polling continues past server close.
	healthPollerStop chan struct{}

	// hub is the SSE pub/sub: every mutator calls s.publishState() so all
	// open browser tabs see changes in real time without polling.
	hub *hub
}

// markLive persists the operator's intent to be live. Called from every
// successful Go Live path. The intent file's presence is the signal that
// drives auto-resume on the next boot.
func (s *Server) markLive(mode, broadcastID, streamID string) {
	if s.intentPath == "" {
		return
	}
	s.mu.Lock()
	ingestURL := s.config.IngestURL
	streamName := s.config.StreamName
	s.mu.Unlock()
	intent := streamIntent{
		Live:        true,
		Mode:        mode,
		BroadcastID: broadcastID,
		StreamID:    streamID,
		IngestURL:   ingestURL,
		StreamName:  streamName,
		StartedAt:   time.Now().UTC(),
	}
	if err := saveStreamIntent(s.intentPath, intent); err != nil {
		s.logger.Printf("intent: failed to persist live intent: %v", err)
	}
}

// markIdle clears the operator's intent. Called from every Stop path
// (manual, scheduler auto-stop, broadcast complete). Absent intent file
// means "no auto-resume on next boot."
func (s *Server) markIdle() {
	if s.intentPath == "" {
		return
	}
	clearStreamIntent(s.intentPath)
}

func NewServer(cfg ServerConfig) *Server {
	supCfg := ffmpeg.SupervisorConfig{}
	intentPath := ""
	if cfg.DataDir != "" {
		// Track the FFmpeg child PID so an orphan from a previous crash
		// can be reaped on startup before we spawn a new stream.
		supCfg.PidFilePath = filepath.Join(cfg.DataDir, "ffmpeg.pid")
		intentPath = filepath.Join(cfg.DataDir, "intent.json")
	}
	supervisor := ffmpeg.NewSupervisor(cfg.Logger, supCfg)
	prev := preview.NewServer(cfg.Logger)

	var ytClient *youtube.Client
	if cfg.YTAuth != nil {
		ytClient = &youtube.Client{Auth: cfg.YTAuth}
	}

	defaultCfg := ffmpeg.DefaultConfig()
	if cfg.HLSServer != nil {
		defaultCfg.HLSDir = cfg.HLSServer.Dir()
	}

	// Load persisted stream config (stream key, preset, input) from disk so
	// it survives restarts. Falls back to defaults if missing/corrupt.
	configPath := ""
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
			if persisted.Encoder != "" {
				defaultCfg.Encoder = persisted.Encoder
			}
			// Resolve AVFoundation device names to current indexes. Device
			// indexes shift between reboots or when USB devices are
			// plugged/unplugged; the persisted name is the stable identifier.
			backend := defaultCfg.Input.Backend
			if backend == "" {
				backend = ffmpeg.PlatformBackend()
			}
			if backend == "avfoundation" {
				if defaultCfg.Input.VideoDeviceName != "" {
					resolved := ffmpeg.ResolveAVFoundationDeviceIndex(
						defaultCfg.Binary, defaultCfg.Input.VideoDevice,
						defaultCfg.Input.VideoDeviceName, "video")
					if resolved != defaultCfg.Input.VideoDevice {
						cfg.Logger.Printf("resolved video device %q from index %s → %s",
							defaultCfg.Input.VideoDeviceName, defaultCfg.Input.VideoDevice, resolved)
					}
					defaultCfg.Input.VideoDevice = resolved
				}
				if defaultCfg.Input.AudioDeviceName != "" {
					resolved := ffmpeg.ResolveAVFoundationDeviceIndex(
						defaultCfg.Binary, defaultCfg.Input.AudioDevice,
						defaultCfg.Input.AudioDeviceName, "audio")
					if resolved != defaultCfg.Input.AudioDevice {
						cfg.Logger.Printf("resolved audio device %q from index %s → %s",
							defaultCfg.Input.AudioDeviceName, defaultCfg.Input.AudioDevice, resolved)
					}
					defaultCfg.Input.AudioDevice = resolved
				}
			}
			cfg.Logger.Printf("loaded persisted stream config from %s", configPath)
		}
	}

	devScanner := devices.NewScanner(defaultCfg.Binary)
	adaptive := ffmpeg.NewAdaptiveController(supervisor, ffmpeg.DefaultAdaptiveConfig(), cfg.Logger)

	server := &Server{
		addr:             cfg.Addr,
		supervisor:       supervisor,
		adaptive:         adaptive,
		preview:          prev,
		hlsServer:        cfg.HLSServer,
		devScanner:       devScanner,
		ytAuth:           cfg.YTAuth,
		ytClient:         ytClient,
		schedStore:       cfg.ScheduleStore,
		logger:           cfg.Logger,
		configPath:       configPath,
		intentPath:       intentPath,
		config:           defaultCfg,
		healthPollerStop: make(chan struct{}),
		hub:              newHub(),
	}
	// Supervisor restart events drive both the adaptive controller and an
	// SSE push so the UI flips to "Reconnecting" instantly.
	supervisor.SetOnRestart(func() {
		adaptive.OnRestart()
		server.publishState()
	})
	adaptive.Start()

	// Initialize preview with the default config so it knows the input source.
	prev.UpdateConfig(defaultCfg)

	// Create scheduler if we have both YouTube and schedule store.
	if cfg.ScheduleStore != nil {
		var bcastCtrl schedule.BroadcastController
		if ytClient != nil && cfg.YTAuth != nil {
			bcastCtrl = &broadcastControllerAdapter{server: server}
		}
		server.scheduler = schedule.NewScheduler(
			cfg.ScheduleStore,
			&streamControllerAdapter{server: server},
			bcastCtrl,
			cfg.Logger,
		)
		server.scheduler.Start()
	}

	// Start the health poller. When a YouTube broadcast is bound to the
	// active stream, we poll YouTube every 15s for streamStatus/healthStatus
	// so the UI can show "Receiving" / "noData" / "bad" indicators.
	go server.runHealthPoller(server.healthPollerStop)

	// If the previous session was live (crashed mid-stream and got restarted
	// by launchd/systemd), resume the broadcast automatically. The PID
	// reaper above has already killed any zombie FFmpeg; this spawns a
	// fresh one with the same destination so the platform sees a brief
	// reconnect rather than a stream end.
	server.resumeIfNeeded()

	server.httpServer = &http.Server{
		Addr:              cfg.Addr,
		Handler:           logRequests(cfg.Logger, server.routes(cfg.WebFS)),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return server
}

// routes registers every HTTP endpoint. Each route's handler lives in
// handlers_*.go in this package.
func (s *Server) routes(webFS fs.FS) http.Handler {
	mux := http.NewServeMux()
	fileServer := http.FileServer(http.FS(webFS))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		fileServer.ServeHTTP(w, r)
	}))

	// Real-time state push (SSE). All open UIs receive every state change
	// immediately; the REST endpoints below remain as control/fallback.
	mux.HandleFunc("GET /api/stream/state", s.handleEventStream)

	// Stream control.
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/presets", s.handlePresets)
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("POST /api/config", s.handleConfigUpdate)
	mux.HandleFunc("POST /api/start", s.handleStart)
	mux.HandleFunc("POST /api/stop", s.handleStop)
	mux.HandleFunc("POST /api/adaptive", s.handleAdaptiveToggle)
	mux.HandleFunc("POST /api/extend", s.handleExtend)

	// Devices.
	mux.HandleFunc("GET /api/devices", s.handleDevices)
	mux.HandleFunc("GET /api/encoders", s.handleEncoders)

	// Preview (WebRTC SDP offer/answer).
	mux.Handle("POST /api/preview/webrtc/offer", s.preview)

	// HLS output.
	if s.hlsServer != nil {
		mux.Handle("/hls/", s.hlsServer)
	}

	// YouTube auth.
	mux.HandleFunc("GET /api/youtube/auth/status", s.handleYTAuthStatus)
	mux.HandleFunc("GET /api/youtube/auth/url", s.handleYTAuthURL)
	mux.HandleFunc("GET /api/youtube/auth/callback", s.handleYTAuthCallback)
	mux.HandleFunc("POST /api/youtube/auth/logout", s.handleYTLogout)

	// YouTube broadcasts.
	mux.HandleFunc("GET /api/youtube/broadcasts", s.handleYTBroadcasts)
	mux.HandleFunc("POST /api/youtube/go-live-now", s.handleGoLiveNow)
	mux.HandleFunc("POST /api/youtube/complete", s.handleCompleteBroadcast)

	// Schedules.
	mux.HandleFunc("GET /api/schedules", s.handleListSchedules)
	mux.HandleFunc("POST /api/schedules", s.handleCreateSchedule)
	mux.HandleFunc("PUT /api/schedules/{id}", s.handleUpdateSchedule)
	mux.HandleFunc("DELETE /api/schedules/{id}", s.handleDeleteSchedule)

	// Overrides.
	mux.HandleFunc("GET /api/overrides", s.handleListOverrides)
	mux.HandleFunc("POST /api/overrides", s.handleCreateOverride)
	mux.HandleFunc("PUT /api/overrides/{id}", s.handleUpdateOverride)
	mux.HandleFunc("DELETE /api/overrides/{id}", s.handleDeleteOverride)

	// Upcoming events.
	mux.HandleFunc("GET /api/events", s.handleListEvents)

	return mux
}

func (s *Server) Addr() string {
	return s.addr
}

func (s *Server) ListenAndServe() error {
	return s.httpServer.ListenAndServe()
}

func (s *Server) Close() {
	// Cancel the testing→live transition first so it can't fire after
	// supervisor.Stop disowns the broadcast.
	s.cancelTransitionGoroutine()

	// Stop the health poller before the supervisor so it doesn't observe
	// an idle FFmpeg state mid-tear-down and incorrectly mark "destination
	// unhealthy."
	s.stopHealthPollerOnce()

	if s.scheduler != nil {
		s.scheduler.Stop()
	}
	if s.adaptive != nil {
		s.adaptive.Stop()
	}
	if s.supervisor != nil {
		s.supervisor.Stop()
	}
	if s.preview != nil {
		s.preview.Block() // tears down preview's child ffmpeg
	}
}

// stopHealthPollerOnce is safe to call multiple times; closing an already-
// closed channel would panic.
func (s *Server) stopHealthPollerOnce() {
	s.mu.Lock()
	ch := s.healthPollerStop
	s.healthPollerStop = nil
	s.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

// Shutdown gracefully drains the HTTP server then runs Close() to stop
// supervised FFmpeg children. EasyStream keeps one clear owner for FFmpeg;
// crash recovery starts a fresh process from persisted intent rather than
// adopting a blind process with no telemetry.
//
// Intent is preserved across Shutdown so a systemd restart can resume the
// broadcast. If the operator is shutting down permanently (e.g. host
// reboot), the active broadcast remains "live" on YouTube until the
// next startup completes resume — which is exactly when systemd
// restarts EasyStream anyway. For a true clean shutdown the operator
// uses the Stop button in the UI, which calls handleStop and properly
// transitions the broadcast to complete.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.httpServer.Shutdown(ctx)
	s.Close()
	return err
}

// resumeIfNeeded checks the on-disk intent file. If the previous session was
// live and the record is fresh (under maxIntentAge), start a fresh FFmpeg with
// full progress/audio/error telemetry. The supervisor reaps any stale orphan
// first; EasyStream does not adopt blind FFmpeg processes.
func (s *Server) resumeIfNeeded() {
	if s.intentPath == "" {
		return
	}
	intent, err := loadStreamIntent(s.intentPath)
	if err != nil {
		// No intent file = nothing to resume. Quiet success.
		return
	}
	if !intent.Live {
		// Idle intent recorded explicitly — caller chose not to be live.
		return
	}
	if !intent.fresh() {
		s.logger.Printf("resume: intent file is stale (started %s ago) — clearing without resuming",
			time.Since(intent.StartedAt).Round(time.Minute))
		clearStreamIntent(s.intentPath)
		return
	}

	s.mu.Lock()
	if intent.IngestURL != "" {
		s.config.IngestURL = intent.IngestURL
	}
	if intent.StreamName != "" {
		s.config.StreamName = intent.StreamName
	}
	config := s.config
	s.activeBroadcastID = intent.BroadcastID
	s.activeStreamID = intent.StreamID
	s.destinationBad = 0
	s.mu.Unlock()

	// Validate that we have enough config to actually resume.
	if err := config.Validate(); err != nil {
		s.logger.Printf("resume: persisted config invalid (%v) — clearing intent without resuming", err)
		clearStreamIntent(s.intentPath)
		return
	}

	// Start fresh. The platform may see a brief reconnect, but EasyStream keeps
	// full observability and recovery control over the new FFmpeg process.
	if config.OutputMode == ffmpeg.OutputHLS && s.hlsServer != nil {
		_ = s.hlsServer.Clean()
	}
	if s.preview != nil && config.Input.Kind != ffmpeg.InputTestVideo {
		s.preview.Block()
	}
	if err := s.supervisor.Start(config); err != nil {
		s.logger.Printf("resume: failed to restart stream (%v) — clearing intent", err)
		if s.preview != nil {
			s.preview.Unblock()
		}
		clearStreamIntent(s.intentPath)
		return
	}
	if s.adaptive != nil {
		s.adaptive.OnStreamStart(config.Preset.ID)
	}
	mode := intent.Mode
	if mode == "" {
		mode = "unknown"
	}
	s.logger.Printf("resume: restarted stream from previous session (mode=%s broadcast=%q, started %s ago)",
		mode, intent.BroadcastID, time.Since(intent.StartedAt).Round(time.Second))

	// Re-trigger the testing → live transition. The original transition
	// goroutine died with the old server process; without re-triggering,
	// the broadcast stays stuck in "testing" or "ready" and YouTube's
	// player spins indefinitely. Applies to both go-live-now and scheduled
	// modes — same recovery is needed either way.
	if (intent.Mode == "go-live-now" || intent.Mode == "scheduled") &&
		intent.BroadcastID != "" && s.ytClient != nil &&
		s.ytAuth != nil && s.ytAuth.IsAuthenticated() {
		s.logger.Printf("resume: re-triggering YouTube broadcast transition for %s", intent.BroadcastID)
		s.startTransitionGoroutine(intent.BroadcastID, intent.StreamID)
	}
}

// --- Shared helpers ---

func writeJSON(w http.ResponseWriter, status int, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		// Marshal can fail on types JSON doesn't support (Inf, NaN, channels).
		// Return a well-formed error instead of an empty body.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"failed to encode response"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n"))
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func logRequests(logger *log.Logger, next http.Handler) http.Handler {
	if logger == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Printf("panic serving %s %s: %v", r.Method, r.URL.Path, rec)
				writeError(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		if strings.HasPrefix(r.URL.Path, "/api/") {
			logger.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}
