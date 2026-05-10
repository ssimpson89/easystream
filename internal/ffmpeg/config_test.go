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
		"-b:v 10000k",         // YT-recommended 1080p30
		"-maxrate 10000k",     // CBR
		"-bufsize 20000k",     // 2x bitrate
		"-g 60",               // 2-second keyframe interval
		"-bf 2",               // YT: 2 B-frames
		"-refs 1",             // YT: 1 reference frame
		"-profile:v high",     // YT requires High profile for CABAC
		"-colorspace bt709",   // YT: Rec.709 SDR
		"open-gop=0",          // Cloudflare requires closed GOP
		"-tcp_keepalive 1",
		"-rw_timeout 12000000",
		"rtmps://a.rtmps.youtube.com/live2/abc-def-ghi",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected args to contain %q, got %s", expected, joined)
		}
	}
}
