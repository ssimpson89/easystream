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

func (p Preset) BufferSize() string {
	return fmt.Sprintf("%dk", p.VideoKbps*2)
}

var Presets = []Preset{
	{
		ID:           "emergency",
		Name:         "Emergency",
		Description:  "Keeps the stream online when upload bandwidth is severely degraded.",
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
		Name:         "Low",
		Description:  "Good fallback for weak campus upload links.",
		Width:        1280,
		Height:       720,
		FPS:          30,
		VideoKbps:    3000,
		AudioKbps:    128,
		UploadTarget: "5 Mbps or better",
	},
	{
		ID:           "standard",
		Name:         "Standard",
		Description:  "Balanced 720p stream for reliable service coverage.",
		Width:        1280,
		Height:       720,
		FPS:          30,
		VideoKbps:    4500,
		AudioKbps:    128,
		UploadTarget: "7 Mbps or better",
	},
	{
		ID:           "recommended",
		Name:         "Recommended",
		Description:  "Default 1080p stream for most campuses.",
		Width:        1920,
		Height:       1080,
		FPS:          30,
		VideoKbps:    8000,
		AudioKbps:    128,
		UploadTarget: "12 Mbps or better",
	},
	{
		ID:           "high",
		Name:         "High",
		Description:  "Sharper 1080p image for stable links.",
		Width:        1920,
		Height:       1080,
		FPS:          30,
		VideoKbps:    10000,
		AudioKbps:    160,
		UploadTarget: "15 Mbps or better",
	},
	{
		ID:           "high-motion",
		Name:         "High Motion",
		Description:  "1080p60 for fast motion when bandwidth is consistently strong.",
		Width:        1920,
		Height:       1080,
		FPS:          60,
		VideoKbps:    12000,
		AudioKbps:    160,
		UploadTarget: "18 Mbps or better",
	},
}

func Default() Preset {
	return Presets[3]
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
