package quality

import "testing"

func TestPresetsHaveExpectedBandwidthTiers(t *testing.T) {
	if len(Presets) < 6 {
		t.Fatalf("expected at least 6 quality presets, got %d", len(Presets))
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
