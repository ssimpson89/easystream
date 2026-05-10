package devices

import (
	"bufio"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// DeviceType categorizes a device for UI grouping.
type DeviceType string

const (
	TypeCamera      DeviceType = "camera"       // Webcams, FaceTime, iPhone Continuity
	TypeCaptureCard DeviceType = "capture-card" // USB HDMI capture (Elgato, Cam Link, etc.)
	TypeScreen      DeviceType = "screen"       // Screen capture
	TypeSDI         DeviceType = "sdi"          // Blackmagic DeckLink
	TypeMicrophone  DeviceType = "microphone"   // Built-in or USB microphones
	TypeAudioInput  DeviceType = "audio-input"  // Generic audio (line-in, NDI, virtual)
	TypeOther       DeviceType = "other"
)

// Device represents a detected capture device.
type Device struct {
	Index   string     `json:"index"`
	Name    string     `json:"name"`
	Kind    string     `json:"kind"`    // "video" or "audio"
	Type    DeviceType `json:"type"`    // category for UI grouping
	Backend string     `json:"backend"` // FFmpeg backend: avfoundation, dshow, v4l2, decklink
}

// DeviceList is a unified categorized list of all available capture devices.
type DeviceList struct {
	Video     []Device  `json:"video"`
	Audio     []Device  `json:"audio"`
	Platform  string    `json:"platform"` // platform backend (avfoundation/dshow/v4l2)
	ScannedAt time.Time `json:"scannedAt"`
}

// Scanner discovers available capture devices by calling FFmpeg.
type Scanner struct {
	binary string

	mu        sync.Mutex
	cache     *DeviceList
	cachedAt  time.Time
}

// NewScanner creates a device scanner.
func NewScanner(ffmpegBinary string) *Scanner {
	if ffmpegBinary == "" {
		ffmpegBinary = "ffmpeg"
	}
	return &Scanner{binary: ffmpegBinary}
}

// Scan returns a unified, categorized list of all detected devices across
// every supported backend. Cached for 5 seconds.
func (s *Scanner) Scan() DeviceList {
	s.mu.Lock()
	if s.cache != nil && time.Since(s.cachedAt) < 5*time.Second {
		list := *s.cache
		s.mu.Unlock()
		return list
	}
	s.mu.Unlock()

	list := s.scanAll()

	s.mu.Lock()
	s.cache = &list
	s.cachedAt = time.Now()
	s.mu.Unlock()
	return list
}

// Invalidate clears the cache so the next Scan does a fresh probe.
func (s *Scanner) Invalidate() {
	s.mu.Lock()
	s.cache = nil
	s.mu.Unlock()
}

// PlatformBackend returns the default capture backend for the current OS.
func PlatformBackend() string {
	switch runtime.GOOS {
	case "darwin":
		return "avfoundation"
	case "windows":
		return "dshow"
	default:
		return "v4l2"
	}
}

// scanAll runs every supported backend probe and merges the results into
// one categorized list.
func (s *Scanner) scanAll() DeviceList {
	list := DeviceList{
		Platform:  PlatformBackend(),
		ScannedAt: time.Now().UTC(),
	}

	// Platform devices.
	switch runtime.GOOS {
	case "darwin":
		platform := s.scanAVFoundation()
		list.Video = append(list.Video, platform.Video...)
		list.Audio = append(list.Audio, platform.Audio...)
	case "windows":
		platform := s.scanDshow()
		list.Video = append(list.Video, platform.Video...)
		list.Audio = append(list.Audio, platform.Audio...)
	default:
		platform := s.scanV4L2()
		list.Video = append(list.Video, platform.Video...)
		list.Audio = append(list.Audio, platform.Audio...)
	}

	// DeckLink devices (cross-platform). Only include if found.
	dl := s.scanDeckLink()
	list.Video = append(list.Video, dl.Video...)
	list.Audio = append(list.Audio, dl.Audio...)

	// Categorize each device.
	for i := range list.Video {
		list.Video[i].Type = classifyVideoDevice(list.Video[i])
	}
	for i := range list.Audio {
		list.Audio[i].Type = classifyAudioDevice(list.Audio[i])
	}

	return list
}

// classifyVideoDevice categorizes a video device based on backend and name.
func classifyVideoDevice(d Device) DeviceType {
	if d.Backend == "decklink" {
		return TypeSDI
	}
	name := strings.ToLower(d.Name)

	// Screen capture detection (must come before camera since "screen" is unambiguous).
	if strings.Contains(name, "screen") || strings.Contains(name, "display") {
		return TypeScreen
	}

	// USB HDMI capture cards — known vendor/product names.
	captureKeywords := []string{
		"cam link", "camlink",
		"elgato",
		"hd60", "hd 60",
		"avermedia", "live gamer",
		"magewell",
		"epiphan",
		"capture", // generic "capture" devices (not "screen capture")
		"hdmi",
		"usb video",
	}
	for _, kw := range captureKeywords {
		if strings.Contains(name, kw) {
			return TypeCaptureCard
		}
	}

	// Default to camera (covers FaceTime, webcams, iPhone Continuity, "Desk View", etc.)
	return TypeCamera
}

// classifyAudioDevice categorizes an audio device.
func classifyAudioDevice(d Device) DeviceType {
	if d.Backend == "decklink" {
		return TypeSDI
	}
	name := strings.ToLower(d.Name)
	if strings.Contains(name, "microphone") || strings.Contains(name, "mic ") || strings.HasSuffix(name, " mic") {
		return TypeMicrophone
	}
	return TypeAudioInput
}

// --- Backend scanners ---

func (s *Scanner) scanAVFoundation() DeviceList {
	output := s.runFFmpeg("-f", "avfoundation", "-list_devices", "true", "-i", "")
	return parseAVFoundation(output)
}

func (s *Scanner) scanDshow() DeviceList {
	output := s.runFFmpeg("-f", "dshow", "-list_devices", "true", "-i", "dummy")
	return parseDshow(output)
}

func (s *Scanner) scanV4L2() DeviceList {
	list := DeviceList{Platform: "v4l2", ScannedAt: time.Now().UTC()}
	list.Video = probeV4L2Devices()
	return list
}

func (s *Scanner) scanDeckLink() DeviceList {
	output := s.runFFmpeg("-f", "decklink", "-list_devices", "1", "-i", "dummy")
	return parseDeckLink(output)
}

func (s *Scanner) runFFmpeg(args ...string) string {
	fullArgs := append([]string{"-hide_banner", "-nostdin", "-loglevel", "info"}, args...)
	cmd := exec.Command(s.binary, fullArgs...)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// --- avfoundation parser ---
var avfDeviceRe = regexp.MustCompile(`\[(\d+)\]\s+(.+)`)

func parseAVFoundation(output string) DeviceList {
	list := DeviceList{}
	scanner := bufio.NewScanner(strings.NewReader(output))
	section := ""
	for scanner.Scan() {
		line := scanner.Text()
		lower := strings.ToLower(line)
		if strings.Contains(lower, "video devices:") {
			section = "video"
			continue
		}
		if strings.Contains(lower, "audio devices:") {
			section = "audio"
			continue
		}
		if section == "" {
			continue
		}
		matches := avfDeviceRe.FindStringSubmatch(line)
		if len(matches) < 3 {
			continue
		}
		d := Device{
			Index:   matches[1],
			Name:    strings.TrimSpace(matches[2]),
			Kind:    section,
			Backend: "avfoundation",
		}
		if section == "video" {
			list.Video = append(list.Video, d)
		} else {
			list.Audio = append(list.Audio, d)
		}
	}
	return list
}

// --- dshow parser ---
var dshowDeviceRe = regexp.MustCompile(`"([^"]+)"\s+\((video|audio)\)`)

func parseDshow(output string) DeviceList {
	list := DeviceList{}
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		matches := dshowDeviceRe.FindStringSubmatch(scanner.Text())
		if len(matches) < 3 {
			continue
		}
		d := Device{
			Index:   matches[1],
			Name:    matches[1],
			Kind:    matches[2],
			Backend: "dshow",
		}
		if d.Kind == "video" {
			list.Video = append(list.Video, d)
		} else {
			list.Audio = append(list.Audio, d)
		}
	}
	return list
}

// --- decklink parser ---
var deckLinkDeviceRe = regexp.MustCompile(`\[decklink[^]]*\]\s+'([^']+)'`)

func parseDeckLink(output string) DeviceList {
	list := DeviceList{}
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		matches := deckLinkDeviceRe.FindStringSubmatch(scanner.Text())
		if len(matches) < 2 {
			continue
		}
		name := strings.TrimSpace(matches[1])
		// DeckLink devices appear once but provide both video and embedded audio.
		// We expose them as a video device with embedded audio implicit.
		list.Video = append(list.Video, Device{
			Index:   name,
			Name:    name,
			Kind:    "video",
			Backend: "decklink",
		})
	}
	return list
}

// --- v4l2 ---
func probeV4L2Devices() []Device {
	cmd := exec.Command("v4l2-ctl", "--list-devices")
	out, err := cmd.Output()
	if err == nil {
		return parseV4L2Ctl(string(out))
	}
	var devices []Device
	for i := 0; i < 10; i++ {
		path := fmt.Sprintf("/dev/video%d", i)
		cmd := exec.Command("test", "-e", path)
		if cmd.Run() == nil {
			devices = append(devices, Device{
				Index:   path,
				Name:    fmt.Sprintf("Video Device %d", i),
				Kind:    "video",
				Backend: "v4l2",
			})
		}
	}
	return devices
}

func parseV4L2Ctl(output string) []Device {
	var devices []Device
	scanner := bufio.NewScanner(strings.NewReader(output))
	currentName := ""
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			currentName = ""
			continue
		}
		if !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, " ") {
			currentName = strings.TrimRight(trimmed, ":")
			if idx := strings.Index(currentName, "("); idx > 0 {
				currentName = strings.TrimSpace(currentName[:idx])
			}
		} else if strings.HasPrefix(trimmed, "/dev/video") {
			devices = append(devices, Device{
				Index:   trimmed,
				Name:    currentName,
				Kind:    "video",
				Backend: "v4l2",
			})
		}
	}
	return devices
}
