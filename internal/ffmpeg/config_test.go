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
