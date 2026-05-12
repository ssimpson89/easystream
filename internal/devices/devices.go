package devices

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
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

	mu       sync.Mutex
	cache    *DeviceList
	cachedAt time.Time
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
//
// Anything that registers as a v4l2 or AVFoundation device works for
// capture regardless of how we group it in the UI — this classification
// only affects which dropdown group the device shows up in. The keyword
// list below covers the SDI/HDMI capture cards we expect to encounter
// (UVC-class devices that show up as a normal capture endpoint, not
// the BlackMagic DeckLink dedicated backend).
func classifyVideoDevice(d Device) DeviceType {
	if d.Backend == "decklink" {
		return TypeSDI
	}
	name := strings.ToLower(d.Name)

	// Screen capture detection. "Capture screen 0" is the canonical
	// AVFoundation screen-capture name. Don't match "display" — Apple's
	// Studio Display Camera contains that substring but is a camera.
	if strings.Contains(name, "screen") {
		return TypeScreen
	}

	// SDI / HDMI capture cards that present as UVC (v4l2 / AVFoundation /
	// dshow). Grouped together under "Capture cards" in the UI.
	captureKeywords := []string{
		"cam link", "camlink",
		"elgato",
		"hd60", "hd 60",
		"avermedia", "live gamer",
		"magewell",
		"inogeni", // SDI / HDMI USB converters
		"aja",     // U-TAP HDMI/SDI USB
		"yuan",    // SDI capture cards
		"datavideo",
		"deltacast",
		"epiphan",
		"capture", // generic "capture" devices (not "screen capture")
		"hdmi",
		"sdi", // explicit SDI keyword catches anything self-identifying
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

// probeV4L2Devices enumerates capture devices via the Linux kernel's
// sysfs view at /sys/class/video4linux/. This is the canonical source:
//   - works without v4l2-ctl installed (Fedora minimal, NixOS without
//     v4l-utils, stripped containers)
//   - covers all videoN nodes regardless of number (not capped at video9)
//   - the name file is the kernel-reported model string, which is the
//     same one v4l2-ctl prints. Stable across reboots for a given
//     piece of hardware.
//
// Falls back to v4l2-ctl --list-devices then to /dev probing if sysfs
// isn't available (running inside a container without /sys, etc.).
func probeV4L2Devices() []Device {
	if devs := probeV4L2FromSysfs(); len(devs) > 0 {
		return devs
	}
	cmd := exec.Command("v4l2-ctl", "--list-devices")
	if out, err := cmd.Output(); err == nil {
		if devs := parseV4L2Ctl(string(out)); len(devs) > 0 {
			return devs
		}
	}
	// Last-resort scan of /dev/video* using os.Stat. No subprocess.
	var devices []Device
	for i := 0; i < 64; i++ {
		path := fmt.Sprintf("/dev/video%d", i)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		devices = append(devices, Device{
			Index:   path,
			Name:    fmt.Sprintf("Video Device %d", i),
			Kind:    "video",
			Backend: "v4l2",
		})
	}
	return devices
}

// probeV4L2FromSysfs reads /sys/class/video4linux/*/name. Each entry
// is a kernel-reported model string. We group entries by name and
// keep only the lowest-numbered videoN per name — capture cards like
// Magewell expose 2-4 nodes per board, only the first of which is the
// actual capture surface. This filter prevents the picker from
// showing 4 identical entries (3 of which would fail at start).
func probeV4L2FromSysfs() []Device {
	entries, err := os.ReadDir("/sys/class/video4linux")
	if err != nil {
		return nil
	}
	type entry struct {
		path string
		name string
		num  int
	}
	var found []entry
	for _, e := range entries {
		nm := e.Name()
		if !strings.HasPrefix(nm, "video") {
			continue
		}
		num, err := strconv.Atoi(strings.TrimPrefix(nm, "video"))
		if err != nil {
			continue
		}
		nameBytes, err := os.ReadFile("/sys/class/video4linux/" + nm + "/name")
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(nameBytes))
		if name == "" {
			name = fmt.Sprintf("Video Device %d", num)
		}
		found = append(found, entry{
			path: "/dev/" + nm,
			name: name,
			num:  num,
		})
	}
	// Sort by num so the lowest-numbered node per name wins.
	sort.Slice(found, func(i, j int) bool { return found[i].num < found[j].num })
	seen := map[string]bool{}
	var devices []Device
	for _, f := range found {
		// First node for this name wins; later sub-device nodes
		// (capture-card metadata streams) get filtered out.
		key := f.name
		if seen[key] {
			continue
		}
		seen[key] = true
		devices = append(devices, Device{
			Index:   f.path,
			Name:    f.name,
			Kind:    "video",
			Backend: "v4l2",
		})
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
