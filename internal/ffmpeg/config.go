package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"math"
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

// OutputMode selects where FFmpeg sends the encoded stream.
type OutputMode string

const (
	OutputRTMP OutputMode = "rtmp" // RTMP/RTMPS push to YouTube/etc
	OutputHLS  OutputMode = "hls"  // Write HLS segments to local dir
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
	HLSDir     string         `json:"hlsDir,omitempty"`
	Network    Network        `json:"network"`
	LogLevel   string         `json:"logLevel"`
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

// EffectiveEncoder returns the encoder to use, defaulting to libx264.
func (c Config) EffectiveEncoder() Encoder {
	if c.Encoder == "" {
		return EncoderX264
	}
	return c.Encoder
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
		"-fflags", "nobuffer",
		"-flags", "low_delay",
		"-progress", "pipe:1",
		"-stats_period", "1",
	}

	inputs := c.buildInputs()
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
		// Audio filter: compute per-second RMS level and print to stderr so
		// the supervisor can detect silent audio (stuck mic, wrong source).
		// astats with metadata=1:reset=1:length=1 emits a stat every 1s.
		// ametadata file output bypasses -loglevel warning suppression while
		// keeping stdout reserved for FFmpeg's machine-readable progress stream.
		"-af", "astats=metadata=1:reset=1:length=1,ametadata=print:key=lavfi.astats.Overall.RMS_level:file=/dev/stderr",
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
		return []string{
			"-c:v", "h264_videotoolbox",
			"-profile:v", "high",
			"-level:v", "4.1",
			"-allow_sw", "1",
			"-realtime", "1",
		}

	case EncoderNVENC:
		// NVIDIA NVENC. Uses -preset p4 (balanced speed/quality) and
		// CBR rate control. -rc cbr requires -b:v to be set (done by caller).
		return []string{
			"-c:v", "h264_nvenc",
			"-preset", "p4",
			"-profile:v", "high",
			"-level:v", "4.1",
			"-rc", "cbr",
			"-bf", "2",
			"-g", gop,
			"-keyint_min", gop,
		}

	case EncoderVAAPI:
		// VA-API (Linux Intel/AMD). Requires a DRM render device.
		// The video filter chain needs hwupload and format conversion.
		return []string{
			"-vaapi_device", "/dev/dri/renderD128",
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
		return []string{
			"-c:v", "libx264",
			"-preset", "veryfast",
			"-profile:v", "high",
			"-level:v", "4.1",
			"-keyint_min", gop,
			"-sc_threshold", "0",
			"-bf", "2",
			"-refs", "1",
			// Closed GOP: every keyframe is IDR and no frame references
			// across keyframe boundaries. Required by Cloudflare Stream;
			// preferred by YouTube/Twitch/etc.
			"-x264-params", "open-gop=0",
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
		device := ResolveAVFoundationDeviceIndex(c.Binary, c.Input.VideoDevice, c.Input.VideoDeviceName, "video")
		audio := ResolveAVFoundationDeviceIndex(c.Binary, c.Input.AudioDevice, c.Input.AudioDeviceName, "audio")
		fps := ProbeAVFoundationFramerate(c.Binary, device, c.Preset.FPS)
		if audio != "" {
			// Both video and audio in one avfoundation input.
			return inputBuild{
				args:     []string{"-f", "avfoundation", "-framerate", fps, "-i", device + ":" + audio},
				videoMap: "0:v", audioMap: "0:a",
			}
		}
		// Video only; mix in a silent audio track.
		return inputBuild{
			args: []string{
				"-f", "avfoundation", "-framerate", fps, "-i", device + ":none",
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

// ResolveAVFoundationDeviceIndex resolves a device name to its current
// AVFoundation index. If deviceName is empty or the probe fails, it falls
// back to fallbackIndex. This handles the AVFoundation problem where
// device indices shift between system boots or when USB devices are
// plugged/unplugged — persisting the name gives stable device selection.
func ResolveAVFoundationDeviceIndex(binary, fallbackIndex, deviceName, kind string) string {
	if deviceName == "" {
		return fallbackIndex
	}
	if binary == "" {
		binary = "ffmpeg"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, binary, "-hide_banner", "-f", "avfoundation", "-list_devices", "true", "-i", "").CombinedOutput()
	if idx, ok := chooseAVFoundationDeviceIndex(string(out), deviceName, kind); ok {
		return idx
	}
	return fallbackIndex
}

// chooseAVFoundationDeviceIndex scans FFmpeg's AVFoundation device list output
// for a device matching the given name in the correct section (video or audio).
// Returns the device index and true if found.
func chooseAVFoundationDeviceIndex(output, deviceName, kind string) (string, bool) {
	if deviceName == "" {
		return "", false
	}
	targetName := strings.TrimSpace(strings.ToLower(deviceName))
	inCorrectSection := false
	wantAudio := kind == "audio"

	for _, line := range strings.Split(output, "\n") {
		lower := strings.ToLower(line)
		// Detect section headers in FFmpeg's device listing.
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
				return m[1], true
			}
		}
	}
	return "", false
}
