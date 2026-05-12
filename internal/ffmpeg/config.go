package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"math"
	neturl "net/url"
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
	// InputNetwork ingests from a URL. The scheme determines the
	// FFmpeg transport: rtsp://, rtsps:// (IP cameras), srt://
	// (pull from another encoder), udp:// (multicast MPEG-TS),
	// http:// or https:// (HLS, MPEG-TS over HTTP). The URL goes
	// in Input.URL.
	InputNetwork InputKind = "network"
)

type Input struct {
	Kind            InputKind `json:"kind"`
	Backend         string    `json:"backend"`
	VideoDevice     string    `json:"videoDevice"`
	AudioDevice     string    `json:"audioDevice"`
	VideoDeviceName string    `json:"videoDeviceName,omitempty"`
	AudioDeviceName string    `json:"audioDeviceName,omitempty"`
	Format          string    `json:"format"`
	// URL is used when Kind == InputNetwork. Carries the full
	// rtsp/srt/udp/http URL the operator pasted, including any
	// query parameters, credentials, or stream names.
	URL string `json:"url,omitempty"`
	// NoAudio disables FFmpeg's attempt to map an audio stream
	// from the source. Useful for network sources (HDMI capture
	// over RTSP without embedded audio, video-only DASH feeds)
	// where the source has no audio track; without this flag
	// FFmpeg errors at startup. When set, a silent stereo track
	// is substituted instead.
	NoAudio bool `json:"noAudio,omitempty"`
	// SourceIsHDR signals that the input carries HDR transfer
	// characteristics (PQ/HLG, BT.2020 primaries). When set, the
	// video filter graph prepends a tone-map chain that converts
	// to SDR Rec.709 before scaling. Without it, the encoder
	// outputs SDR-tagged frames with HDR pixel data — colors
	// clip ugly on YouTube and most playback paths. Opt-in
	// because the tone-map chain costs CPU and is wrong for
	// SDR sources (where it'd compress contrast unnecessarily).
	SourceIsHDR bool `json:"sourceIsHdr,omitempty"`
}

// networkInputSchemes is the allowlist of URL schemes we accept for
// InputNetwork sources. file://, pipe:, concat:, and similar are
// deliberately excluded to keep ingest a strictly network operation.
var networkInputSchemes = map[string]bool{
	"rtsp":  true,
	"rtsps": true,
	"srt":   true,
	"udp":   true,
	"rtp":   true,
	"http":  true,
	"https": true,
}

// RedactedCredentialSentinel is the placeholder substituted for URL
// credentials when an Input.URL is serialised for API responses. The
// configUpdate handler treats incoming URLs containing this sentinel
// as "operator didn't change credentials" and preserves the stored
// value, so the round-trip never exposes secrets to the UI.
const RedactedCredentialSentinel = "REDACTED"

// secretQueryParams names URL query parameters whose values are
// always sensitive. Their values are scrubbed alongside any
// userinfo (user:pass@) component when an Input.URL is serialised.
var secretQueryParams = map[string]bool{
	"passphrase": true,
	"password":   true,
	"token":      true,
	"key":        true,
	"secret":     true,
}

// RedactURLCredentials returns u with any embedded credentials
// replaced by RedactedCredentialSentinel. Specifically:
//   - userinfo (rtsp://user:pass@host/path → rtsp://REDACTED:REDACTED@host/path)
//   - secret query parameters (srt://host:port?passphrase=X → ?passphrase=REDACTED)
//
// Used to scrub network-input URLs before sending them to UI clients
// or writing to logs, so RTSP camera passwords and SRT passphrases
// don't leak to anyone watching the dashboard SSE stream.
func RedactURLCredentials(u string) string {
	if u == "" {
		return u
	}
	parsed, err := neturl.Parse(u)
	if err != nil {
		// Couldn't parse — fall back to substring stripping for
		// the obvious user:pass@ form.
		return redactRawURL(u)
	}
	if parsed.User != nil {
		parsed.User = neturl.UserPassword(RedactedCredentialSentinel, RedactedCredentialSentinel)
	}
	if parsed.RawQuery != "" {
		q := parsed.Query()
		changed := false
		for k := range q {
			if secretQueryParams[strings.ToLower(k)] {
				q.Set(k, RedactedCredentialSentinel)
				changed = true
			}
		}
		if changed {
			parsed.RawQuery = q.Encode()
		}
	}
	return parsed.String()
}

// redactRawURL strips userinfo from a URL we couldn't fully parse.
// Used only as a fallback; the parsed path handles everything we
// expect to see.
func redactRawURL(u string) string {
	schemeIdx := strings.Index(u, "://")
	if schemeIdx < 0 {
		return u
	}
	rest := u[schemeIdx+3:]
	atIdx := strings.Index(rest, "@")
	slashIdx := strings.Index(rest, "/")
	if atIdx < 0 || (slashIdx >= 0 && atIdx > slashIdx) {
		return u
	}
	return u[:schemeIdx+3] + RedactedCredentialSentinel + ":" + RedactedCredentialSentinel + rest[atIdx:]
}

// urlInLogRegex matches URL-shaped substrings inside a free-form
// log line. Matches the schemes we accept for ingest/egress
// (RTMP/SRT/RTSP/HTTP/UDP/RTP), stopping at whitespace or the
// trailing punctuation FFmpeg uses around URLs in error messages.
var urlInLogRegex = regexp.MustCompile(`(?i)(rtmp|rtmps|rtsp|rtsps|srt|http|https|udp|rtp)://[^\s'"]+`)

// RedactURLsInLog scans a free-form log line for URLs and runs each
// one through RedactURLCredentials. FFmpeg routinely echoes the full
// ingest/destination URL (with userinfo or query-string secrets) in
// error messages and connect logs. Without this, those secrets land
// in /status.lastLogLine, the logger output, and any SSE consumer.
func RedactURLsInLog(line string) string {
	return urlInLogRegex.ReplaceAllStringFunc(line, func(match string) string {
		// The regex may grab a trailing ',', ')', or '.' that's
		// punctuation from the surrounding log message, not part of
		// the URL. Strip those before redacting and reattach after.
		trailing := ""
		for len(match) > 0 {
			last := match[len(match)-1]
			if last == ',' || last == '.' || last == ')' || last == ']' || last == ';' || last == ':' {
				trailing = string(last) + trailing
				match = match[:len(match)-1]
				continue
			}
			break
		}
		return RedactURLCredentials(match) + trailing
	})
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
	OutputSRT  OutputMode = "srt"  // SRT push
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

// keyframeIntervalSec is the 2-second cadence used for primary-output
// GOP boundaries (preset.GOP returns FPS*2) and for the VideoToolbox
// -force_key_frames expression. Keeping these derived from the same
// constant prevents the encoder's GOP and VT's keyframe-forcing
// expression from drifting if we ever pick a different live cadence.
const keyframeIntervalSec = 2

// previewFPS is the framerate for the secondary low-res preview leg.
// 15 fps is plenty for a thumbnail preview and lets us decimate the
// source before scaling — halving the scaler workload on a 30 fps
// preset, more on higher-FPS sources.
const previewFPS = 15

// EncoderInfo describes a detected encoder for the UI.
type EncoderInfo struct {
	ID          Encoder `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Available   bool    `json:"available"`
}

// knownEncoders lists all encoders we know how to configure, in display order.
var knownEncoders = []EncoderInfo{
	{EncoderX264, "Software (x264)", "CPU-based, true CBR, widest compatibility. Required for SRT destinations.", true},
	{EncoderVideoToolbox, "Apple VideoToolbox", "macOS hardware encoder. Soft CBR — not used for SRT, which requires true CBR.", false},
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
	switch c.Input.Kind {
	case InputTestVideo:
		// no device required
	case InputNetwork:
		url := strings.TrimSpace(c.Input.URL)
		if url == "" {
			return errors.New("network input URL is required")
		}
		i := strings.Index(url, "://")
		if i <= 0 {
			return fmt.Errorf("network input URL must include a scheme like rtsp:// or srt://, got %q", url)
		}
		scheme := strings.ToLower(url[:i])
		if !networkInputSchemes[scheme] {
			return fmt.Errorf("unsupported network input scheme %q; supported: rtsp, rtsps, srt, udp, rtp, http, https", scheme)
		}
	default:
		if strings.TrimSpace(c.Input.VideoDevice) == "" {
			return errors.New("video device is required")
		}
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

// EffectiveEncoder returns the encoder to use.
//
// SRT destinations always use libx264. Apple's h264_videotoolbox is a
// quality-target encoder (soft CBR — it produces what the content needs
// even with -constant_bit_rate=1) and SRT receivers expect a true CBR
// stream that matches the declared bitrate.
func (c Config) EffectiveEncoder() Encoder {
	if c.OutputMode == OutputSRT {
		return EncoderX264
	}
	if c.Encoder == "" {
		return EncoderX264
	}
	return c.Encoder
}

func (c Config) OutputURL() string {
	if c.OutputMode == OutputSRT {
		// SRT URL formats vary by receiver (streamid syntax,
		// passphrase, latency, etc.). The user pastes the complete
		// URL; we pass it through verbatim. Mutating it would
		// URL-encode characters like ':' that some receivers
		// require literal.
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

// primaryOutputArgs returns the direct-output args (no tee) for the
// primary destination — used when HLS sidecar is disabled.
func (c Config) primaryOutputArgs() []string {
	switch c.OutputMode {
	case OutputSRT:
		// -flush_packets 1 + -muxdelay 0 -muxpreload 0 keep the muxer
		// low-latency: each packet is flushed immediately and the
		// muxer doesn't hold audio waiting for video PTS (default
		// muxdelay is 0.7 s).
		return []string{
			"-f", "mpegts",
			"-flush_packets", "1",
			"-muxdelay", "0",
			"-muxpreload", "0",
			c.OutputURL(),
		}
	default: // OutputRTMP and "" both go here
		out := []string{"-f", "flv"}
		if c.Network.TCPKeepalive {
			out = append(out, "-tcp_keepalive", "1")
		}
		if c.Network.RWTimeout > 0 {
			out = append(out, "-rw_timeout", fmt.Sprintf("%d", c.Network.RWTimeout.Microseconds()))
		}
		return append(out, c.OutputURL())
	}
}

// hlsOutputArgs returns the direct-output args for the HLS sidecar
// when there's no primary destination configured.
func (c Config) hlsOutputArgs() []string {
	return []string{
		"-f", "hls",
		"-hls_time", "6",
		"-hls_list_size", "10",
		"-hls_flags", "delete_segments+append_list+independent_segments",
		"-hls_segment_filename", fmt.Sprintf("%s/seg%%d.ts", c.HLSDir),
		fmt.Sprintf("%s/stream.m3u8", c.HLSDir),
	}
}

// primaryTeeSlave returns the tee-muxer slave spec for the primary
// destination. Slave options inside [...] are colon-separated; the
// URL follows after ']' and may contain any character except '|'.
//
// muxdelay and muxpreload are deliberately NOT included here even
// though the non-tee SRT path sets them: they're global format-
// context options that the tee slave option parser rejects with
// "Unknown option 'muxdelay'". The tee path still gets low-latency
// behaviour from -flush_packets and the encoder's own VBV settings.
func (c Config) primaryTeeSlave() (string, error) {
	switch c.OutputMode {
	case OutputSRT:
		return "[f=mpegts:flush_packets=1]" + c.OutputURL(), nil
	case OutputRTMP, "":
		opts := []string{"f=flv"}
		if c.Network.TCPKeepalive {
			opts = append(opts, "tcp_keepalive=1")
		}
		if c.Network.RWTimeout > 0 {
			opts = append(opts, fmt.Sprintf("rw_timeout=%d", c.Network.RWTimeout.Microseconds()))
		}
		return "[" + strings.Join(opts, ":") + "]" + c.OutputURL(), nil
	default:
		return "", fmt.Errorf("unsupported output mode for tee: %q", c.OutputMode)
	}
}

// hlsTeeSlave returns the tee-muxer slave spec for the HLS sidecar.
func (c Config) hlsTeeSlave() string {
	return fmt.Sprintf(
		"[f=hls:hls_time=6:hls_list_size=10:hls_flags=delete_segments+append_list+independent_segments:hls_segment_filename=%s/seg%%d.ts]%s/stream.m3u8",
		c.HLSDir, c.HLSDir,
	)
}

func (c Config) Args() ([]string, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	inputs, err := c.buildInputs()
	if err != nil {
		return nil, err
	}
	return c.argsWithInputs(inputs)
}

// argsWithInputs builds the full argv given an already-resolved
// inputBuild. Used by both Args (which calls buildInputs internally)
// and Build (which exposes the inputBuild's fallback metadata to
// the supervisor). Splitting keeps buildInputs's silent-audio fallback
// decision and the supervisor's recovery-watcher target perfectly in
// sync.
func (c Config) argsWithInputs(inputs inputBuild) ([]string, error) {
	args := []string{
		"-hide_banner",
		"-nostdin",
		"-loglevel", defaultString(c.LogLevel, "warning"),
		"-fflags", "nobuffer",
		"-flags", "low_delay",
		"-progress", "pipe:1",
		"-stats_period", "1",
	}
	args = append(args, inputs.args...)

	encoder := c.EffectiveEncoder()
	gop := fmt.Sprintf("%d", c.Preset.GOP())
	w, h := c.Preset.Width, c.Preset.Height

	// Single -filter_complex graph that:
	//   1. Runs deinterlace+scale+pad once on the source video and
	//      splits to [v] (primary encode) and [v_preview] (RTP).
	//      Without the split, the preview leg would re-scale the
	//      *raw* source independently — a 4K capture gets scaled
	//      twice. split keeps it to one pass.
	//   2. Runs aresample once on the source audio and splits to
	//      [a_enc] (primary), [a_preview] (Opus RTP), and a side
	//      [a_stats] branch that drives the audio meter via
	//      stderr-printed astats metadata. Mixing astats into the
	//      encode path would synchronously block the filter graph
	//      on stderr writes — producing AAC frames with gappy PTS
	//      that strict demuxers/players reject.
	//
	// Resampler options:
	//   async=1            broadcast-standard soft sync — pad/trim
	//                      only on stream start, never warps PTS
	//                      deltas mid-stream.
	//   min_hard_comp=0.1  fall back to hard pad/trim only after
	//                      100 ms of accumulated drift.
	//   osr=48000          pin output rate so encoders always see
	//                      48 kHz regardless of source rate.
	//
	// SDI/DeckLink can be interlaced so the software path deints with
	// yadif (no-op on progressive). VideoToolbox handles progressive
	// natively; scale_vt would need hwaccel frames AVFoundation can't
	// produce, so the CPU scaler runs cheaply alongside the GPU encoder.
	deint := "yadif=deint=interlaced,"
	if encoder == EncoderVideoToolbox {
		deint = ""
	}
	// Preview chain: fps decimation comes BEFORE the scale so we only
	// pay the scale cost on the frames we'll actually keep. Halving
	// 30→15 in front of scale saves ~50% of the preview-leg scaler
	// work — on a high-FPS source like a 60p capture, much more.
	//
	// HDR→SDR tone-map chain. Converts PQ/HLG (BT.2020) source pixels
	// to Rec.709 SDR before downstream scaling. Skipped for SDR
	// sources because the linear-light conversion costs real CPU and
	// compresses contrast on already-SDR material.
	//
	// Chain: zscale to linear (npl=100 SDR target), float32 for
	// tonemap precision, primaries to bt709, hable tonemap with
	// desat=0 (preserve saturation), zscale back to bt709/limited,
	// format to yuv420p for the encoder.
	tonemap := ""
	if c.Input.SourceIsHDR {
		tonemap = "zscale=t=linear:npl=100,format=gbrpf32le," +
			"zscale=p=bt709,tonemap=tonemap=hable:desat=0," +
			"zscale=t=bt709:m=bt709:r=tv,format=yuv420p,"
	}
	videoChain := fmt.Sprintf(
		"[%s]%s%sscale=%d:%d:force_original_aspect_ratio=decrease,"+
			"pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black,split=2[v][v_pre_src];"+
			"[v_pre_src]fps=%d,scale=640:360:force_original_aspect_ratio=decrease[v_preview]",
		inputs.videoMap, deint, tonemap, w, h, w, h, previewFPS,
	)
	audioChain := fmt.Sprintf(
		"[%s]aresample=async=1:min_hard_comp=0.100000:osr=48000,asplit=3[a_enc][a_preview][a_stats];"+
			"[a_stats]astats=metadata=1:reset=1:length=1,"+
			"ametadata=print:key=lavfi.astats.Overall.RMS_level:file=/dev/stderr,anullsink",
		inputs.audioMap,
	)

	args = append(args, "-filter_complex", videoChain+";"+audioChain)
	args = append(args, "-map", "[v]", "-map", "[a_enc]")

	// Encoder settings aligned with YouTube's H.264 recommendations:
	//   - High profile where supported (CABAC, 8-bit 4:2:0)
	//   - 2 B-frames, 1 reference frame, progressive scan (software)
	//   - CBR via -b:v == -maxrate, bufsize == maxrate for 1s VBV
	//   - 2-second keyframe interval (GOP = FPS * 2)
	//   - Rec.709 color primaries / transfer / matrix for SDR
	//   - AAC stereo at 48 kHz, encoder selected per platform
	// Color signaling goes BEFORE -c:v so hardware encoders
	// (VideoToolbox, NVENC, VAAPI, QSV) bake the VUI hints into
	// their SPS during init. Hardware encoders that read color
	// state at init-time would otherwise emit unspecified VUI,
	// which lets downstream transcoders (notably YouTube's ABR
	// pipeline) tonemap inconsistently. color_range tv signals
	// limited-range explicitly — without it video_full_range_flag
	// defaults to ambiguous and some players guess wrong.
	args = append(args,
		"-color_primaries", "bt709",
		"-color_trc", "bt709",
		"-colorspace", "bt709",
		"-color_range", "tv",
	)
	args = append(args, c.encoderArgs(encoder, gop)...)
	args = append(args,
		"-b:v", c.Preset.VideoBitrate(),
		"-maxrate", c.Preset.VideoBitrate(),
		"-bufsize", c.Preset.BufferSize(),
		"-g", gop,
		"-r", fmt.Sprintf("%d", c.Preset.FPS),
		"-pix_fmt", "yuv420p",
	)
	args = append(args, c.audioEncoderArgs()...)
	args = append(args,
		"-b:a", c.Preset.AudioBitrate(),
		"-ar", "48000",
		"-ac", "2",
	)

	// Primary destination + optional HLS sidecar.
	//
	// When both are enabled we use ffmpeg's tee muxer so a single
	// encoder feeds both outputs — re-encoding twice would double
	// CPU, and -c copy on the raw input streams produces invalid
	// HLS (the inputs are uyvy422 / PCM, not H.264 / AAC).
	hlsEnabled := c.EnableHLS && strings.TrimSpace(c.HLSDir) != ""
	switch {
	case c.primaryRunnable() && hlsEnabled:
		primary, err := c.primaryTeeSlave()
		if err != nil {
			return nil, err
		}
		args = append(args, "-f", "tee", primary+"|"+c.hlsTeeSlave())
	case c.primaryRunnable():
		args = append(args, c.primaryOutputArgs()...)
	case hlsEnabled:
		// HLS-only run. Same encoded output as the primary path
		// would have used, just muxed to HLS instead.
		args = append(args, c.hlsOutputArgs()...)
	}

	// Secondary output: low-res H.264 RTP for live browser preview.
	// Maps [v_preview] (already scaled to 640×360 and decimated to
	// 15 fps in the filter graph) so we don't re-process the source.
	args = append(args, "-map", "[v_preview]")
	args = append(args, c.previewEncoderArgs(encoder)...)
	args = append(args,
		// GOP = previewFPS * 2 for a 2-second keyframe interval that
		// matches the preset's cadence; keyint_min = previewFPS gates
		// scenecut closer to GOP boundaries.
		"-g", strconv.Itoa(previewFPS*2),
		"-keyint_min", strconv.Itoa(previewFPS),
		"-b:v", "800k",
		"-flush_packets", "1", "-muxdelay", "0", "-muxpreload", "0",
		"-payload_type", "96",
		"-f", "rtp",
		"rtp://127.0.0.1:52001?pkt_size=1200",
	)

	// Tertiary output: Opus RTP for live browser audio meter.
	// Maps [a_preview] (the resampled 48 kHz copy from the audio
	// asplit) so playback timing matches the primary encode.
	args = append(args,
		"-map", "[a_preview]",
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
		// configured target for low-motion content (a static scene can
		// come in at 1-2 Mbps even when -b:v says 10 Mbps). Live ingest
		// receivers expect a constant rate at or near the target.
		// Requires macOS 13+ / current VideoToolbox.
		//
		// -force_key_frames pins IDR boundaries to exact 2-second
		// intervals. VideoToolbox respects this more reliably than
		// -g alone, which is important because YouTube's transcoder
		// keys segment boundaries off our keyframe cadence.
		return []string{
			"-c:v", "h264_videotoolbox",
			"-profile:v", "high",
			"-level:v", "4.1",
			"-allow_sw", "1",
			"-realtime", "1",
			"-constant_bit_rate", "1",
			"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", keyframeIntervalSec),
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
		// Industry-standard live H.264 CBR settings:
		//   filler=1     VBV filler NALs to maintain hard-CBR without
		//                emitting HRD parameters in the SPS VUI.
		//   open-gop=0   closed GOP — every keyframe is IDR, no frames
		//                reference across keyframe boundaries.
		//   scenecut=0   no scene-change keyframes; keyframes only at
		//                the configured GOP boundary so receivers
		//                segment predictably.
		// No -tune zerolatency: it disables B-frames, which YouTube
		// explicitly recommends keeping (2 B-frames).
		// (-refs is left at the veryfast preset's default of 1.
		// keyint/min-keyint live inside -x264-params; setting them
		// twice via the flag and the param string is redundant.)
		return []string{
			"-c:v", "libx264",
			"-preset", "veryfast",
			"-profile:v", "high",
			"-level:v", "4.1",
			"-sc_threshold", "0",
			"-bf", "2",
			"-x264-params", "filler=1:open-gop=0:scenecut=0:keyint=" + gop + ":min-keyint=" + gop,
		}
	}
}

// audioEncoderArgs picks the best available AAC-LC encoder for the
// current platform. On macOS we prefer aac_at (Apple AudioToolbox),
// which produces noticeably better quality per bit than FFmpeg's
// native encoder at 128–192 kbps — particularly on music and choir
// content — and is what OBS Studio ships on macOS. Elsewhere we
// fall back to the native aac encoder with -aac_coder twoloop
// (the higher-quality two-loop search coder, not the default fast
// coder which can produce frames strict demuxers reject).
func (c Config) audioEncoderArgs() []string {
	if runtime.GOOS == "darwin" {
		return []string{"-c:a", "aac_at"}
	}
	return []string{"-c:a", "aac", "-aac_coder", "twoloop"}
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
	// audioFallbackDevice is set when buildInputs substituted silent
	// audio because the configured audio device couldn't be resolved.
	// Names the missing device so the supervisor can watch for it to
	// come back. Empty when audio was resolved normally or when the
	// input path doesn't support silent-audio fallback.
	audioFallbackDevice string
}

const silentAudio = "anullsrc=channel_layout=stereo:sample_rate=48000"

// buildNetworkInput constructs FFmpeg input args for a URL-based
// source (RTSP camera, SRT pull, UDP multicast, HTTP/HLS).
//
// Each scheme gets transport tuning that matches what production
// rigs use:
//   - rtsp / rtsps: TCP transport so we don't lose packets the way
//     UDP RTSP does on lossy LANs; reduced socket-buffer timeouts.
//   - srt: rely on URL params (latency, passphrase, streamid, etc.)
//   - udp / rtp: prefer larger fifo so packet bursts don't drop.
//   - http / https: behave like a normal pull; rely on TCP.
//
// Audio: we map 0:a for the network source by default; most live
// network sources (IP cameras, SRT pulls, broadcast HLS) carry an
// audio track. If the operator marks NoAudio (or the source is
// known to be video-only), we add a silent stereo lavfi input as a
// second source and the audio map points there instead.
func (c Config) buildNetworkInput() inputBuild {
	url := strings.TrimSpace(c.Input.URL)
	scheme := ""
	if i := strings.Index(url, "://"); i > 0 {
		scheme = strings.ToLower(url[:i])
	}

	// Per-input flags apply to the next -i. These override the
	// global -fflags nobuffer / -flags low_delay defaults that
	// are tuned for capture devices (where we want minimum
	// latency) but actively hurt network ingest:
	//
	//   +discardcorrupt  drops frames that reference pictures we
	//                    haven't received yet — happens when an
	//                    RTSP/SRT pull starts mid-GOP before the
	//                    next IDR arrives. Without this the H.264
	//                    decoder logs "mmco: unref short failure"
	//                    and never produces a clean frame.
	//   +genpts          regenerate PTS for sources that ship
	//                    weird timestamps (negative start times,
	//                    non-monotonic clocks, etc.).
	//   analyzeduration  and probesize: cap how long FFmpeg
	//                    inspects the input before emitting the
	//                    first output. Default is 5 seconds /
	//                    5 MB; 1 s / 1 MB is plenty for a stream
	//                    that's already announcing its codec.
	args := []string{
		"-fflags", "+discardcorrupt+genpts",
		"-analyzeduration", "1000000",
		"-probesize", "1000000",
	}

	switch scheme {
	case "rtsp", "rtsps":
		// TCP avoids the UDP-RTSP packet-loss / NAT-traversal pain
		// that's typical on church LANs with PoE switches.
		args = append(args, "-rtsp_transport", "tcp")
	case "srt":
		// libsrt reads transport options from URL params (latency,
		// passphrase, streamid, etc.). Nothing to add here.
	case "udp", "rtp":
		// MPEG-TS over UDP tends to be bursty; bigger fifo prevents
		// drops on the source side, overrun_nonfatal keeps us
		// running through transient overruns.
		args = append(args,
			"-fifo_size", "1000000",
			"-overrun_nonfatal", "1",
		)
	case "http", "https":
		// HLS / progressive HTTP pulls: auto-reconnect on transient
		// network blips. -reconnect_streamed 1 resumes mid-stream
		// instead of erroring at the first disconnect.
		args = append(args,
			"-reconnect", "1",
			"-reconnect_streamed", "1",
			"-reconnect_delay_max", "5",
			"-multiple_requests", "1",
		)
	}
	args = append(args, "-i", url)

	build := inputBuild{
		args:     args,
		videoMap: "0:v",
		audioMap: "0:a",
	}
	if c.Input.NoAudio {
		build.args = append(build.args, "-f", "lavfi", "-i", silentAudio)
		build.audioMap = "1:a"
	}
	return build
}

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
	if c.Input.Kind == InputNetwork {
		return c.buildNetworkInput(), nil
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
		// Audio device: try strict resolution, but if the configured
		// mic isn't present right now, FALL BACK to silent audio
		// rather than refusing to start. Losing audio mid-Sunday is
		// recoverable (the supervisor watches for the device to come
		// back and restarts); losing the whole stream is not.
		audio := c.Input.AudioDevice
		audioFallback := ""
		if c.Input.AudioDeviceName != "" {
			resolved, err := ResolveAVFoundationDeviceIndexStrict(c.Binary, c.Input.AudioDeviceName, "audio", c.Input.AudioDevice)
			if err != nil {
				audio = "" // triggers silent-audio branch below
				audioFallback = c.Input.AudioDeviceName
			} else {
				audio = resolved
			}
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
			audioFallbackDevice: audioFallback,
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

// BuildResult bundles Args's command-line output with metadata about
// what buildInputs actually did. The supervisor uses this metadata
// (e.g. AudioFallbackDevice) to drive recovery watchers consistent
// with the FFmpeg invocation we just started — not a separate later
// probe whose answer could differ from what buildInputs decided.
type BuildResult struct {
	Args                []string
	AudioFallbackDevice string
}

// Build is the supervisor-facing entry point. It runs buildInputs +
// Args in one call and returns both the argv FFmpeg will see and the
// fallback metadata buildInputs produced. Consistency is the point:
// if buildInputs substituted silent audio, BuildResult.AudioFallbackDevice
// names the missing mic — and that mic is the one we should watch for,
// not whatever a later re-probe happens to find.
func (c Config) Build() (BuildResult, error) {
	if err := c.Validate(); err != nil {
		return BuildResult{}, err
	}
	inputs, err := c.buildInputs()
	if err != nil {
		return BuildResult{}, err
	}
	args, err := c.argsWithInputs(inputs)
	if err != nil {
		return BuildResult{}, err
	}
	return BuildResult{Args: args, AudioFallbackDevice: inputs.audioFallbackDevice}, nil
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
