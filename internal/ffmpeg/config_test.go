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
		"-maxrate 10000k",   // CBR
		"-bufsize 20000k",   // 2x bitrate
		"-g 60",             // 2-second keyframe interval
		"-bf 2",             // YT: 2 B-frames
		"-refs 1",           // YT: 1 reference frame
		"-profile:v high",   // YT requires High profile for CABAC
		"-colorspace bt709", // YT: Rec.709 SDR
		"open-gop=0",        // Cloudflare requires closed GOP
		"-tcp_keepalive 1",
		"-rw_timeout 12000000",
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
		"-map 1:a -vn -c:a libopus -ar 48000 -ac 2 -b:a 64k",
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
		"+resend_headers+initial_discontinuity",
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
// SRT URL formats vary too much across receivers (Cloudflare's
// "streamid=id:password" gets URL-encoded to "streamid=id%3Apassword"
// if we touch it, which Cloudflare rejects). User pastes the full URL.
func TestArgsSRTPassesURLVerbatim(t *testing.T) {
	cfg := DefaultConfig()
	cfg.OutputMode = OutputSRT
	cfg.IngestURL = "srt://live.cloudflare.com:778?streamid=abc-input:secret&latency=4000"
	cfg.StreamName = "ignored-for-srt"

	args, err := cfg.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "srt://live.cloudflare.com:778?streamid=abc-input:secret&latency=4000") {
		t.Errorf("expected SRT URL passed verbatim, got: %s", joined)
	}
	if strings.Contains(joined, "%3A") {
		t.Errorf("URL must not URL-encode the colon in Cloudflare-style streamid: %s", joined)
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
	if !strings.Contains(joined, "-f flv") {
		t.Errorf("expected primary RTMP output: %s", joined)
	}
	if !strings.Contains(joined, "-f hls") {
		t.Errorf("expected secondary HLS output: %s", joined)
	}
	if !strings.Contains(joined, "-c copy") {
		t.Errorf("expected HLS to use -c copy (no re-encode): %s", joined)
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
