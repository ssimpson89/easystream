package preview

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"sync"

	"github.com/ssimpson89/easystream/internal/ffmpeg"
)

const boundary = "easystream-preview-frame"

var (
	jpegSOI = []byte{0xFF, 0xD8}
	jpegEOI = []byte{0xFF, 0xD9}
)

// Server streams a low-res MJPEG preview from the capture source.
// macOS avfoundation locks a capture device to one process, so when the
// main stream is running on a real capture (not the test pattern) the
// preview must release the device. Block() / Unblock() control that.
type Server struct {
	mu           sync.Mutex
	logger       *log.Logger
	config       ffmpeg.Config
	blocked      bool
	activeCancel context.CancelFunc // currently running preview, if any
}

func NewServer(logger *log.Logger) *Server {
	return &Server{logger: logger}
}

func (s *Server) UpdateConfig(config ffmpeg.Config) {
	s.mu.Lock()
	s.config = config
	s.mu.Unlock()
}

// Block prevents the preview from starting FFmpeg and cancels any in-flight
// preview stream so the main stream can claim the capture device.
func (s *Server) Block() {
	s.mu.Lock()
	s.blocked = true
	c := s.activeCancel
	s.activeCancel = nil
	s.mu.Unlock()
	if c != nil {
		c()
	}
}

// Unblock allows preview FFmpeg processes to start again.
func (s *Server) Unblock() {
	s.mu.Lock()
	s.blocked = false
	s.mu.Unlock()
}

// IsBlocked reports whether previews are currently disallowed.
func (s *Server) IsBlocked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blocked
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	config := s.config
	if s.blocked {
		s.mu.Unlock()
		http.Error(w, "preview paused while stream is live", http.StatusServiceUnavailable)
		return
	}
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Register this preview so Block() can interrupt it. If another preview
	// is already running, cancel that one first (we only allow one at a time).
	s.mu.Lock()
	if prev := s.activeCancel; prev != nil {
		prev()
	}
	s.activeCancel = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.activeCancel != nil {
			s.activeCancel = nil
		}
		s.mu.Unlock()
	}()

	binary := config.Binary
	if binary == "" {
		binary = "ffmpeg"
	}
	args := previewArgs(config)
	cmd := exec.CommandContext(ctx, binary, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		http.Error(w, fmt.Sprintf("preview ffmpeg failed to start: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")

	flusher, canFlush := w.(http.Flusher)

	// Write initial boundary to force chunked encoding.
	_, _ = fmt.Fprintf(w, "--%s\r\n", boundary)
	if canFlush {
		flusher.Flush()
	}

	buf := make([]byte, 128*1024)
	var accum []byte

	for {
		n, readErr := stdout.Read(buf)
		if n > 0 {
			accum = append(accum, buf[:n]...)
			for {
				frame, rest, ok := extractJPEG(accum)
				if !ok {
					break
				}
				accum = rest
				header := fmt.Sprintf("\r\n--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", boundary, len(frame))
				if _, err := io.WriteString(w, header); err != nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					return
				}
				if _, err := w.Write(frame); err != nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					return
				}
				if canFlush {
					flusher.Flush()
				}
			}
			if len(accum) > 1024*1024 {
				if idx := bytes.LastIndex(accum, jpegSOI); idx > 0 {
					accum = accum[idx:]
				} else {
					accum = accum[:0]
				}
			}
		}
		if readErr != nil {
			break
		}
	}
	waitErr := cmd.Wait()
	if stderrBuf.Len() > 0 || waitErr != nil {
		errMsg := stderrBuf.String()
		s.logger.Printf("preview: ffmpeg exited: err=%v stderr=%q", waitErr, errMsg)
	}
}

func extractJPEG(data []byte) (frame, rest []byte, ok bool) {
	soiIdx := bytes.Index(data, jpegSOI)
	if soiIdx < 0 {
		return nil, data, false
	}
	eoiIdx := bytes.Index(data[soiIdx+2:], jpegEOI)
	if eoiIdx < 0 {
		return nil, data, false
	}
	eoiEnd := soiIdx + 2 + eoiIdx + 2
	return data[soiIdx:eoiEnd], data[eoiEnd:], true
}

// previewArgs builds ffmpeg arguments for a low-res MJPEG preview.
// No framerate or resolution is forced on the capture device — FFmpeg
// auto-negotiates with the hardware. The output is scaled and rate-limited.
func previewArgs(config ffmpeg.Config) []string {
	args := []string{
		"-hide_banner",
		"-nostdin",
		"-loglevel", "error",
	}

	switch config.Input.Kind {
	case ffmpeg.InputTestVideo:
		args = append(args,
			"-re",
			"-f", "lavfi",
			"-i", "testsrc2=size=640x360:rate=10",
		)
	default:
		backend := config.Input.Backend
		if backend == "" {
			backend = ffmpeg.PlatformBackend()
		}
		device := config.Input.VideoDevice
		switch backend {
		case "avfoundation":
			if config.Input.AudioDevice != "" {
				device = device + ":" + config.Input.AudioDevice
			} else {
				device = device + ":none"
			}
			args = append(args, "-f", "avfoundation", "-i", device)
		case "dshow":
			args = append(args, "-f", "dshow", "-i", "video="+device)
		case "v4l2":
			args = append(args, "-f", "v4l2", "-i", device)
		case "decklink":
			args = append(args, "-f", "decklink", "-i", device)
		default:
			args = append(args, "-f", backend, "-i", device)
		}
	}

	// Output: scale down, low framerate, MJPEG to stdout.
	args = append(args,
		"-an",
		"-vf", "scale=640:360:force_original_aspect_ratio=decrease",
		"-r", "10",
		"-q:v", "5",
		"-f", "mjpeg",
		"pipe:1",
	)
	return args
}
