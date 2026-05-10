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

// JPEG markers.
var (
	jpegSOI = []byte{0xFF, 0xD8}
	jpegEOI = []byte{0xFF, 0xD9}
)

// Server runs a lightweight FFmpeg process that converts the capture source
// to MJPEG and serves it as a multipart stream the browser can display
// in an <img> tag via the "multipart/x-mixed-replace" content type.
type Server struct {
	mu     sync.Mutex
	logger *log.Logger
	config ffmpeg.Config
}

// NewServer creates a preview server.
func NewServer(logger *log.Logger) *Server {
	return &Server{logger: logger}
}

// UpdateConfig stores a new capture config for the next preview request.
func (s *Server) UpdateConfig(config ffmpeg.Config) {
	s.mu.Lock()
	s.config = config
	s.mu.Unlock()
}

// ServeHTTP streams MJPEG from the current capture source, properly framed
// as multipart/x-mixed-replace so browsers can display it in an <img> tag.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	config := s.config
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

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
	// Capture stderr for debugging.
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		http.Error(w, fmt.Sprintf("preview ffmpeg failed to start: %v", err), http.StatusInternalServerError)
		return
	}
	s.logger.Printf("preview: started (pid %d)", cmd.Process.Pid)

	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")

	flusher, canFlush := w.(http.Flusher)

	// Write initial boundary to force the headers out with chunked encoding.
	_, _ = fmt.Fprintf(w, "--%s\r\n", boundary)
	if canFlush {
		flusher.Flush()
	}

	// Read the raw MJPEG stream in chunks and split into individual JPEG frames.
	// Each frame starts with SOI (0xFFD8) and ends with EOI (0xFFD9).
	buf := make([]byte, 128*1024)
	var accum []byte

	frameCount := 0
	for {
		n, readErr := stdout.Read(buf)
		if n > 0 {
			accum = append(accum, buf[:n]...)

			// Extract complete frames from the accumulator.
			for {
				frame, rest, ok := extractJPEG(accum)
				if !ok {
					break
				}
				accum = rest
				frameCount++

				// Write as multipart part.
				header := fmt.Sprintf("\r\n--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", boundary, len(frame))
				if _, err := io.WriteString(w, header); err != nil {
					// Client disconnected.
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					return
				}
				if _, err := w.Write(frame); err != nil {
					// Client disconnected.
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					return
				}
				if canFlush {
					flusher.Flush()
				}
			}

			// Prevent unbounded accumulation — keep only from last SOI.
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
	_ = cmd.Wait()
	if stderrBuf.Len() > 0 {
		s.logger.Printf("preview: ffmpeg stderr: %s", stderrBuf.String())
	}
}

// extractJPEG finds the first complete JPEG frame in data.
// Returns the frame bytes, remaining data, and whether a frame was found.
func extractJPEG(data []byte) (frame, rest []byte, ok bool) {
	// Find SOI.
	soiIdx := bytes.Index(data, jpegSOI)
	if soiIdx < 0 {
		return nil, data, false
	}
	// Find EOI after SOI.
	eoiIdx := bytes.Index(data[soiIdx+2:], jpegEOI)
	if eoiIdx < 0 {
		return nil, data, false
	}
	eoiEnd := soiIdx + 2 + eoiIdx + 2 // include the 2-byte EOI marker
	return data[soiIdx:eoiEnd], data[eoiEnd:], true
}

// previewArgs builds ffmpeg arguments that output MJPEG to stdout.
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
				"-framerate", "30",
				"-pixel_format", "yuyv422",
				"-i", device,
			)
		case "dshow":
			device := "video=" + config.Input.VideoDevice
			args = append(args,
				"-f", "dshow",
				"-i", device,
			)
		case "v4l2":
			args = append(args,
				"-f", "v4l2",
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

	args = append(args,
		"-an",
		"-vf", "scale=640:360",
		"-r", "10",
		"-q:v", "5",
		"-f", "mjpeg",
		"pipe:1",
	)
	return args
}
