package quality

import "testing"

func TestPresetsHaveExpectedBandwidthTiers(t *testing.T) {
	if len(Presets) < 4 {
		t.Fatalf("expected at least 4 quality presets, got %d", len(Presets))
	}

	// Bitrate must increase monotonically across the *adaptive* ladder
	// (the presets the controller actually walks between). Cinema
	// presets are deliberately excluded from that ladder, so they can
	// sit anywhere in the slice without breaking the ordering check.
	last := 0
	for _, preset := range Presets {
		if preset.ExcludeFromAdaptive {
			continue
		}
		if preset.VideoKbps <= last {
			t.Fatalf("preset %q is not ordered by increasing video bitrate", preset.ID)
		}
		last = preset.VideoKbps
	}

	for _, preset := range Presets {
		if preset.GOP() != preset.FPS()*2 {
			// Tolerance: the 23.976 cinema presets round to GOP=48
			// and FPS=24, so 24*2 still equals 48. If a future
			// preset breaks this coincidence (e.g. 25.000), the
			// assertion should relax instead of failing.
			t.Fatalf("preset %q should use a 2-second GOP (got %d, FPS()*2=%d)",
				preset.ID, preset.GOP(), preset.FPS()*2)
		}
	}
}

// TestPresetsAlignWithYouTubeRecommendations verifies our bitrates match
// YouTube's published H.264 recommendations for each resolution/framerate.
// Source: https://support.google.com/youtube/answer/2853702
func TestPresetsAlignWithYouTubeRecommendations(t *testing.T) {
	want := map[string]struct {
		W, H, FPS, Kbps int
	}{
		"low":             {1280, 720, 30, 4000},   // YT 720p30: 4 Mbps
		"standard":        {1280, 720, 60, 6000},   // YT 720p60: 6 Mbps
		"recommended":     {1920, 1080, 30, 10000}, // YT 1080p30: 10 Mbps
		"high":            {1920, 1080, 60, 12000}, // YT 1080p60: 12 Mbps
		"cinema-720p24":   {1280, 720, 24, 4000},   // YT 720p24-30: 2.5-5 Mbps
		"cinema-1080p24":  {1920, 1080, 24, 8000},  // YT 1080p24-30: 4.5-9 Mbps
		"cinema-1440p24":  {2560, 1440, 24, 14000}, // YT 1440p24-30: 9-18 Mbps
	}
	for id, w := range want {
		p, ok := ByID(id)
		if !ok {
			t.Errorf("missing preset %q", id)
			continue
		}
		if p.Width != w.W || p.Height != w.H || p.FPS() != w.FPS || p.VideoKbps != w.Kbps {
			t.Errorf("preset %q = %dx%d@%dfps %dk, want %dx%d@%dfps %dk",
				id, p.Width, p.Height, p.FPS(), p.VideoKbps, w.W, w.H, w.FPS, w.Kbps)
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

func TestCinemaPresetsEmit23976FPS(t *testing.T) {
	// Cinema presets must encode NTSC-cinema 23.976 exactly via the
	// 24000/1001 fraction, not the rounded 24.000. The exact ratio
	// keeps cadence stable over service-length streams; 24.000 drifts
	// ~3.6 s per hour and forces the muxer to drop frames.
	for _, id := range []string{"cinema-720p24", "cinema-1080p24", "cinema-1440p24"} {
		p, ok := ByID(id)
		if !ok {
			t.Errorf("missing preset %q", id)
			continue
		}
		if p.FPSExpr() != "24000/1001" {
			t.Errorf("preset %q FPSExpr=%q, want \"24000/1001\"", id, p.FPSExpr())
		}
		if p.GOP() != 48 {
			t.Errorf("preset %q GOP=%d, want 48 (≈2 s at 23.976)", id, p.GOP())
		}
		if !p.ExcludeFromAdaptive {
			t.Errorf("preset %q must opt out of the adaptive ladder", id)
		}
	}
}

func TestNonCinemaPresetsEmitIntegerFPS(t *testing.T) {
	// Integer-rate presets must emit a plain integer (no slash) on -r
	// so ffmpeg doesn't accidentally treat them as fractions.
	for _, id := range []string{"low", "standard", "balanced", "recommended", "high"} {
		p, ok := ByID(id)
		if !ok {
			t.Errorf("missing preset %q", id)
			continue
		}
		expr := p.FPSExpr()
		// Should not contain a slash for integer rates.
		for _, c := range expr {
			if c == '/' {
				t.Errorf("preset %q FPSExpr=%q should be a plain integer", id, expr)
				break
			}
		}
	}
}

func TestAdaptiveLadderSkipsCinemaPresets(t *testing.T) {
	// Stepping down from a regular preset must not land on a cinema
	// preset (a framerate flip mid-stream is more jarring than a
	// bitrate cut). And stepping from a cinema preset is disabled
	// entirely — the operator chose cadence.
	for _, id := range []string{"low", "standard", "balanced", "recommended", "high"} {
		if lower, ok := LowerTier(id); ok && lower.ExcludeFromAdaptive {
			t.Errorf("LowerTier(%q) returned cinema preset %q — adaptive must skip these",
				id, lower.ID)
		}
	}
	for _, id := range []string{"cinema-720p24", "cinema-1080p24", "cinema-1440p24"} {
		if _, ok := LowerTier(id); ok {
			t.Errorf("LowerTier(%q) returned non-empty — cinema presets must be terminal", id)
		}
		if _, ok := HigherTier(id, "high"); ok {
			t.Errorf("HigherTier(%q,...) returned non-empty — cinema presets must be terminal", id)
		}
	}
}

func TestH264LevelMatchesResolutionAndFramerate(t *testing.T) {
	// Level encodes the SPS profile-level cap. Mismatched levels
	// cause strict hardware decoders to reject the stream.
	cases := map[string]string{
		"emergency":      "3.1",
		"low":            "4.0",
		"standard":       "4.1",
		"balanced":       "4.1",
		"recommended":    "4.1",
		"high":           "4.2", // 1080p60 exceeds 4.1's macroblock-rate cap
		"cinema-720p24":  "3.2",
		"cinema-1080p24": "4.0",
		"cinema-1440p24": "5.0",
	}
	for id, want := range cases {
		p, ok := ByID(id)
		if !ok {
			t.Errorf("missing preset %q", id)
			continue
		}
		if got := p.Level(); got != want {
			t.Errorf("preset %q level=%q, want %q", id, got, want)
		}
	}
}
