package ffmpeg

import (
	"strings"
	"testing"
	"time"

	"github.com/ssimpson89/easystream/internal/quality"
)

func TestArgsIncludeBandwidthAndRecoveryOptions(t *testing.T) {
	preset, _ := quality.ByID("recommended")
	cfg := DefaultConfig()
	cfg.Preset = preset
	cfg.StreamName = "abc-def-ghi"
	cfg.Network.RWTimeout = 12 * time.Second

	args, err := cfg.Args()
	if err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(args, " ")
	for _, expected := range []string{
		"-b:v 10000k",       // YT-recommended 1080p30
		"-maxrate 10000k",   // CBR target
		"-bufsize 10000k",   // 1s VBV (bufsize == maxrate) — true CBR
		"-g 60",             // 2-second keyframe interval
		"-bf 2",             // YT: 2 B-frames (refs is left at veryfast default of 1)
		"-profile:v high",   // YT requires High profile for CABAC
		"-colorspace bt709", // YT: Rec.709 SDR
		"-color_range tv",   // explicit limited-range VUI signaling
		"filler=1",          // VBV filler NALs maintain hard-CBR
		"open-gop=0",        // Closed GOP
		"scenecut=0",        // No scene-change keyframes; predictable segmenting
		"-tcp_keepalive 1",
		"-rw_timeout 12000000",
		"aresample=async=1:min_hard_comp=0.100000:osr=48000", // soft drift correction; no PTS-warping
		"ametadata=print:key=lavfi.astats.Overall.RMS_level:file=/dev/stderr",
		"rtmps://a.rtmps.youtube.com/live2/abc-def-ghi",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected args to contain %q, got %s", expected, joined)
		}
	}
}

func TestChooseAVFoundationFramerateUsesClosestSupportedMode(t *testing.T) {
	output := `Selected framerate (29.970030) is not supported by the device.
Supported modes: 1280x720@[30.000000 30.000000]fps, 1920x1080@[60.000000 60.000000]fps`

	fps, ok := chooseAVFoundationFramerate(output, 60)
	if !ok {
		t.Fatal("expected framerate to be parsed")
	}
	if fps != "60" {
		t.Fatalf("expected 60fps, got %s", fps)
	}

	fps, ok = chooseAVFoundationFramerate(output, 30)
	if !ok {
		t.Fatal("expected framerate to be parsed")
	}
	if fps != "30" {
		t.Fatalf("expected 30fps, got %s", fps)
	}
}

func TestArgsIncludesPreviewAudioMeterOutput(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StreamName = "abc"

	args, err := cfg.Args()
	if err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(args, " ")
	for _, expected := range []string{
		"rtp://127.0.0.1:52001?pkt_size=1200",
		"-map [a_preview] -c:a libopus -ar 48000 -ac 2 -b:a 64k",
		"-flush_packets 1 -muxdelay 0 -muxpreload 0 -payload_type 111 -f rtp rtp://127.0.0.1:52002?pkt_size=1200",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected args to contain %q, got %s", expected, joined)
		}
	}
}

func TestArgsSetAVFoundationCaptureOptions(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Binary = "ffmpeg-test-mock" // force probe to fallback to 30
	cfg.Input.Kind = InputWebcam
	cfg.Input.Backend = "avfoundation"
	cfg.Input.VideoDevice = "0"
	cfg.Input.AudioDevice = "1"
	cfg.StreamName = "abc"

	args, err := cfg.Args()
	if err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(args, " ")
	for _, expected := range []string{
		"-f avfoundation -framerate 30 -i 0:1",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected args to contain %q, got %s", expected, joined)
		}
	}
}

func TestChooseAVFoundationDeviceIndex(t *testing.T) {
	output := `[AVFoundation indev @ 0x7f] AVFoundation video devices:
[AVFoundation indev @ 0x7f] [0] OBS Virtual Camera
[AVFoundation indev @ 0x7f] [1] FaceTime HD Camera
[AVFoundation indev @ 0x7f] [2] Capture screen 0
[AVFoundation indev @ 0x7f] AVFoundation audio devices:
[AVFoundation indev @ 0x7f] [0] Microsoft Teams Audio
[AVFoundation indev @ 0x7f] [1] MacBook Air Microphone`

	// Video: find FaceTime when it moved to index 1
	idx, ok := chooseAVFoundationDeviceIndex(output, "FaceTime HD Camera", "video")
	if !ok || idx != "1" {
		t.Fatalf("expected video index 1, got %q ok=%v", idx, ok)
	}

	// Video: find OBS at index 0
	idx, ok = chooseAVFoundationDeviceIndex(output, "OBS Virtual Camera", "video")
	if !ok || idx != "0" {
		t.Fatalf("expected video index 0, got %q ok=%v", idx, ok)
	}

	// Audio: find MacBook Air Microphone
	idx, ok = chooseAVFoundationDeviceIndex(output, "MacBook Air Microphone", "audio")
	if !ok || idx != "1" {
		t.Fatalf("expected audio index 1, got %q ok=%v", idx, ok)
	}

	// Audio: name not found
	_, ok = chooseAVFoundationDeviceIndex(output, "NonexistentMic", "audio")
	if ok {
		t.Fatal("expected not found for nonexistent audio device")
	}

	// Video name in audio section should not match
	_, ok = chooseAVFoundationDeviceIndex(output, "MacBook Air Microphone", "video")
	if ok {
		t.Fatal("expected not found for audio device searched in video section")
	}
}

func networkInputCfg(url string) Config {
	cfg := DefaultConfig()
	cfg.Input = Input{Kind: InputNetwork, URL: url}
	cfg.IngestURL = "rtmp://localhost"
	cfg.StreamName = "test"
	return cfg
}

func TestArgsNetworkInputRTSP(t *testing.T) {
	cfg := networkInputCfg("rtsp://camera.local:554/stream1")

	args, err := cfg.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, expected := range []string{
		"-rtsp_transport tcp",
		"-i rtsp://camera.local:554/stream1",
		"-map [v]",
		"-map [a_enc]",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("expected %q in args: %s", expected, joined)
		}
	}
}

func TestArgsNetworkInputSRTPull(t *testing.T) {
	cfg := networkInputCfg("srt://upstream.example.com:9999?streamid=read:test")

	args, err := cfg.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-i srt://upstream.example.com:9999?streamid=read:test") {
		t.Errorf("expected SRT pull input verbatim: %s", joined)
	}
	if strings.Contains(joined, "-rtsp_transport") {
		t.Errorf("did not expect RTSP transport on SRT pull: %s", joined)
	}
}

func TestArgsNetworkInputNoAudioAddsSilentTrack(t *testing.T) {
	cfg := networkInputCfg("rtsp://hdmi-only-camera/feed")
	cfg.Input.NoAudio = true

	args, err := cfg.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "anullsrc=channel_layout=stereo") {
		t.Errorf("expected silent audio lavfi input when NoAudio set: %s", joined)
	}
	if !strings.Contains(joined, "-map [a_enc]") {
		t.Errorf("expected encoded audio mapped: %s", joined)
	}
}

func TestNetworkInputValidateRejectsBadSchemes(t *testing.T) {
	cfg := networkInputCfg("file:///etc/passwd")
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation to reject file:// scheme")
	}
}

func TestNetworkInputValidateRequiresURL(t *testing.T) {
	cfg := networkInputCfg("")
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation to require URL for network input")
	}
}

func srtListenerCfg(port int, passphrase string) Config {
	cfg := DefaultConfig()
	cfg.Input = Input{
		Kind:                InputSRTListener,
		SRTListenPort:       port,
		SRTListenPassphrase: passphrase,
	}
	cfg.IngestURL = "rtmp://localhost"
	cfg.StreamName = "test"
	return cfg
}

func TestSRTListenerURLConstruction(t *testing.T) {
	got := SRTListenerURL(9000, "")
	want := "srt://0.0.0.0:9000?mode=listener&latency=300000"
	if got != want {
		t.Errorf("SRTListenerURL(9000,\"\") = %q, want %q", got, want)
	}

	got = SRTListenerURL(0, "") // 0 → default 9999
	want = "srt://0.0.0.0:9999?mode=listener&latency=300000"
	if got != want {
		t.Errorf("SRTListenerURL(0,\"\") fell-through default port: got %q", got)
	}

	got = SRTListenerURL(9999, "MySecretPwd")
	if !strings.Contains(got, "passphrase=MySecretPwd") {
		t.Errorf("expected passphrase in listener URL: %q", got)
	}
	// libsrt rejects srt://:port (empty host) — must use 0.0.0.0.
	if strings.Contains(got, "srt://:") {
		t.Errorf("listener URL must not have empty host: %q", got)
	}
}

func TestArgsSRTListenerInput(t *testing.T) {
	cfg := srtListenerCfg(9000, "")
	args, err := cfg.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, expected := range []string{
		"-fflags discardcorrupt+genpts",
		"-i srt://0.0.0.0:9000?mode=listener&latency=300000",
		"-map [v]",
		"-map [a_enc]",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("expected %q in SRT listener args: %s", expected, joined)
		}
	}
}

func TestSRTListenerValidateRejectsPrivilegedPort(t *testing.T) {
	cfg := srtListenerCfg(80, "")
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation to reject port < 1024")
	}
}

func TestSRTListenerValidateRejectsShortPassphrase(t *testing.T) {
	cfg := srtListenerCfg(9999, "short")
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation to reject < 10-char passphrase")
	}
}

func TestHDRSourceAddsTonemapChain(t *testing.T) {
	cfg := networkInputCfg("rtsp://hdr-camera.local/feed")
	cfg.Input.SourceIsHDR = true

	args, err := cfg.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "zscale=t=linear:npl=100") {
		t.Errorf("expected HDR tonemap chain when SourceIsHDR set: %s", joined)
	}
	if !strings.Contains(joined, "tonemap=tonemap=hable") {
		t.Errorf("expected hable tonemap when SourceIsHDR set: %s", joined)
	}
	if !strings.Contains(joined, "zscale=t=bt709:m=bt709:r=tv") {
		t.Errorf("expected BT.709 SDR conversion when SourceIsHDR set: %s", joined)
	}
}

func TestSDRSourceSkipsTonemap(t *testing.T) {
	cfg := networkInputCfg("rtsp://sdr-camera.local/feed")
	cfg.Input.SourceIsHDR = false

	args, err := cfg.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "zscale=t=linear") {
		t.Errorf("did not expect tonemap chain on SDR source: %s", joined)
	}
}

func TestArgsSRTOutput(t *testing.T) {
	cfg := DefaultConfig()
	cfg.OutputMode = OutputSRT
	cfg.IngestURL = "srt://example.com:9999"
	cfg.StreamName = ""

	args, err := cfg.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, expected := range []string{
		"-f mpegts",
		"-flush_packets 1",
		"srt://example.com:9999",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("expected %q in args: %s", expected, joined)
		}
	}
	if strings.Contains(joined, "-f flv") {
		t.Errorf("did not expect RTMP flags in SRT output: %s", joined)
	}
}

// TestArgsSRTPassesURLVerbatim confirms we don't mutate the SRT URL.
// Receivers use varied streamid/passphrase/latency syntax — any
// re-encoding (e.g. %3A for ':') breaks the handshake.
func TestArgsSRTPassesURLVerbatim(t *testing.T) {
	cfg := DefaultConfig()
	cfg.OutputMode = OutputSRT
	cfg.IngestURL = "srt://ingest.example.com:9999?streamid=abc-input:secret&latency=4000"
	cfg.StreamName = "ignored-for-srt"

	args, err := cfg.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "srt://ingest.example.com:9999?streamid=abc-input:secret&latency=4000") {
		t.Errorf("expected SRT URL passed verbatim, got: %s", joined)
	}
	if strings.Contains(joined, "%3A") {
		t.Errorf("URL must not URL-encode the colon in streamid: %s", joined)
	}
}

// TestSRTRequiresOnlyURL verifies that SRT validates with just a URL
// (no stream key), since the streamid goes inside the URL.
func TestSRTRequiresOnlyURL(t *testing.T) {
	cfg := DefaultConfig()
	cfg.OutputMode = OutputSRT
	cfg.IngestURL = "srt://example.com:9999?streamid=foo"
	cfg.StreamName = ""
	if err := cfg.Validate(); err != nil {
		t.Errorf("SRT with URL-only should validate, got: %v", err)
	}
}

func TestArgsHLSToggleAlongsideRTMP(t *testing.T) {
	cfg := DefaultConfig()
	cfg.IngestURL = "rtmp://x"
	cfg.StreamName = "abc"
	cfg.EnableHLS = true
	cfg.HLSDir = "/tmp/hls"

	args, err := cfg.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	// Primary + HLS sidecar share one encoder via the tee muxer —
	// so the args should NOT have separate -f flv / -f hls outputs,
	// they should be a single -f tee with two bracketed targets.
	if !strings.Contains(joined, "-f tee ") {
		t.Errorf("expected tee muxer to fan out one encoder to RTMP+HLS: %s", joined)
	}
	if !strings.Contains(joined, "[f=flv:tcp_keepalive=1") {
		t.Errorf("expected tee primary slave spec: %s", joined)
	}
	if !strings.Contains(joined, "[f=hls:hls_time=6") {
		t.Errorf("expected tee HLS slave spec: %s", joined)
	}
	// HLS must NOT use -c copy on raw input — that produces invalid
	// HLS because inputs are uyvy422/PCM, not H.264/AAC. tee shares
	// the global -c:v libx264 / -c:a aac_at across both targets.
	if strings.Contains(joined, "-c copy") {
		t.Errorf("HLS must not use -c copy on raw inputs: %s", joined)
	}
}

func TestArgsHLSOnly(t *testing.T) {
	cfg := DefaultConfig()
	cfg.IngestURL = ""
	cfg.StreamName = ""
	cfg.EnableHLS = true
	cfg.HLSDir = "/tmp/hls"

	args, err := cfg.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "-f flv") {
		t.Errorf("did not expect RTMP output for HLS-only: %s", joined)
	}
	if !strings.Contains(joined, "-f hls") {
		t.Errorf("expected HLS output: %s", joined)
	}
}

func TestValidateRequiresAnyOutput(t *testing.T) {
	cfg := DefaultConfig()
	cfg.IngestURL = ""
	cfg.StreamName = ""
	cfg.EnableHLS = false
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validate to fail when no output is configured")
	}
}

// FFmpeg 8.1.1 (Homebrew on Apple Silicon) real output. Note `indev`
// (not "input device") — the section-header detection must remain
// robust against the prefix wording change across FFmpeg versions.
const ffmpeg8AVFoundationOutput = `[AVFoundation indev @ 0x7c4c14140] AVFoundation video devices:
[AVFoundation indev @ 0x7c4c14140] [0] OBS Virtual Camera
[AVFoundation indev @ 0x7c4c14140] [1] FaceTime HD Camera
[AVFoundation indev @ 0x7c4c14140] [2] Ssimpson Camera
[AVFoundation indev @ 0x7c4c14140] [3] Ssimpson Desk View Camera
[AVFoundation indev @ 0x7c4c14140] [4] Capture screen 0
[AVFoundation indev @ 0x7c4c14140] AVFoundation audio devices:
[AVFoundation indev @ 0x7c4c14140] [0] NDI Audio
[AVFoundation indev @ 0x7c4c14140] [1] MacBook Air Microphone
[AVFoundation indev @ 0x7c4c14140] [2] Ssimpson Microphone
[in#0 @ 0x7c4c14000] Error opening input: Input/output error`

func TestMatchAVFoundationDevicesAgainstFFmpeg8Fixture(t *testing.T) {
	// Continuity Camera Desk View — name with multiple words including
	// "Desk View". The regex must capture the full name despite the
	// embedded "Desk" word which the parser doesn't special-case.
	matches := matchAVFoundationDevices(ffmpeg8AVFoundationOutput, "Ssimpson Desk View Camera", "video")
	if len(matches) != 1 || matches[0] != "3" {
		t.Fatalf("expected [3] for Desk View Camera, got %v", matches)
	}
	// Audio name should not match a video device with the same word.
	matches = matchAVFoundationDevices(ffmpeg8AVFoundationOutput, "Ssimpson Camera", "audio")
	if len(matches) != 0 {
		t.Errorf("video-only name should not match in audio section: %v", matches)
	}
	// Capture screen 0 — name contains a digit at the end. Confirms
	// the regex doesn't greedily eat the trailing "0".
	matches = matchAVFoundationDevices(ffmpeg8AVFoundationOutput, "Capture screen 0", "video")
	if len(matches) != 1 || matches[0] != "4" {
		t.Errorf("expected [4] for Capture screen 0, got %v", matches)
	}
}

func TestMatchAVFoundationDevicesDetectsDuplicateNames(t *testing.T) {
	// Two HDMI capture cards both reporting the same name — a real-world
	// AVFoundation case that the old chooseAVFoundationDeviceIndex would
	// silently resolve to the first match, picking the wrong twin.
	output := `[AVFoundation indev] AVFoundation video devices:
[AVFoundation indev] [0] FaceTime HD Camera
[AVFoundation indev] [1] USB Capture HDMI
[AVFoundation indev] [2] USB Capture HDMI
[AVFoundation indev] AVFoundation audio devices:
[AVFoundation indev] [0] MacBook Air Microphone`

	matches := matchAVFoundationDevices(output, "USB Capture HDMI", "video")
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d (%v)", len(matches), matches)
	}
	if matches[0] != "1" || matches[1] != "2" {
		t.Fatalf("expected indexes 1,2, got %v", matches)
	}
}

func TestEncoderArgsVideoToolbox(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Encoder = EncoderVideoToolbox
	cfg.StreamName = "abc"

	args, err := cfg.Args()
	if err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(args, " ")
	for _, expected := range []string{
		"-c:v h264_videotoolbox",
		"-allow_sw 1",
		"-realtime 1",
		"-profile:v high",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected args to contain %q, got %s", expected, joined)
		}
	}
	// Should NOT contain x264-specific flags.
	for _, absent := range []string{
		"-preset veryfast",
		"-x264-params",
	} {
		if strings.Contains(joined, absent) {
			t.Fatalf("did not expect args to contain %q for videotoolbox", absent)
		}
	}
}

func TestDefaultEncoderIsX264(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StreamName = "abc"

	args, err := cfg.Args()
	if err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-c:v libx264") {
		t.Fatalf("expected default encoder to be libx264, got %s", joined)
	}
	if !strings.Contains(joined, "-preset veryfast") {
		t.Fatalf("expected veryfast preset for libx264, got %s", joined)
	}
}
