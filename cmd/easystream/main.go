package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/joho/godotenv"
	"github.com/ssimpson89/easystream/internal/app"
	"github.com/ssimpson89/easystream/internal/hls"
	"github.com/ssimpson89/easystream/internal/schedule"
	"github.com/ssimpson89/easystream/internal/ui"
	"github.com/ssimpson89/easystream/internal/youtube"
)

func main() {
	logger := log.New(os.Stdout, "easystream ", log.LstdFlags|log.LUTC)

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

	// Schedule store.
	schedStore, err := schedule.NewStore(filepath.Join(dataDir, "schedules.json"))
	if err != nil {
		logger.Fatalf("failed to initialize schedule store: %v", err)
	}

	// HLS output server.
	hlsDir := filepath.Join(dataDir, "hls")
	hlsServer, err := hls.NewServer(hlsDir)
	if err != nil {
		logger.Fatalf("failed to initialize HLS server: %v", err)
	}
	logger.Printf("HLS output directory: %s", hlsDir)

	server := app.NewServer(app.ServerConfig{
		Addr:            addr,
		WebFS:           ui.FS(),
		Logger:          logger,
		StatusPollEvery: time.Second,
		YTAuth:          ytAuth,
		ScheduleStore:   schedStore,
		HLSServer:       hlsServer,
		DataDir:         dataDir,
	})
	defer server.Close()

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
