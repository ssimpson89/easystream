package ffmpeg

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/ssimpson89/easystream/internal/quality"
)

// PlatformBackend returns the default capture backend for the current OS.
func PlatformBackend() string {
	switch runtime.GOOS {
	case "darwin":
		return "avfoundation"
	case "windows":
		return "dshow"
	default:
		return "v4l2"
	}
}

type InputKind string

const (
	InputWebcam    InputKind = "webcam"
	InputHDMI      InputKind = "hdmi"
	InputSDI       InputKind = "sdi"
	InputTestVideo InputKind = "test-video"
)

type Input struct {
	Kind        InputKind `json:"kind"`
	Backend     string    `json:"backend"`
	VideoDevice string    `json:"videoDevice"`
	AudioDevice string    `json:"audioDevice"`
	Format      string    `json:"format"`
}

// OutputMode selects where FFmpeg sends the encoded stream.
type OutputMode string

const (
	OutputRTMP OutputMode = "rtmp" // RTMP/RTMPS push to YouTube/etc
	OutputHLS  OutputMode = "hls"  // Write HLS segments to local dir
)

type Config struct {
	Binary       string         `json:"binary"`
	Input        Input          `json:"input"`
	Preset       quality.Preset `json:"preset"`
	OutputMode   OutputMode     `json:"outputMode"`
	IngestURL    string         `json:"ingestUrl"`
	StreamName   string         `json:"streamName"`
	HLSDir       string         `json:"hlsDir,omitempty"`
	Network      Network        `json:"network"`
	LogLevel     string         `json:"logLevel"`
	ProcessTitle string         `json:"processTitle"`
}

type Network struct {
	RWTimeout    time.Duration `json:"rwTimeout"`
	TCPKeepalive bool          `json:"tcpKeepalive"`
}

func DefaultConfig() Config {
	return Config{
		Binary: "ffmpeg",
		Input: Input{
			Kind:    InputTestVideo,
			Backend: "lavfi",
			Format:  "testsrc2",
		},
		Preset:     quality.Default(),
		IngestURL:  "rtmps://a.rtmps.youtube.com/live2",
		StreamName: "",
		Network: Network{
			RWTimeout:    15 * time.Second,
			TCPKeepalive: true,
		},
		LogLevel: "warning",
	}
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Binary) == "" {
		return errors.New("ffmpeg binary is required")
	}
	if c.Preset.ID == "" {
		return errors.New("quality preset is required")
	}
	if c.Input.Kind != InputTestVideo && strings.TrimSpace(c.Input.VideoDevice) == "" {
		return errors.New("video device is required")
	}
	switch c.OutputMode {
	case OutputHLS:
		if strings.TrimSpace(c.HLSDir) == "" {
			return errors.New("HLS output directory is required")
		}
	default: // rtmp
		if strings.TrimSpace(c.IngestURL) == "" {
			return errors.New("ingest URL is required")
		}
	}
	return nil
}

func (c Config) OutputURL() string {
	base := strings.TrimRight(c.IngestURL, "/")
	name := strings.TrimLeft(c.StreamName, "/")
	if name == "" {
		return base
	}
	return base + "/" + name
}

func (c Config) Args() ([]string, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}

	args := []string{
		"-hide_banner",
		"-nostdin",
		"-loglevel", defaultString(c.LogLevel, "warning"),
		"-progress", "pipe:1",
		"-stats_period", "1",
	}

	args = append(args, c.inputArgs()...)
	args = append(args,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-tune", "zerolatency",
		"-b:v", c.Preset.VideoBitrate(),
		"-maxrate", c.Preset.VideoBitrate(),
		"-bufsize", c.Preset.BufferSize(),
		"-g", fmt.Sprintf("%d", c.Preset.GOP()),
		"-keyint_min", fmt.Sprintf("%d", c.Preset.GOP()),
		"-sc_threshold", "0",
		"-pix_fmt", "yuv420p",
		"-r", fmt.Sprintf("%d", c.Preset.FPS),
		"-s", c.Preset.Resolution(),
		"-c:a", "aac",
		"-b:a", c.Preset.AudioBitrate(),
		"-ar", "48000",
		"-ac", "2",
	)

	switch c.OutputMode {
	case OutputHLS:
		args = append(args,
			"-f", "hls",
			"-hls_time", "6",
			"-hls_list_size", "10",
			"-hls_flags", "delete_segments+append_list+independent_segments",
			"-hls_segment_filename", fmt.Sprintf("%s/seg%%d.ts", c.HLSDir),
			fmt.Sprintf("%s/stream.m3u8", c.HLSDir),
		)
	default: // rtmp
		args = append(args, "-f", "flv")
		if c.Network.TCPKeepalive {
			args = append(args, "-tcp_keepalive", "1")
		}
		if c.Network.RWTimeout > 0 {
			args = append(args, "-rw_timeout", fmt.Sprintf("%d", c.Network.RWTimeout.Microseconds()))
		}
		args = append(args, c.OutputURL())
	}
	return args, nil
}

func (c Config) inputArgs() []string {
	switch c.Input.Kind {
	case InputTestVideo:
		return []string{
			"-re",
			"-f", "lavfi",
			"-i", fmt.Sprintf("testsrc2=size=%s:rate=%d", c.Preset.Resolution(), c.Preset.FPS),
			"-f", "lavfi",
			"-i", "sine=frequency=1000:sample_rate=48000",
			"-shortest",
		}
	case InputWebcam, InputHDMI:
		return captureArgs(c.Input, c.Preset)
	case InputSDI:
		if c.Input.Backend == "" {
			input := c.Input
			input.Backend = "decklink"
			return captureArgs(input, c.Preset)
		}
		return captureArgs(c.Input, c.Preset)
	default:
		return captureArgs(c.Input, c.Preset)
	}
}

// captureArgs builds FFmpeg input arguments for a capture device.
// No framerate or resolution is forced on the device — FFmpeg auto-negotiates
// with the hardware. The output encoding flags (-r, -s) handle conversion.
func captureArgs(input Input, preset quality.Preset) []string {
	backend := defaultString(input.Backend, "avfoundation")
	switch backend {
	case "avfoundation":
		device := input.VideoDevice
		if input.AudioDevice != "" {
			device = device + ":" + input.AudioDevice
		}
		return []string{"-f", "avfoundation", "-i", device}
	case "dshow":
		device := "video=" + input.VideoDevice
		if input.AudioDevice != "" {
			device = device + ":audio=" + input.AudioDevice
		}
		return []string{"-f", "dshow", "-i", device}
	case "v4l2":
		return []string{"-f", "v4l2", "-i", input.VideoDevice}
	case "decklink":
		return []string{"-f", "decklink", "-i", input.VideoDevice}
	default:
		return []string{"-f", backend, "-i", input.VideoDevice}
	}
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
