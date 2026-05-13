package quality

import (
	"fmt"
	"math"
)

type Preset struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	// FPSNum / FPSDen express the frame rate as a rational number so
	// 23.976 (cinema cadence) can be represented exactly as 24000/1001
	// rather than rounded to 24. Most presets use integer rates
	// (FPSDen=1), but cinema cameras shoot true NTSC-cinema 23.976 and
	// the encoder needs an exact ratio to keep PTS perfectly cadenced
	// over a service-length stream. Use the FPS() / FPSExpr() helpers
	// instead of these fields directly so callers don't fall into
	// rounding traps.
	FPSNum int `json:"fpsNum"`
	FPSDen int `json:"fpsDen"`
	// H264Level is the H.264 profile-level emitted in the SPS. The
	// macroblock-rate cap is per-level: 1080p30 fits in 4.1 (245.76 k
	// MB/s); 1080p60 requires 4.2 (522 k MB/s); 1440p24 fits in 5.0
	// (589 k MB/s) and 1440p60 would need 5.1. Encoders happily emit
	// a smaller value than the spec allows, but downstream hardware
	// decoders reject non-conformant SPS, so we let each preset
	// declare its own and the encoder paths honor it.
	H264Level string `json:"h264Level"`
	VideoKbps int    `json:"videoKbps"`
	AudioKbps int    `json:"audioKbps"`
	UploadTarget string `json:"uploadTarget"`
	// AutoOnly hides this preset from the user dropdown. It can still be
	// selected internally by the adaptive quality controller.
	AutoOnly bool `json:"autoOnly,omitempty"`
	// ExcludeFromAdaptive opts the preset out of the adaptive-bitrate
	// ladder so the controller never steps INTO or OUT OF it
	// mid-stream. Cinema presets set this: a framerate flip mid-service
	// (24p → 30p) is more visually jarring than any bitrate cut, and
	// the operator who picked 24p chose cadence over robustness.
	ExcludeFromAdaptive bool `json:"excludeFromAdaptive,omitempty"`
}

func (p Preset) Resolution() string {
	return fmt.Sprintf("%dx%d", p.Width, p.Height)
}

// FPS returns the preset's frame rate as an integer, rounded from the
// rational FPSNum/FPSDen. Used where ffmpeg or AVFoundation only accept
// integer rates (the testsrc lavfi rate, the AVFoundation framerate
// probe). For everywhere the exact ratio matters (the encoder -r),
// use FPSExpr() instead.
func (p Preset) FPS() int {
	if p.FPSDen <= 0 {
		return p.FPSNum
	}
	return int(math.Round(float64(p.FPSNum) / float64(p.FPSDen)))
}

// FPSFloat returns the frame rate as a float64. Use this when the
// consumer compares against floating-point values (e.g. the
// AVFoundation mode probe, which needs to distinguish 23.976 from
// 24.000 — comparing rounded integers ties at zero distance and the
// probe picks the wrong mode). FPSFloat handles FPSDen<=0 the same
// way FPS() does, so legacy presets that only set FPSNum still work.
func (p Preset) FPSFloat() float64 {
	if p.FPSDen <= 0 {
		return float64(p.FPSNum)
	}
	return float64(p.FPSNum) / float64(p.FPSDen)
}

// FPSExpr returns the frame rate as a string ffmpeg accepts on -r.
// Integer rates render as plain decimals; fractional rates render
// as "num/den" (e.g. "24000/1001" for 23.976). The fractional form
// preserves NTSC-cinema cadence exactly through long streams without
// PTS drift.
func (p Preset) FPSExpr() string {
	if p.FPSDen <= 1 {
		return fmt.Sprintf("%d", p.FPSNum)
	}
	return fmt.Sprintf("%d/%d", p.FPSNum, p.FPSDen)
}

// GOP is the keyframe interval in frames, sized for a 2-second GOP
// (YouTube-recommended for live). Rounds to the nearest integer frame
// count so 23.976 produces 48 (which is ~2.003 s — within YouTube's
// ≤4 s spec) and 30 produces 60.
func (p Preset) GOP() int {
	if p.FPSDen <= 0 {
		return p.FPSNum * 2
	}
	return int(math.Round(2.0 * float64(p.FPSNum) / float64(p.FPSDen)))
}

func (p Preset) VideoBitrate() string {
	return fmt.Sprintf("%dk", p.VideoKbps)
}

func (p Preset) AudioBitrate() string {
	return fmt.Sprintf("%dk", p.AudioKbps)
}

// BufferSize is the VBV buffer (-bufsize). Industry practice for true
// CBR live streaming is bufsize == maxrate (1-second VBV) so the
// encoder maintains a constant rate without burst headroom that masks
// network congestion. A larger buffer lets the encoder save up bits
// during easy scenes and burst on hard ones — fine for VOD, bad for
// live where receivers expect a steady stream.
func (p Preset) BufferSize() string {
	return fmt.Sprintf("%dk", p.VideoKbps)
}

// Level returns the H.264 profile-level for the SPS. Falls back to
// "4.1" when a preset doesn't declare one, preserving the historical
// default for legacy callers.
func (p Preset) Level() string {
	if p.H264Level == "" {
		return "4.1"
	}
	return p.H264Level
}

// Presets are aligned with YouTube's recommended H.264 bitrate
// settings: https://support.google.com/youtube/answer/2853702
//
// Bitrate guidance by framerate band (SDR):
//
//	720p  24-30 fps:  2.5 -  5 Mbps   /  720p 48-60:   3.5 -  7
//	1080p 24-30 fps:  4.5 -  9 Mbps   / 1080p 48-60:    6   - 12
//	1440p 24-30 fps:    9 - 18 Mbps   / 1440p 48-60:   12   - 24
//
// The cinema-* presets sit toward the upper end of the 24-30 band so
// the cinema cadence has enough bitrate for film grain / shallow DOF
// motion without burning operator headroom for nothing.
var Presets = []Preset{
	{
		ID:           "emergency",
		Name:         "Emergency (480p30)",
		Description:  "Last-resort fallback when upload bandwidth is severely degraded. Below YouTube's recommended minimum.",
		Width:        854,
		Height:       480,
		FPSNum:       30,
		FPSDen:       1,
		H264Level:    "3.1",
		VideoKbps:    1500,
		AudioKbps:    96,
		UploadTarget: "2.5 Mbps or better",
		AutoOnly:     true,
	},
	{
		ID:           "low",
		Name:         "Low (720p30)",
		Description:  "720p at 30fps. YouTube-recommended bitrate for weaker upload links.",
		Width:        1280,
		Height:       720,
		FPSNum:       30,
		FPSDen:       1,
		H264Level:    "4.0",
		VideoKbps:    4000,
		AudioKbps:    160,
		UploadTarget: "6 Mbps or better",
	},
	{
		ID:           "standard",
		Name:         "Standard (720p60)",
		Description:  "720p at 60fps. Smoother motion for worship music or movement.",
		Width:        1280,
		Height:       720,
		FPSNum:       60,
		FPSDen:       1,
		H264Level:    "4.1",
		VideoKbps:    6000,
		AudioKbps:    160,
		UploadTarget: "9 Mbps or better",
	},
	{
		// Lower-bitrate 1080p30 for destinations that cap below
		// YouTube's recommended 10 Mbps. Below YouTube's published
		// guidance but produces fine 1080p30 quality.
		ID:           "balanced",
		Name:         "Balanced (1080p30)",
		Description:  "1080p at 30fps at a CDN-friendly 7 Mbps bitrate. Useful for destinations that cap below YouTube's recommended 10 Mbps.",
		Width:        1920,
		Height:       1080,
		FPSNum:       30,
		FPSDen:       1,
		H264Level:    "4.1",
		VideoKbps:    7000,
		AudioKbps:    160,
		UploadTarget: "10 Mbps or better",
	},
	{
		ID:           "recommended",
		Name:         "Recommended (1080p30)",
		Description:  "1080p at 30fps. YouTube's recommended setting for most live streams.",
		Width:        1920,
		Height:       1080,
		FPSNum:       30,
		FPSDen:       1,
		H264Level:    "4.1",
		VideoKbps:    10000,
		AudioKbps:    160,
		UploadTarget: "13 Mbps or better",
	},
	{
		ID:           "high",
		Name:         "High (1080p60)",
		Description:  "1080p at 60fps. YouTube-recommended bitrate for fast motion when bandwidth is strong.",
		Width:        1920,
		Height:       1080,
		FPSNum:       60,
		FPSDen:       1,
		// 1080p60 exceeds Level 4.1's macroblock-rate cap (245.76 k
		// MB/s); 4.2 caps at 522 k which covers it.
		H264Level:    "4.2",
		VideoKbps:    12000,
		AudioKbps:    160,
		UploadTarget: "16 Mbps or better",
	},

	// Cinema-cadence presets. These ship 23.976 fps (NTSC cinema)
	// exactly via the 24000/1001 ratio so the cadence makes it all
	// the way to YouTube without rounding into 24.000 (which drifts
	// ~3.6 seconds per hour and forces the muxer to drop a frame).
	// All three opt out of the adaptive ladder — a framerate flip
	// mid-service is more visually disruptive than a bitrate cut,
	// and the operator who picked 24p chose cadence over robustness.
	{
		ID:                  "cinema-720p24",
		Name:                "Cinema 720p (24p)",
		Description:         "720p at 23.976fps for cinema-camera sources (Sony FX3, Pocket 6K, etc.). Preserves film cadence end-to-end.",
		Width:               1280,
		Height:              720,
		FPSNum:              24000,
		FPSDen:              1001,
		H264Level:           "3.2",
		VideoKbps:           4000,
		AudioKbps:           160,
		UploadTarget:        "6 Mbps or better",
		ExcludeFromAdaptive: true,
	},
	{
		ID:                  "cinema-1080p24",
		Name:                "Cinema 1080p (24p)",
		Description:         "1080p at 23.976fps for cinema-camera sources. The standard cinematic-look preset.",
		Width:               1920,
		Height:              1080,
		FPSNum:              24000,
		FPSDen:              1001,
		H264Level:           "4.0",
		VideoKbps:           8000,
		AudioKbps:           160,
		UploadTarget:        "11 Mbps or better",
		ExcludeFromAdaptive: true,
	},
	{
		ID:                  "cinema-1440p24",
		Name:                "Cinema 1440p (24p)",
		Description:         "1440p at 23.976fps for high-end cinema setups. Needs strong upload bandwidth.",
		Width:               2560,
		Height:              1440,
		FPSNum:              24000,
		FPSDen:              1001,
		H264Level:           "5.0",
		VideoKbps:           14000,
		AudioKbps:           160,
		UploadTarget:        "18 Mbps or better",
		ExcludeFromAdaptive: true,
	},
}

func Default() Preset {
	p, _ := ByID("recommended")
	return p
}

func ByID(id string) (Preset, bool) {
	for _, preset := range Presets {
		if preset.ID == id {
			return preset, true
		}
	}
	return Preset{}, false
}

// Selectable returns presets shown to the user (excludes auto-only fallbacks).
func Selectable() []Preset {
	out := make([]Preset, 0, len(Presets))
	for _, p := range Presets {
		if !p.AutoOnly {
			out = append(out, p)
		}
	}
	return out
}

// IndexOf returns the position of id in Presets, or -1 if not found.
func IndexOf(id string) int {
	for i, p := range Presets {
		if p.ID == id {
			return i
		}
	}
	return -1
}

// LowerTier returns the next-lower-bitrate preset for the adaptive
// controller, or empty if at the bottom. Cinema presets are
// invisible to the ladder in both directions (ExcludeFromAdaptive),
// and a non-cinema starting preset never crosses into one.
func LowerTier(id string) (Preset, bool) {
	idx := IndexOf(id)
	if idx <= 0 {
		return Preset{}, false
	}
	start := Presets[idx]
	for i := idx - 1; i >= 0; i-- {
		next := Presets[i]
		if next.ExcludeFromAdaptive || start.ExcludeFromAdaptive {
			// Cinema preset (or stepping from a cinema preset) — both
			// directions are disabled by design.
			return Preset{}, false
		}
		return next, true
	}
	return Preset{}, false
}

// HigherTier returns the next-higher-bitrate preset that is not
// AutoOnly and not ExcludeFromAdaptive, or empty if already at the
// top. Capped at the original target tier.
func HigherTier(currentID, capID string) (Preset, bool) {
	idx := IndexOf(currentID)
	cap := IndexOf(capID)
	if idx < 0 || cap < 0 || idx >= cap {
		return Preset{}, false
	}
	start := Presets[idx]
	if start.ExcludeFromAdaptive {
		return Preset{}, false
	}
	for i := idx + 1; i <= cap; i++ {
		next := Presets[i]
		if next.AutoOnly || next.ExcludeFromAdaptive {
			continue
		}
		return next, true
	}
	return Preset{}, false
}
