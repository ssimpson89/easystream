package preview

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"sync"

	"github.com/ssimpson89/easystream/internal/ffmpeg"
)

// Server runs a lightweight FFmpeg process that converts the capture source
// to MJPEG and serves it as a multipart stream the browser can display
// in an <img> tag via the "multipart/x-mixed-replace" content type.
type Server struct {
	mu     sync.Mutex
	logger *log.Logger
	cancel context.CancelFunc
	config ffmpeg.Config
}

// NewServer creates a preview server.
func NewServer(logger *log.Logger) *Server {
	return &Server{logger: logger}
}

// UpdateConfig restarts the preview with a new capture config.
func (s *Server) UpdateConfig(config ffmpeg.Config) {
	s.mu.Lock()
	changed := s.config.Input != config.Input
	s.config = config
	cancel := s.cancel
	s.mu.Unlock()

	if changed && cancel != nil {
		cancel() // restart on next request
	}
}

// ServeHTTP streams MJPEG from the current capture source.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	config := s.config
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	args := previewArgs(config)
	cmd := exec.CommandContext(ctx, config.Binary, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cmd.Stderr = nil // discard

	if err := cmd.Start(); err != nil {
		http.Error(w, fmt.Sprintf("preview ffmpeg failed to start: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")

	buf := make([]byte, 64*1024)
	for {
		n, readErr := stdout.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				break
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if readErr != nil {
			break
		}
	}
	_ = cmd.Wait()
}

// previewArgs builds ffmpeg arguments that output MJPEG to stdout.
func previewArgs(config ffmpeg.Config) []string {
	args := []string{
		"-hide_banner",
		"-nostdin",
		"-loglevel", "error",
	}

	// Use the same input source as the main stream.
	switch config.Input.Kind {
	case ffmpeg.InputTestVideo:
		args = append(args,
			"-re",
			"-f", "lavfi",
			"-i", fmt.Sprintf("testsrc2=size=640x360:rate=15"),
		)
	default:
		backend := config.Input.Backend
		if backend == "" {
			backend = ffmpeg.PlatformBackend()
		}
		switch backend {
		case "avfoundation":
			device := config.Input.VideoDevice
			if config.Input.AudioDevice != "" {
				device = device + ":" + config.Input.AudioDevice
			} else {
				device = device + ":none"
			}
			args = append(args,
				"-f", "avfoundation",
				"-framerate", "15",
				"-video_size", "640x360",
				"-i", device,
			)
		case "dshow":
			device := "video=" + config.Input.VideoDevice
			args = append(args,
				"-f", "dshow",
				"-framerate", "15",
				"-video_size", "640x360",
				"-i", device,
			)
		case "v4l2":
			args = append(args,
				"-f", "v4l2",
				"-framerate", "15",
				"-video_size", "640x360",
				"-i", config.Input.VideoDevice,
			)
		case "decklink":
			args = append(args,
				"-f", "decklink",
				"-i", config.Input.VideoDevice,
			)
		default:
			args = append(args,
				"-f", backend,
				"-i", config.Input.VideoDevice,
			)
		}
	}

	// Output MJPEG to stdout for browser consumption.
	args = append(args,
		"-an",                    // no audio for preview
		"-vf", "scale=640:360",  // small preview
		"-r", "15",              // low framerate for preview
		"-q:v", "5",             // JPEG quality (2=best, 31=worst)
		"-f", "mjpeg",
		"-",
	)
	return args
}
