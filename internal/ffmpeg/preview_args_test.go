package ffmpeg

import (
	"strings"
	"testing"

	"github.com/ssimpson89/easystream/internal/quality"
)

func TestArgsIncludesPreviewOutput(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Input.Kind = InputTestVideo
	cfg.Preset = quality.Default()
	cfg.OutputMode = OutputRTMP
	cfg.IngestURL = "rtmp://example.com/live"
	cfg.StreamName = "abc"
	args, err := cfg.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "rtp://127.0.0.1:52001") {
		t.Errorf("expected preview RTP output, got:\n%s", joined)
	}
	if !strings.Contains(joined, "rtmp://example.com/live/abc") {
		t.Errorf("expected main RTMP output, got:\n%s", joined)
	}
	// Preview should be 640x360 with ultrafast preset, distinct from main.
	if !strings.Contains(joined, "scale=640:360") {
		t.Errorf("expected preview to scale to 640:360, got:\n%s", joined)
	}
	if !strings.Contains(joined, "payload_type 96") {
		t.Errorf("expected RTP payload_type 96, got:\n%s", joined)
	}
}
