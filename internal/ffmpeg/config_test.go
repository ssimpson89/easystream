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
		"-b:v 8000k",
		"-maxrate 8000k",
		"-bufsize 16000k",
		"-g 60",
		"-tcp_keepalive 1",
		"-rw_timeout 12000000",
		"rtmps://a.rtmps.youtube.com/live2/abc-def-ghi",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected args to contain %q, got %s", expected, joined)
		}
	}
}
