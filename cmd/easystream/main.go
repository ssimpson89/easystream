package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	// Embed Go's tzdata. macOS and most Linux distros ship system
	// zoneinfo, but minimal containers, NixOS images without
	// `localization.timeZone`, and older systems can have stale or
	// missing zones — which silently breaks DST-aware schedule firing.
	_ "time/tzdata"

	"github.com/joho/godotenv"
	"github.com/ssimpson89/easystream/internal/app"
	"github.com/ssimpson89/easystream/internal/hls"
	"github.com/ssimpson89/easystream/internal/preview"
	"github.com/ssimpson89/easystream/internal/schedule"
	"github.com/ssimpson89/easystream/internal/ui"
	"github.com/ssimpson89/easystream/internal/version"
	"github.com/ssimpson89/easystream/internal/youtube"
)

// findFFmpeg locates the ffmpeg binary across common install locations.
// macOS apps launched from Finder/launchd inherit a minimal PATH
// (/usr/bin:/bin:/usr/sbin:/sbin) — Homebrew (/opt/homebrew/bin on
// Apple Silicon, /usr/local/bin on Intel) and MacPorts (/opt/local/bin)
// aren't on it. Without this, every FFmpeg probe (device list,
// encoder detect, framerate) silently returns empty and the daemon
// fails to start with a confusing error.
//
// Prefers `ffmpeg-full` (Homebrew keg-only formula that bundles all
// codecs + libsrt) over the default `ffmpeg` formula, since the
// stripped-down default is missing protocols people actually need
// (SRT, RIST, etc.). EasyStream auto-picks ffmpeg-full when present.
func findFFmpeg() string {
	// Keg-only ffmpeg-full first — most feature-complete on Homebrew.
	for _, candidate := range []string{
		"/opt/homebrew/opt/ffmpeg-full/bin/ffmpeg", // Apple Silicon
		"/usr/local/opt/ffmpeg-full/bin/ffmpeg",    // Intel
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	for _, candidate := range []string{
		"/opt/homebrew/bin/ffmpeg",
		"/usr/local/bin/ffmpeg",
		"/opt/local/bin/ffmpeg",
		"/usr/bin/ffmpeg",
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "ffmpeg" // last resort; supervisor will surface the error
}

func main() {
	logger := log.New(os.Stdout, "easystream ", log.LstdFlags|log.LUTC)
	logger.Printf("EasyStream %s", version.Version)

	// Reap any leftover ffmpeg children from a previous EasyStream session.
	// Without this, orphans accumulate on every restart (or crash) and all
	// write to the same RTP port, garbling the preview. The supervisor's
	// pid file already handles the main-stream ffmpeg; this catches the
	// preview ffmpeg and any post-tee secondary outputs.
	preview.ReapOrphans(logger)

	// Load .env from the current directory if present. godotenv's default
	// Load() does NOT override values already in os.Environ, so real env
	// vars still take precedence over the file. Allows simple local config
	// without having to export variables, while keeping prod deployments
	// where env vars come from systemd / docker / etc. working unchanged.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		logger.Printf("note: .env present but could not be loaded: %v", err)
	}

	addr := envDefault("EASYSTREAM_ADDR", "127.0.0.1:8080")
	dataDir := envDefault("EASYSTREAM_DATA_DIR", defaultDataDir())

	// YouTube OAuth (optional — app works without it in manual mode).
	redirectURI := fmt.Sprintf("http://%s/api/youtube/auth/callback", addr)
	ytAuth := youtube.NewAuth(
		os.Getenv("YOUTUBE_CLIENT_ID"),
		os.Getenv("YOUTUBE_CLIENT_SECRET"),
		redirectURI,
		filepath.Join(dataDir, "tokens.json"),
	)
	if ytAuth != nil {
		// Surface token-refresh persistence failures in the daemon log
		// instead of dropping them silently to disk-full/permission errors.
		ytAuth.SetLogger(logger)
	}
	// Surface real auth state at startup, not just "credentials present."
	// A typo'd client ID, expired token, or revoked OAuth grant should
	// show up here — not when the volunteer clicks Go Live Sunday morning.
	switch {
	case ytAuth == nil:
		logger.Println("YouTube: integration disabled (set YOUTUBE_CLIENT_ID and YOUTUBE_CLIENT_SECRET to enable)")
	case !ytAuth.IsAuthenticated():
		logger.Println("YouTube: credentials present but no saved token — click \"Connect YouTube\" in the UI")
	default:
		if name, err := ytAuth.VerifyAuth(); err != nil {
			logger.Printf("YouTube: token invalid or expired (%v) — re-authenticate via the UI", err)
		} else {
			logger.Printf("YouTube: authenticated as %q", name)
		}
	}

	// Schedule store. A corrupt file is moved aside to schedules.json.corrupt-<unix>
	// and we start with an empty store — better than refusing to launch
	// and missing the next service entirely. The recovery warning is
	// surfaced as a log line so the operator can investigate.
	schedStore, recovery, err := schedule.NewStore(filepath.Join(dataDir, "schedules.json"))
	if err != nil {
		logger.Fatalf("failed to initialize schedule store: %v", err)
	}
	if recovery != nil {
		logger.Printf("schedule store: %v", recovery)
	}

	// HLS output server.
	hlsDir := filepath.Join(dataDir, "hls")
	hlsServer, err := hls.NewServer(hlsDir)
	if err != nil {
		logger.Fatalf("failed to initialize HLS server: %v", err)
	}
	logger.Printf("HLS output directory: %s", hlsDir)

	ffmpegBin := findFFmpeg()
	logger.Printf("ffmpeg binary: %s", ffmpegBin)

	server := app.NewServer(app.ServerConfig{
		Addr:            addr,
		WebFS:           ui.FS(),
		Logger:          logger,
		StatusPollEvery: time.Second,
		YTAuth:          ytAuth,
		ScheduleStore:   schedStore,
		HLSServer:       hlsServer,
		DataDir:         dataDir,
		FFmpegBinary:    ffmpegBin,
	})
	defer server.Close()

	// Graceful shutdown: on SIGTERM/SIGINT, close the server and stop the
	// supervised FFmpeg before exiting. EasyStream intentionally keeps FFmpeg
	// owned by this process so live telemetry and recovery never go blind.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sigCh
		logger.Printf("received %s — shutting down", s)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	logger.Printf("starting web interface on http://%s", server.Addr())
	logger.Printf("HLS playlist URL: http://%s/hls/stream.m3u8", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Fatal(err)
	}
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".easystream"
	}
	return filepath.Join(home, ".easystream")
}
