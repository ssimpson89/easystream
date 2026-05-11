package app

import (
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

	mu                sync.Mutex
	config            ffmpeg.Config
	destinationMode   string // UI hint: which destination tab is active
	activeBroadcastID string // YouTube broadcast bound to the current stream
	activeStreamID    string // YouTube stream resource bound to the current broadcast
	streamHealth      streamHealthSnapshot
}

func NewServer(cfg ServerConfig) *Server {
	supCfg := ffmpeg.SupervisorConfig{}
	if cfg.DataDir != "" {
		// Track the FFmpeg child PID so an orphan from a previous crash
		// can be reaped on startup before we spawn a new stream.
		supCfg.PidFilePath = filepath.Join(cfg.DataDir, "ffmpeg.pid")
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
	mux.Handle("/", http.FileServer(http.FS(webFS)))

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

	// Preview.
	mux.Handle("GET /api/preview", s.preview)

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
	mux.HandleFunc("DELETE /api/schedules/{id}", s.handleDeleteSchedule)

	// Overrides.
	mux.HandleFunc("GET /api/overrides", s.handleListOverrides)
	mux.HandleFunc("POST /api/overrides", s.handleCreateOverride)
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
	if s.scheduler != nil {
		s.scheduler.Stop()
	}
	if s.adaptive != nil {
		s.adaptive.Stop()
	}
}

// --- Shared helpers ---

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
