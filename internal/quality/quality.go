package quality

import "fmt"

type Preset struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	FPS          int    `json:"fps"`
	VideoKbps    int    `json:"videoKbps"`
	AudioKbps    int    `json:"audioKbps"`
	UploadTarget string `json:"uploadTarget"`
	// AutoOnly hides this preset from the user dropdown. It can still be
	// selected internally by the adaptive quality controller.
	AutoOnly bool `json:"autoOnly,omitempty"`
}

func (p Preset) Resolution() string {
	return fmt.Sprintf("%dx%d", p.Width, p.Height)
}

func (p Preset) GOP() int {
	return p.FPS * 2
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

// Presets are aligned with YouTube's recommended H.264 bitrate settings.
// See: https://support.google.com/youtube/answer/2853702 (encoder settings).
var Presets = []Preset{
	{
		ID:           "emergency",
		Name:         "Emergency",
		Description:  "Last-resort fallback when upload bandwidth is severely degraded. Below YouTube's recommended minimum.",
		Width:        854,
		Height:       480,
		FPS:          30,
		VideoKbps:    1500,
		AudioKbps:    96,
		UploadTarget: "2.5 Mbps or better",
		AutoOnly:     true,
	},
	{
		ID:           "low",
		Name:         "Low (720p)",
		Description:  "720p at 30fps. YouTube-recommended bitrate for weaker upload links.",
		Width:        1280,
		Height:       720,
		FPS:          30,
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
		FPS:          60,
		VideoKbps:    6000,
		AudioKbps:    160,
		UploadTarget: "9 Mbps or better",
	},
	{
		// Lower-bitrate 1080p30 for destinations that cap below
		// YouTube's recommended 10 Mbps. Below YouTube's published
		// guidance but produces fine 1080p30 quality.
		ID:           "balanced",
		Name:         "Balanced (1080p)",
		Description:  "1080p at 30fps at a CDN-friendly 7 Mbps bitrate. Useful for destinations that cap below YouTube's recommended 10 Mbps.",
		Width:        1920,
		Height:       1080,
		FPS:          30,
		VideoKbps:    7000,
		AudioKbps:    160,
		UploadTarget: "10 Mbps or better",
	},
	{
		ID:           "recommended",
		Name:         "Recommended (1080p)",
		Description:  "1080p at 30fps. YouTube's recommended setting for most live streams.",
		Width:        1920,
		Height:       1080,
		FPS:          30,
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
		FPS:          60,
		VideoKbps:    12000,
		AudioKbps:    160,
		UploadTarget: "16 Mbps or better",
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

// LowerTier returns the next-lower-bitrate preset, or empty if at the bottom.
func LowerTier(id string) (Preset, bool) {
	idx := IndexOf(id)
	if idx <= 0 {
		return Preset{}, false
	}
	return Presets[idx-1], true
}

// HigherTier returns the next-higher-bitrate preset that is not AutoOnly,
// or empty if already at the top. Capped at the original target tier.
func HigherTier(currentID, capID string) (Preset, bool) {
	idx := IndexOf(currentID)
	cap := IndexOf(capID)
	if idx < 0 || cap < 0 || idx >= cap {
		return Preset{}, false
	}
	next := Presets[idx+1]
	if next.AutoOnly {
		return Preset{}, false
	}
	return next, true
}
