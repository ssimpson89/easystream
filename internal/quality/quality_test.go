package quality

import "testing"

func TestPresetsHaveExpectedBandwidthTiers(t *testing.T) {
	if len(Presets) < 4 {
		t.Fatalf("expected at least 4 quality presets, got %d", len(Presets))
	}

	last := 0
	for _, preset := range Presets {
		if preset.VideoKbps <= last {
			t.Fatalf("preset %q is not ordered by increasing video bitrate", preset.ID)
		}
		if preset.GOP() != preset.FPS*2 {
			t.Fatalf("preset %q should use a 2-second GOP", preset.ID)
		}
		last = preset.VideoKbps
	}
}

// TestPresetsAlignWithYouTubeRecommendations verifies our bitrates match
// YouTube's published H.264 recommendations for each resolution/framerate.
// Source: https://support.google.com/youtube/answer/2853702
func TestPresetsAlignWithYouTubeRecommendations(t *testing.T) {
	want := map[string]struct {
		W, H, FPS, Kbps int
	}{
		"low":         {1280, 720, 30, 4000},   // YT 720p30: 4 Mbps
		"standard":    {1280, 720, 60, 6000},   // YT 720p60: 6 Mbps
		"recommended": {1920, 1080, 30, 10000}, // YT 1080p30: 10 Mbps
		"high":        {1920, 1080, 60, 12000}, // YT 1080p60: 12 Mbps
	}
	for id, w := range want {
		p, ok := ByID(id)
		if !ok {
			t.Errorf("missing preset %q", id)
			continue
		}
		if p.Width != w.W || p.Height != w.H || p.FPS != w.FPS || p.VideoKbps != w.Kbps {
			t.Errorf("preset %q = %dx%d@%dfps %dk, want %dx%d@%dfps %dk",
				id, p.Width, p.Height, p.FPS, p.VideoKbps, w.W, w.H, w.FPS, w.Kbps)
		}
	}
}

func TestSelectableExcludesAutoOnly(t *testing.T) {
	for _, p := range Selectable() {
		if p.AutoOnly {
			t.Errorf("Selectable() returned auto-only preset %q", p.ID)
		}
	}
	emergency, ok := ByID("emergency")
	if !ok || !emergency.AutoOnly {
		t.Error("emergency preset should be marked AutoOnly")
	}
}
