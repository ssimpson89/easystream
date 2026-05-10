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

	inputs := c.buildInputs()
	args = append(args, inputs.args...)
	args = append(args, "-map", inputs.videoMap, "-map", inputs.audioMap)

	// Build video filter chain. SDI/DeckLink sources may be interlaced
	// so we auto-deinterlace with yadif, then scale + pad to the target
	// resolution preserving aspect ratio.
	//
	// scale/pad accept W:H (colon-separated) — NOT W x H. The pad filter
	// in particular doesn't recognise "1920x1080" as a dimension pair.
	vf := fmt.Sprintf(
		"yadif=deint=interlaced,scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black",
		c.Preset.Width, c.Preset.Height,
		c.Preset.Width, c.Preset.Height,
	)
	// yadif deint=interlaced is a no-op on progressive sources.

	// Encoder settings aligned with YouTube's H.264 recommendations:
	//   - High profile (CABAC, 8-bit 4:2:0)
	//   - 2 B-frames, 1 reference frame, progressive scan
	//   - CBR via -b:v == -maxrate, 2x bufsize for ~2s buffer
	//   - 2-second keyframe interval (GOP = FPS * 2)
	//   - Rec.709 color primaries / transfer / matrix for SDR
	//   - 128 kbps AAC stereo at 48 kHz
	//
	// No -tune zerolatency: it disables B-frames, which YouTube explicitly
	// recommends keeping (2 B-frames). Latency doesn't matter for broadcast
	// streaming — viewers always have a multi-second buffer.
	args = append(args,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-profile:v", "high",
		"-level:v", "4.1",
		"-vf", vf,
		"-b:v", c.Preset.VideoBitrate(),
		"-maxrate", c.Preset.VideoBitrate(),
		"-bufsize", c.Preset.BufferSize(),
		"-g", fmt.Sprintf("%d", c.Preset.GOP()),
		"-keyint_min", fmt.Sprintf("%d", c.Preset.GOP()),
		"-sc_threshold", "0",
		"-bf", "2",
		"-refs", "1",
		// Closed GOP: every keyframe is IDR and no frame references across
		// keyframe boundaries. x264 default but explicit is safer. Required
		// by Cloudflare Stream; preferred by YouTube/Twitch/etc.
		"-x264-params", "open-gop=0",
		"-pix_fmt", "yuv420p",
		"-r", fmt.Sprintf("%d", c.Preset.FPS),
		"-color_primaries", "bt709",
		"-color_trc", "bt709",
		"-colorspace", "bt709",
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

// inputBuild describes the constructed FFmpeg inputs and how to map them.
type inputBuild struct {
	args     []string // -f / -i / etc. for each input
	videoMap string   // e.g. "0:v" or "0:v:0"
	audioMap string   // e.g. "0:a" or "1:a"
}

const silentAudio = "anullsrc=channel_layout=stereo:sample_rate=48000"

// buildInputs constructs FFmpeg input flags. When the capture source has no
// audio (or audio isn't applicable), it adds a silent audio track as a
// second input. RTMP streams require an audio track; YouTube rejects video-
// only streams.
func (c Config) buildInputs() inputBuild {
	if c.Input.Kind == InputTestVideo {
		return inputBuild{
			args: []string{
				"-re",
				"-f", "lavfi",
				"-i", fmt.Sprintf("testsrc2=size=%s:rate=%d", c.Preset.Resolution(), c.Preset.FPS),
				"-f", "lavfi",
				"-i", "sine=frequency=1000:sample_rate=48000",
			},
			videoMap: "0:v",
			audioMap: "1:a",
		}
	}

	backend := defaultString(c.Input.Backend, PlatformBackend())
	if c.Input.Kind == InputSDI && backend != "decklink" {
		backend = "decklink"
	}

	switch backend {
	case "avfoundation":
		device := c.Input.VideoDevice
		if c.Input.AudioDevice != "" {
			// Both video and audio in one avfoundation input.
			return inputBuild{
				args:     []string{"-f", "avfoundation", "-i", device + ":" + c.Input.AudioDevice},
				videoMap: "0:v", audioMap: "0:a",
			}
		}
		// Video only; mix in a silent audio track.
		return inputBuild{
			args: []string{
				"-f", "avfoundation", "-i", device + ":none",
				"-f", "lavfi", "-i", silentAudio,
			},
			videoMap: "0:v", audioMap: "1:a",
		}

	case "dshow":
		device := "video=" + c.Input.VideoDevice
		if c.Input.AudioDevice != "" {
			return inputBuild{
				args:     []string{"-f", "dshow", "-i", device + ":audio=" + c.Input.AudioDevice},
				videoMap: "0:v", audioMap: "0:a",
			}
		}
		return inputBuild{
			args: []string{
				"-f", "dshow", "-i", device,
				"-f", "lavfi", "-i", silentAudio,
			},
			videoMap: "0:v", audioMap: "1:a",
		}

	case "v4l2":
		// v4l2 is video-only. Audio comes from ALSA (separate input) or silent.
		if c.Input.AudioDevice != "" {
			return inputBuild{
				args: []string{
					"-f", "v4l2", "-i", c.Input.VideoDevice,
					"-f", "alsa", "-i", c.Input.AudioDevice,
				},
				videoMap: "0:v", audioMap: "1:a",
			}
		}
		return inputBuild{
			args: []string{
				"-f", "v4l2", "-i", c.Input.VideoDevice,
				"-f", "lavfi", "-i", silentAudio,
			},
			videoMap: "0:v", audioMap: "1:a",
		}

	case "decklink":
		// SDI carries embedded audio in the same signal.
		return inputBuild{
			args:     []string{"-f", "decklink", "-audio_input", "embedded", "-i", c.Input.VideoDevice},
			videoMap: "0:v", audioMap: "0:a",
		}

	default:
		return inputBuild{
			args: []string{
				"-f", backend, "-i", c.Input.VideoDevice,
				"-f", "lavfi", "-i", silentAudio,
			},
			videoMap: "0:v", audioMap: "1:a",
		}
	}
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
