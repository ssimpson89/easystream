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
