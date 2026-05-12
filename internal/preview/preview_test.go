package preview

import (
	"strings"
	"testing"

	"github.com/ssimpson89/easystream/internal/ffmpeg"
)

func TestPreviewArgsIncludeAudioMeterRTP(t *testing.T) {
	cfg := ffmpeg.DefaultConfig()
	args := previewArgs(cfg, 52001, 52002)
	joined := strings.Join(args, " ")

	for _, expected := range []string{
		"-map 0:v -an",
		"rtp://127.0.0.1:52001?pkt_size=1200",
		"-map 1:a -vn -c:a libopus -ar 48000 -ac 2 -b:a 64k",
		"-flush_packets 1 -muxdelay 0 -muxpreload 0 -payload_type 111 -f rtp rtp://127.0.0.1:52002?pkt_size=1200",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected preview args to contain %q, got %s", expected, joined)
		}
	}
}

// TestPreviewArgsHandleNetworkInput verifies the preview ffmpeg
// picks up the same network-source tuning as the main pipeline.
// Without an explicit InputNetwork case in previewInputs, the
// preview falls back to avfoundation with an empty device and
// produces no frames — exactly the "blank preview on RTSP" bug.
func TestPreviewArgsHandleNetworkInput(t *testing.T) {
	cfg := ffmpeg.DefaultConfig()
	cfg.Input = ffmpeg.Input{
		Kind: ffmpeg.InputNetwork,
		URL:  "rtsp://camera.local:554/stream1",
	}
	args := previewArgs(cfg, 52001, 52002)
	joined := strings.Join(args, " ")

	for _, expected := range []string{
		"-rtsp_transport tcp",
		"-fflags discardcorrupt+genpts", // replaces global nobuffer
		"-i rtsp://camera.local:554/stream1",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected preview args to contain %q for RTSP source, got: %s", expected, joined)
		}
	}
	// Must NOT fall through to avfoundation when source is network.
	if strings.Contains(joined, "-f avfoundation") {
		t.Errorf("preview must not start avfoundation for a network source: %s", joined)
	}
}
