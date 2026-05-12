package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
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
	Kind            InputKind `json:"kind"`
	Backend         string    `json:"backend"`
	VideoDevice     string    `json:"videoDevice"`
	AudioDevice     string    `json:"audioDevice"`
	VideoDeviceName string    `json:"videoDeviceName,omitempty"`
	AudioDeviceName string    `json:"audioDeviceName,omitempty"`
	Format          string    `json:"format"`
}

// OutputMode selects the *primary* destination FFmpeg sends to.
// HLS is no longer one of the mutually-exclusive primaries — it is an
// independent toggle (Config.EnableHLS) that runs alongside any primary
// for local monitoring. The legacy "hls" value is accepted on input
// solely for backwards-compat migration; new code emits only "rtmp"
// (or future "srt").
type OutputMode string

const (
	OutputRTMP OutputMode = "rtmp" // RTMP/RTMPS push (YouTube, Twitch, etc.)
	OutputSRT  OutputMode = "srt"  // SRT push (Cloudflare Stream, custom receivers)
	// Legacy "hls" value is recognised on the load path (server.go,
	// handleConfigUpdate, configResponse) for migrating older configs.
	// New code should not emit it as a primary mode.
)

// Encoder selects which H.264 video encoder FFmpeg uses.
type Encoder string

const (
	EncoderX264         Encoder = "libx264"           // Software (CPU)
	EncoderVideoToolbox Encoder = "h264_videotoolbox" // macOS hardware (Apple Silicon / Intel)
	EncoderNVENC        Encoder = "h264_nvenc"        // NVIDIA GPU
	EncoderVAAPI        Encoder = "h264_vaapi"        // Linux Intel/AMD GPU
	EncoderQSV          Encoder = "h264_qsv"          // Intel QuickSync
)

// EncoderInfo describes a detected encoder for the UI.
type EncoderInfo struct {
	ID          Encoder `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Available   bool    `json:"available"`
}

// knownEncoders lists all encoders we know how to configure, in display order.
var knownEncoders = []EncoderInfo{
	{EncoderX264, "Software (x264)", "CPU-based, always available, widest compatibility", true},
	{EncoderVideoToolbox, "Apple VideoToolbox", "macOS hardware encoder (Apple Silicon / Intel)", false},
	{EncoderNVENC, "NVIDIA NVENC", "NVIDIA GPU hardware encoder", false},
	{EncoderVAAPI, "VA-API", "Linux Intel/AMD GPU hardware encoder", false},
	{EncoderQSV, "Intel QuickSync", "Intel GPU hardware encoder", false},
}

// Capabilities reports which optional FFmpeg features this build
// supports. Probed once at startup; the UI uses it to gate features
// (e.g. don't let the operator pick SRT if FFmpeg can't speak it).
type Capabilities struct {
	SRT bool `json:"srt"`
}

// DetectCapabilities runs `ffmpeg -protocols` and reports which
// optional protocols the binary was built with. Homebrew's default
// FFmpeg formula on macOS doesn't link against libsrt — without this
// check the operator only finds out when the stream fails with
// "Protocol not found" at the wrong moment.
func DetectCapabilities(binary string) Capabilities {
	if binary == "" {
		binary = "ffmpeg"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "-hide_banner", "-protocols").CombinedOutput()
	if err != nil {
		return Capabilities{}
	}
	// `ffmpeg -protocols` prints sections "Input:" and "Output:" with
	// one protocol per line. We need SRT in the Output section.
	caps := Capabilities{}
	inOutput := false
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "Output:" {
			inOutput = true
			continue
		}
		if trimmed == "Input:" {
			inOutput = false
			continue
		}
		if inOutput && trimmed == "srt" {
			caps.SRT = true
		}
	}
	return caps
}

// DetectEncoders probes ffmpeg for available hardware encoders.
func DetectEncoders(binary string) []EncoderInfo {
	if binary == "" {
		binary = "ffmpeg"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "-hide_banner", "-encoders").CombinedOutput()
	if err != nil {
		// Return list with only software available.
		result := make([]EncoderInfo, len(knownEncoders))
		copy(result, knownEncoders)
		return result
	}
	output := string(out)
	result := make([]EncoderInfo, len(knownEncoders))
	copy(result, knownEncoders)
	for i := range result {
		if result[i].ID == EncoderX264 {
			result[i].Available = true
			continue
		}
		// Look for the encoder name in the output.
		if strings.Contains(output, string(result[i].ID)) {
			result[i].Available = true
		}
	}
	return result
}

type Config struct {
	Binary     string         `json:"binary"`
	Input      Input          `json:"input"`
	Preset     quality.Preset `json:"preset"`
	Encoder    Encoder        `json:"encoder,omitempty"`
	OutputMode OutputMode     `json:"outputMode"`
	IngestURL  string         `json:"ingestUrl"`
	StreamName string         `json:"streamName"`
	// EnableHLS writes a local HLS playlist alongside the primary
	// destination. Independent of OutputMode — operators can have both
	// a YouTube broadcast and a local HLS playlist for monitoring.
	EnableHLS bool    `json:"enableHls"`
	HLSDir    string  `json:"hlsDir,omitempty"`
	Network   Network `json:"network"`
	LogLevel  string  `json:"logLevel"`
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
	if c.EnableHLS && strings.TrimSpace(c.HLSDir) == "" {
		return errors.New("HLS output directory is required when EnableHLS is true")
	}
	// At least one runnable output must exist.
	primary := c.primaryRunnable()
	if !primary && !c.EnableHLS {
		return errors.New("no output configured — set a destination URL+key or enable HLS")
	}
	if primary {
		switch c.OutputMode {
		case OutputRTMP, OutputSRT, "":
		default:
			return fmt.Errorf("unsupported primary output mode %q", c.OutputMode)
		}
	}
	return nil
}

// primaryRunnable reports whether the primary destination has enough
// config to actually run. Used by Args() to decide whether to append
// the primary output muxer, and by Validate() to permit HLS-only runs.
//
// RTMP requires both URL and stream key (key is a path segment).
// SRT requires only the URL — the user pastes the complete URL
// (including any streamid=... query param required by the receiver).
func (c Config) primaryRunnable() bool {
	url := strings.TrimSpace(c.IngestURL)
	if url == "" {
		return false
	}
	if c.OutputMode == OutputSRT {
		return true
	}
	return strings.TrimSpace(c.StreamName) != ""
}

// EffectiveEncoder returns the encoder to use, defaulting to libx264.
func (c Config) EffectiveEncoder() Encoder {
	if c.Encoder == "" {
		return EncoderX264
	}
	return c.Encoder
}

func (c Config) OutputURL() string {
	if c.OutputMode == OutputSRT {
		// SRT URL formats vary widely across receivers (Cloudflare
		// uses streamid=<id>:<password>, MediaMTX uses
		// streamid=publish:<name>, others use the raw key). The user
		// pastes the complete URL — we pass it through verbatim
		// without manipulating streamid or other params. Trying to
		// be smart here causes URL-encoding of colons (Cloudflare
		// rejects %3A) and other corruption.
		return c.IngestURL
	}
	// RTMP: append stream key as a path segment.
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
		"-fflags", "nobuffer",
		"-flags", "low_delay",
		"-progress", "pipe:1",
		"-stats_period", "1",
	}

	inputs, err := c.buildInputs()
	if err != nil {
		return nil, err
	}
	args = append(args, inputs.args...)
	args = append(args, "-map", inputs.videoMap, "-map", inputs.audioMap)

	encoder := c.EffectiveEncoder()
	gop := fmt.Sprintf("%d", c.Preset.GOP())

	// Build video filter chain. SDI/DeckLink sources may be interlaced
	// so we auto-deinterlace with yadif, then scale + pad to the target
	// resolution preserving aspect ratio.
	//
	// scale/pad accept W:H (colon-separated) — NOT W x H. The pad filter
	// in particular doesn't recognise "1920x1080" as a dimension pair.
	var vf string
	w, h := c.Preset.Width, c.Preset.Height
	if encoder == EncoderVideoToolbox {
		// VideoToolbox encoder handles progressive webcam/OBS input natively
		// so we skip yadif deinterlacing. scale_vt requires hwaccel frames
		// which AVFoundation doesn't produce, so we keep the CPU scaler
		// (cheap relative to encoding).
		vf = fmt.Sprintf(
			"scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black",
			w, h, w, h,
		)
	} else {
		// Software path: yadif deint=interlaced is a no-op on progressive
		// sources but catches real interlaced SDI/DeckLink input.
		vf = fmt.Sprintf(
			"yadif=deint=interlaced,scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black",
			w, h, w, h,
		)
	}

	// Encoder settings aligned with YouTube's H.264 recommendations:
	//   - High profile where supported (CABAC, 8-bit 4:2:0)
	//   - 2 B-frames, 1 reference frame, progressive scan (software)
	//   - CBR via -b:v == -maxrate, 2x bufsize for ~2s buffer
	//   - 2-second keyframe interval (GOP = FPS * 2)
	//   - Rec.709 color primaries / transfer / matrix for SDR
	//   - 128 kbps AAC stereo at 48 kHz
	args = append(args, "-vf", vf)
	args = append(args, c.encoderArgs(encoder, gop)...)
	args = append(args,
		"-b:v", c.Preset.VideoBitrate(),
		"-maxrate", c.Preset.VideoBitrate(),
		"-bufsize", c.Preset.BufferSize(),
		"-g", gop,
		"-r", fmt.Sprintf("%d", c.Preset.FPS),
		"-pix_fmt", "yuv420p",
		"-color_primaries", "bt709",
		"-color_trc", "bt709",
		"-colorspace", "bt709",
		// Audio filter chain:
		//   1. aresample=async=1000:first_pts=0 — explicit resampler
		//      handles mic→encoder rate mismatch (AVFoundation captures
		//      MacBook mics at 44.1k, we force 48k). Without an explicit
		//      filter FFmpeg's implicit resampler can desync over time,
		//      producing periodic audio dropouts (~1 cutout/second on
		//      affected systems). async=1000 corrects PTS drift up to
		//      1s smoothly; first_pts=0 zero-aligns the audio start.
		//   2. astats=metadata=1:reset=1:length=1 — emits per-second
		//      RMS stats consumed by the supervisor for silent-audio
		//      detection (stuck mic, wrong source).
		//   3. ametadata=print — writes the stats to /dev/stderr; this
		//      bypasses -loglevel warning suppression while keeping
		//      stdout reserved for FFmpeg's progress stream.
		"-af", "aresample=async=1000:first_pts=0,astats=metadata=1:reset=1:length=1,ametadata=print:key=lavfi.astats.Overall.RMS_level:file=/dev/stderr",
		"-c:a", "aac",
		"-b:a", c.Preset.AudioBitrate(),
		// 48 kHz over YouTube's recommended 44.1 kHz for stereo:
		//   - matches every other live destination's preferred rate
		//     (Cloudflare Stream, Twitch, MediaMTX, SRT receivers)
		//   - matches the broadcast/SDI standard, so HDMI/SDI feeds
		//     pass through without resampling
		//   - YouTube accepts 48 kHz fine — their 44.1 stereo line is
		//     a recommendation, not a requirement
		//   - aresample (above) handles 44.1→48 for consumer macOS
		//     mics smoothly via async=1000 PTS correction
		"-ar", "48000",
		"-ac", "2",
	)

	// Primary destination: appended only if runnable (URL + key set).
	// Allowing this to be skipped lets the operator do HLS-only runs.
	if c.primaryRunnable() {
		switch c.OutputMode {
		case OutputSRT:
			args = append(args,
				"-f", "mpegts",
				// resend_headers: re-emit PAT/PMT periodically so a
				// receiver joining mid-stream (or reconnecting after a
				// blip) can decode without waiting for a full keyframe
				// interval. initial_discontinuity primes the receiver.
				"-mpegts_flags", "+resend_headers+initial_discontinuity",
				"-flush_packets", "1",
				c.OutputURL(),
			)
		default: // OutputRTMP and "" both go here
			args = append(args, "-f", "flv")
			if c.Network.TCPKeepalive {
				args = append(args, "-tcp_keepalive", "1")
			}
			if c.Network.RWTimeout > 0 {
				args = append(args, "-rw_timeout", fmt.Sprintf("%d", c.Network.RWTimeout.Microseconds()))
			}
			args = append(args, c.OutputURL())
		}
	}

	// Optional HLS local-monitoring output. Re-maps the already-encoded
	// streams with -c copy so we don't pay a second encode. Independent
	// of the primary destination — runs alongside RTMP/SRT, or alone.
	if c.EnableHLS && strings.TrimSpace(c.HLSDir) != "" {
		args = append(args,
			"-map", inputs.videoMap, "-map", inputs.audioMap,
			"-c", "copy",
			"-f", "hls",
			"-hls_time", "6",
			"-hls_list_size", "10",
			"-hls_flags", "delete_segments+append_list+independent_segments",
			"-hls_segment_filename", fmt.Sprintf("%s/seg%%d.ts", c.HLSDir),
			fmt.Sprintf("%s/stream.m3u8", c.HLSDir),
		)
	}

	// Secondary output: low-res H.264 RTP for live browser preview.
	// Uses the same encoder family as the primary stream to avoid a
	// redundant CPU-based re-encode when hardware encoding is active.
	previewScale := "scale=640:360:force_original_aspect_ratio=decrease"
	args = append(args,
		"-map", inputs.videoMap, "-an",
		"-vf", previewScale,
		"-r", "15",
	)
	args = append(args, c.previewEncoderArgs(encoder)...)
	args = append(args,
		"-g", "30", "-keyint_min", "15",
		"-b:v", "800k",
		"-flush_packets", "1", "-muxdelay", "0", "-muxpreload", "0",
		"-payload_type", "96",
		"-f", "rtp",
		"rtp://127.0.0.1:52001?pkt_size=1200",
	)

	// Tertiary output: Opus RTP for live browser audio meter.
	args = append(args,
		"-map", inputs.audioMap, "-vn",
		"-c:a", "libopus", "-ar", "48000", "-ac", "2", "-b:a", "64k",
		"-flush_packets", "1", "-muxdelay", "0", "-muxpreload", "0",
		"-payload_type", "111",
		"-f", "rtp",
		"rtp://127.0.0.1:52002?pkt_size=1200",
	)

	return args, nil
}

// encoderArgs returns the encoder-specific flags for the primary stream output.
// Each hardware encoder needs different flags because they don't share x264's
// CLI options (no -preset veryfast, no -x264-params, etc.).
func (c Config) encoderArgs(encoder Encoder, gop string) []string {
	switch encoder {
	case EncoderVideoToolbox:
		// Apple VideoToolbox (macOS). Uses hardware H.264 on Apple Silicon
		// or Intel. Supports -profile and -level but not x264 presets.
		// -allow_sw 1 falls back to software if hardware is busy.
		//
		// -constant_bit_rate 1 forces true CBR. Without it, VideoToolbox
		// defaults to VBR — which drops the bitrate well below the
		// configured target for low-motion content (a static OBS scene
		// can come in at 1-2 Mbps even when -b:v says 10 Mbps). Cloudflare
		// Stream and other live receivers expect a constant bitrate at
		// or near the target and treat sub-threshold streams as "not
		// broadcasting".  Requires macOS 13+ / current VideoToolbox.
		return []string{
			"-c:v", "h264_videotoolbox",
			"-profile:v", "high",
			"-level:v", "4.1",
			"-allow_sw", "1",
			"-realtime", "1",
			"-constant_bit_rate", "1",
		}

	case EncoderNVENC:
		// NVIDIA NVENC. Industry-standard live CBR settings:
		//   -rc cbr           true constant bitrate (with -b:v from caller)
		//   -no-scenecut 1    no auto-inserted IDR on scene change — keep
		//                     keyframes on the configured GOP boundaries
		//                     so receivers can segment cleanly
		//   -forced-idr 1     every keyframe is an IDR (closed GOP)
		//   -strict_gop 1     no GOP-boundary slippage; receivers can
		//                     count frames to predict the next keyframe
		return []string{
			"-c:v", "h264_nvenc",
			"-preset", "p4",
			"-profile:v", "high",
			"-level:v", "4.1",
			"-rc", "cbr",
			"-no-scenecut", "1",
			"-forced-idr", "1",
			"-strict_gop", "1",
			"-bf", "2",
			"-g", gop,
			"-keyint_min", gop,
		}

	case EncoderVAAPI:
		// VA-API (Linux Intel/AMD). Requires a DRM render device.
		// Multi-GPU systems (iGPU + dGPU) expose renderD128 AND
		// renderD129 — pinning to 128 silently picks the wrong card.
		return []string{
			"-vaapi_device", pickVAAPIRenderNode(),
			"-c:v", "h264_vaapi",
			"-profile:v", "high",
			"-level", "41",
			"-bf", "2",
			"-g", gop,
			"-keyint_min", gop,
		}

	case EncoderQSV:
		// Intel QuickSync Video.
		return []string{
			"-c:v", "h264_qsv",
			"-preset", "veryfast",
			"-profile:v", "high",
			"-level", "41",
			"-bf", "2",
			"-g", gop,
			"-keyint_min", gop,
		}

	default: // libx264 (software)
		// No -tune zerolatency: it disables B-frames, which YouTube
		// explicitly recommends keeping (2 B-frames). Latency doesn't
		// matter for broadcast streaming.
		//
		// x264-params:
		//   nal-hrd=cbr   signal CBR in the bitstream HRD parameters
		//                 (Cloudflare and other strict receivers check
		//                 this and may reject streams without it)
		//   open-gop=0    closed GOP — every keyframe is IDR, no
		//                 frames reference across keyframe boundaries
		//   scenecut=0    no scene-change keyframes; keyframes only at
		//                 the configured GOP boundary, so receivers
		//                 segment predictably
		return []string{
			"-c:v", "libx264",
			"-preset", "veryfast",
			"-profile:v", "high",
			"-level:v", "4.1",
			"-keyint_min", gop,
			"-sc_threshold", "0",
			"-bf", "2",
			"-refs", "1",
			"-x264-params", "nal-hrd=cbr:open-gop=0:scenecut=0:keyint=" + gop + ":min-keyint=" + gop,
		}
	}
}

// previewEncoderArgs returns lightweight encoder flags for the secondary
// low-res RTP preview output. Hardware encoders use their native codec;
// software falls back to ultrafast/zerolatency for minimal CPU overhead.
func (c Config) previewEncoderArgs(encoder Encoder) []string {
	switch encoder {
	case EncoderVideoToolbox:
		return []string{
			"-c:v", "h264_videotoolbox",
			"-profile:v", "baseline",
			"-level:v", "3.1",
			"-allow_sw", "1",
			"-realtime", "1",
			"-pix_fmt", "yuv420p",
			"-bf", "0",
		}
	case EncoderNVENC:
		return []string{
			"-c:v", "h264_nvenc",
			"-preset", "p1",
			"-profile:v", "baseline",
			"-level:v", "3.1",
			"-rc", "cbr",
			"-bf", "0",
			"-pix_fmt", "yuv420p",
		}
	case EncoderVAAPI:
		return []string{
			"-c:v", "h264_vaapi",
			"-profile:v", "constrained_baseline",
			"-level", "31",
			"-bf", "0",
		}
	case EncoderQSV:
		return []string{
			"-c:v", "h264_qsv",
			"-preset", "veryfast",
			"-profile:v", "baseline",
			"-level", "31",
			"-bf", "0",
			"-pix_fmt", "yuv420p",
		}
	default:
		return []string{
			"-c:v", "libx264",
			"-preset", "ultrafast",
			"-tune", "zerolatency",
			"-profile:v", "baseline",
			"-level:v", "3.1",
			"-pix_fmt", "yuv420p",
			"-bf", "0",
		}
	}
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
//
// Fail-closed: for AVFoundation, if the persisted VideoDeviceName cannot
// be located in the current device listing (camera unplugged, renamed,
// AVFoundation indexes shifted), returns ErrDeviceNotFound rather than
// silently spawning FFmpeg against whichever device is at the stale
// persisted index. Picking the wrong source on Sunday morning is a
// worse failure than refusing to start with a clear error.
func (c Config) buildInputs() (inputBuild, error) {
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
		}, nil
	}

	backend := defaultString(c.Input.Backend, PlatformBackend())
	if c.Input.Kind == InputSDI && backend != "decklink" {
		backend = "decklink"
	}

	switch backend {
	case "avfoundation":
		// Video device: require strict name resolution. If the user
		// picked a device whose name is persisted, that name MUST
		// match something in the current listing.
		device := c.Input.VideoDevice
		if c.Input.VideoDeviceName != "" {
			resolved, err := ResolveAVFoundationDeviceIndexStrict(c.Binary, c.Input.VideoDeviceName, "video", c.Input.VideoDevice)
			if err != nil {
				return inputBuild{}, fmt.Errorf("video source: %w", err)
			}
			device = resolved
		}
		// Audio device: same strictness when a name is persisted. An
		// empty audio device is legitimate (silent audio fallback).
		audio := c.Input.AudioDevice
		if c.Input.AudioDeviceName != "" {
			resolved, err := ResolveAVFoundationDeviceIndexStrict(c.Binary, c.Input.AudioDeviceName, "audio", c.Input.AudioDevice)
			if err != nil {
				return inputBuild{}, fmt.Errorf("audio source: %w", err)
			}
			audio = resolved
		}
		fps := ProbeAVFoundationFramerate(c.Binary, device, c.Preset.FPS)
		if audio != "" {
			return inputBuild{
				args:     []string{"-f", "avfoundation", "-framerate", fps, "-i", device + ":" + audio},
				videoMap: "0:v", audioMap: "0:a",
			}, nil
		}
		return inputBuild{
			args: []string{
				"-f", "avfoundation", "-framerate", fps, "-i", device + ":none",
				"-f", "lavfi", "-i", silentAudio,
			},
			videoMap: "0:v", audioMap: "1:a",
		}, nil

	case "dshow":
		device := "video=" + c.Input.VideoDevice
		if c.Input.AudioDevice != "" {
			return inputBuild{
				args:     []string{"-f", "dshow", "-i", device + ":audio=" + c.Input.AudioDevice},
				videoMap: "0:v", audioMap: "0:a",
			}, nil
		}
		return inputBuild{
			args: []string{
				"-f", "dshow", "-i", device,
				"-f", "lavfi", "-i", silentAudio,
			},
			videoMap: "0:v", audioMap: "1:a",
		}, nil

	case "v4l2":
		// Resolve the v4l2 device strictly by its kernel-reported name
		// (/sys/class/video4linux/videoN/name). Symmetrical to the
		// AVFoundation strict path: a missing/renamed device aborts
		// the start rather than silently going live on whatever sits
		// at the stale /dev/videoN path.
		device := c.Input.VideoDevice
		if c.Input.VideoDeviceName != "" {
			resolved, err := ResolveV4L2DevicePathStrict(c.Input.VideoDeviceName, c.Input.VideoDevice)
			if err != nil {
				return inputBuild{}, fmt.Errorf("video source: %w", err)
			}
			device = resolved
		}
		if c.Input.AudioDevice != "" {
			return inputBuild{
				args: []string{
					"-f", "v4l2", "-i", device,
					"-f", "alsa", "-i", c.Input.AudioDevice,
				},
				videoMap: "0:v", audioMap: "1:a",
			}, nil
		}
		return inputBuild{
			args: []string{
				"-f", "v4l2", "-i", device,
				"-f", "lavfi", "-i", silentAudio,
			},
			videoMap: "0:v", audioMap: "1:a",
		}, nil

	case "decklink":
		// DeckLink addresses by name (BlackMagic appends "(2)", "(3)"
		// to disambiguate identical hardware). When a card is removed,
		// the remaining ones get re-numbered — verify the saved name
		// is still in `ffmpeg -list_devices` output before spawning,
		// otherwise the operator sees a cryptic FFmpeg error.
		device := c.Input.VideoDevice
		if err := verifyDeckLinkDevicePresent(c.Binary, device); err != nil {
			return inputBuild{}, fmt.Errorf("video source: %w", err)
		}
		return inputBuild{
			args:     []string{"-f", "decklink", "-audio_input", "embedded", "-i", device},
			videoMap: "0:v", audioMap: "0:a",
		}, nil

	default:
		return inputBuild{
			args: []string{
				"-f", backend, "-i", c.Input.VideoDevice,
				"-f", "lavfi", "-i", silentAudio,
			},
			videoMap: "0:v", audioMap: "1:a",
		}, nil
	}
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// ProbeAVFoundationFramerate queries the given AVFoundation video device index
// for its supported framerates, and returns the one closest to targetFPS. If
// the probe fails or cannot parse the output, it defaults to "30".
func ProbeAVFoundationFramerate(binary, deviceIndex string, targetFPS int) string {
	if binary == "" {
		binary = "ffmpeg"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, binary, "-hide_banner", "-f", "avfoundation", "-i", deviceIndex, "-vframes", "1", "-f", "null", "-").CombinedOutput()
	if fps, ok := chooseAVFoundationFramerate(string(out), targetFPS); ok {
		return fps
	}
	return "30"
}

var avfoundationModeRE = regexp.MustCompile(`@\[([0-9.]+)`)

// avfoundationDeviceRE matches AVFoundation device listing lines like "[0] FaceTime HD Camera".
var avfoundationDeviceRE = regexp.MustCompile(`\[(\d+)\]\s+(.+)`)

func chooseAVFoundationFramerate(output string, targetFPS int) (string, bool) {
	if targetFPS <= 0 {
		targetFPS = 30
	}
	var bestFPS string
	bestDiff := 1000.0

	for _, matches := range avfoundationModeRE.FindAllStringSubmatch(output, -1) {
		if len(matches) <= 1 {
			continue
		}
		if f, err := strconv.ParseFloat(matches[1], 64); err == nil {
			diff := math.Abs(f - float64(targetFPS))
			if diff < bestDiff {
				bestDiff = diff
				bestFPS = strconv.FormatFloat(f, 'f', -1, 64)
			}
		}
	}
	if bestFPS != "" {
		return bestFPS, true
	}
	return "", false
}

// ErrDeviceNotFound is returned when an AVFoundation device cannot be
// resolved by name. The configured source's index has likely shifted
// (USB replug, reboot) AND the persisted name no longer matches anything
// FFmpeg is reporting. Critically, this is returned INSTEAD of silently
// using the stale index — picking the wrong device on Sunday morning
// is a worse failure than refusing to start.
var ErrDeviceNotFound = errors.New("configured AVFoundation device not found")

// ResolveAVFoundationDeviceIndex resolves a device name to its current
// AVFoundation index. Best-effort variant used for UI/listing where
// "almost right" is acceptable. For start-time resolution, callers
// must use ResolveAVFoundationDeviceIndexStrict.
//
// If deviceName is empty or the probe fails, it falls back to
// fallbackIndex. This handles AVFoundation index drift between boots.
func ResolveAVFoundationDeviceIndex(binary, fallbackIndex, deviceName, kind string) string {
	if deviceName == "" {
		return fallbackIndex
	}
	idx, err := resolveAVFoundationDeviceIndex(binary, deviceName, kind, fallbackIndex)
	if err != nil {
		return fallbackIndex
	}
	return idx
}

// ResolveAVFoundationDeviceIndexStrict is fail-closed: it returns
// ErrDeviceNotFound when the named device isn't present in FFmpeg's
// device listing. Callers about to spawn FFmpeg must use this variant
// so a missing/renamed device aborts the start instead of silently
// picking whatever sits at the stale persisted index.
//
// fallbackIndex is used only as a tie-breaker when multiple devices
// share the same name (rare but possible: two HDMI capture cards both
// named "USB Capture HDMI"). The candidate matching fallbackIndex wins;
// otherwise ambiguity is an error so we don't pick the wrong twin.
func ResolveAVFoundationDeviceIndexStrict(binary, deviceName, kind, fallbackIndex string) (string, error) {
	if deviceName == "" {
		return "", fmt.Errorf("%w: empty device name", ErrDeviceNotFound)
	}
	return resolveAVFoundationDeviceIndex(binary, deviceName, kind, fallbackIndex)
}

func resolveAVFoundationDeviceIndex(binary, deviceName, kind, fallbackIndex string) (string, error) {
	if binary == "" {
		binary = "ffmpeg"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, binary, "-hide_banner", "-f", "avfoundation", "-list_devices", "true", "-i", "").CombinedOutput()
	matches := matchAVFoundationDevices(string(out), deviceName, kind)
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w: %q not in current AVFoundation %s list", ErrDeviceNotFound, deviceName, kind)
	case 1:
		return matches[0], nil
	default:
		// Multiple devices share this name. Prefer the one whose index
		// matches the saved fallbackIndex — that's the operator's most
		// recent successful pick. If no match, fail rather than guess.
		for _, m := range matches {
			if m == fallbackIndex {
				return m, nil
			}
		}
		return "", fmt.Errorf("%w: %q is ambiguous (%d devices share this name, none at saved index %q)",
			ErrDeviceNotFound, deviceName, len(matches), fallbackIndex)
	}
}

// verifyDeckLinkDevicePresent runs FFmpeg's decklink listing and
// returns nil if the named device is present. Used as a preflight in
// buildInputs so a missing DeckLink (one of three was unplugged and
// the rest got renumbered) surfaces a clean ErrDeviceNotFound instead
// of a cryptic FFmpeg failure once we spawn the encoder.
func verifyDeckLinkDevicePresent(binary, deviceName string) error {
	if deviceName == "" {
		return fmt.Errorf("%w: empty DeckLink device name", ErrDeviceNotFound)
	}
	if binary == "" {
		binary = "ffmpeg"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, binary, "-hide_banner", "-f", "decklink", "-list_devices", "1", "-i", "dummy").CombinedOutput()
	// FFmpeg's decklink listing format: [decklink @ 0x...] 'DeckLink Mini Recorder HD'
	target := strings.TrimSpace(strings.ToLower(deviceName))
	for _, line := range strings.Split(string(out), "\n") {
		// Single-quoted name extraction; case-insensitive equality.
		if i := strings.Index(line, "'"); i >= 0 {
			if j := strings.Index(line[i+1:], "'"); j >= 0 {
				name := strings.TrimSpace(strings.ToLower(line[i+1 : i+1+j]))
				if name == target {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("%w: DeckLink %q not in current device list", ErrDeviceNotFound, deviceName)
}

// pickVAAPIRenderNode returns the first readable /dev/dri/renderD12X
// node. Multi-GPU machines have several; rootless containers may have
// none. Falls back to renderD128 (the kernel's first allocation) so
// existing configs aren't disturbed.
func pickVAAPIRenderNode() string {
	for i := 128; i < 144; i++ {
		path := fmt.Sprintf("/dev/dri/renderD%d", i)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return "/dev/dri/renderD128"
}

// ResolveV4L2DevicePathStrict resolves a saved v4l2 device-name to its
// CURRENT /dev/videoN path by reading /sys/class/video4linux/. This
// closes the same wrong-source bug class on Linux as the AVFoundation
// strict resolver does on macOS: when USB renumeration shifts what
// /dev/video0 points at, we refuse to start FFmpeg on the new device.
//
// fallbackPath is used as a tie-breaker when multiple v4l2 nodes share
// the same name (rare — capture cards expose sub-device nodes; the
// device scanner filters those out, but a duplicate-name dual-camera
// case can still happen). If a candidate's path matches the saved
// fallbackPath we prefer it; otherwise ambiguity is an error.
func ResolveV4L2DevicePathStrict(deviceName, fallbackPath string) (string, error) {
	if deviceName == "" {
		return "", fmt.Errorf("%w: empty device name", ErrDeviceNotFound)
	}
	entries, err := os.ReadDir("/sys/class/video4linux")
	if err != nil {
		// /sys not present (non-Linux, stripped container) — let the
		// caller use the persisted path as-is.
		return fallbackPath, nil
	}
	type cand struct{ path string }
	var matches []cand
	for _, e := range entries {
		nm := e.Name()
		if !strings.HasPrefix(nm, "video") {
			continue
		}
		raw, err := os.ReadFile("/sys/class/video4linux/" + nm + "/name")
		if err != nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(string(raw)), deviceName) {
			matches = append(matches, cand{path: "/dev/" + nm})
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w: %q not under /sys/class/video4linux", ErrDeviceNotFound, deviceName)
	case 1:
		return matches[0].path, nil
	default:
		for _, m := range matches {
			if m.path == fallbackPath {
				return m.path, nil
			}
		}
		return "", fmt.Errorf("%w: %q ambiguous (%d matches, none at saved path %q)",
			ErrDeviceNotFound, deviceName, len(matches), fallbackPath)
	}
}

// matchAVFoundationDevices returns every index whose device name matches
// (case-insensitive, trimmed) in the requested kind's section. Returning
// a slice — not the first hit — lets the strict resolver detect
// duplicates and avoid the "wrong twin" bug.
func matchAVFoundationDevices(output, deviceName, kind string) []string {
	if deviceName == "" {
		return nil
	}
	targetName := strings.TrimSpace(strings.ToLower(deviceName))
	inCorrectSection := false
	wantAudio := kind == "audio"
	var out []string

	for _, line := range strings.Split(output, "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "avfoundation video devices") {
			inCorrectSection = !wantAudio
			continue
		}
		if strings.Contains(lower, "avfoundation audio devices") {
			inCorrectSection = wantAudio
			continue
		}
		if !inCorrectSection {
			continue
		}
		if m := avfoundationDeviceRE.FindStringSubmatch(line); len(m) == 3 {
			name := strings.TrimSpace(strings.ToLower(m[2]))
			if name == targetName {
				out = append(out, m[1])
			}
		}
	}
	return out
}

// chooseAVFoundationDeviceIndex is the legacy best-effort wrapper kept
// for callers that don't need strict semantics (currently only tests).
func chooseAVFoundationDeviceIndex(output, deviceName, kind string) (string, bool) {
	matches := matchAVFoundationDevices(output, deviceName, kind)
	if len(matches) == 0 {
		return "", false
	}
	return matches[0], true
}
