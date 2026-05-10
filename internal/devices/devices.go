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

// Device represents a detected capture device.
type Device struct {
	Index   string `json:"index"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`    // "video" or "audio"
	Backend string `json:"backend"` // "avfoundation", "dshow", "v4l2", "decklink"
}

// DeviceList is the result of a scan.
type DeviceList struct {
	Video     []Device  `json:"video"`
	Audio     []Device  `json:"audio"`
	Backend   string    `json:"backend"`
	ScannedAt time.Time `json:"scannedAt"`
}

// Scanner discovers available capture devices by calling FFmpeg.
type Scanner struct {
	binary string

	mu    sync.Mutex
	cache map[string]*cachedScan // keyed by backend
}

type cachedScan struct {
	list DeviceList
	at   time.Time
}

// NewScanner creates a device scanner.
func NewScanner(ffmpegBinary string) *Scanner {
	if ffmpegBinary == "" {
		ffmpegBinary = "ffmpeg"
	}
	return &Scanner{
		binary: ffmpegBinary,
		cache:  make(map[string]*cachedScan),
	}
}

// Scan returns devices for the given backend. Uses a 5-second cache.
// If backend is empty, it auto-detects based on the OS.
func (s *Scanner) Scan(backend string) DeviceList {
	if backend == "" {
		backend = PlatformBackend()
	}

	s.mu.Lock()
	if c, ok := s.cache[backend]; ok && time.Since(c.at) < 5*time.Second {
		list := c.list
		s.mu.Unlock()
		return list
	}
	s.mu.Unlock()

	list := s.scanBackend(backend)

	s.mu.Lock()
	s.cache[backend] = &cachedScan{list: list, at: time.Now()}
	s.mu.Unlock()
	return list
}

// Invalidate clears the cache so the next Scan does a fresh probe.
func (s *Scanner) Invalidate() {
	s.mu.Lock()
	s.cache = make(map[string]*cachedScan)
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

func (s *Scanner) scanBackend(backend string) DeviceList {
	switch backend {
	case "avfoundation":
		return s.scanAVFoundation()
	case "dshow":
		return s.scanDshow()
	case "v4l2":
		return s.scanV4L2()
	case "decklink":
		return s.scanDeckLink()
	default:
		return DeviceList{Backend: backend, ScannedAt: time.Now().UTC()}
	}
}

func (s *Scanner) scanAVFoundation() DeviceList {
	output := s.runFFmpeg("-f", "avfoundation", "-list_devices", "true", "-i", "")
	return parseAVFoundation(output)
}

func (s *Scanner) scanDshow() DeviceList {
	output := s.runFFmpeg("-f", "dshow", "-list_devices", "true", "-i", "dummy")
	return parseDshow(output)
}

func (s *Scanner) scanV4L2() DeviceList {
	list := DeviceList{Backend: "v4l2", ScannedAt: time.Now().UTC()}
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
	list := DeviceList{Backend: "avfoundation", ScannedAt: time.Now().UTC()}
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
	list := DeviceList{Backend: "dshow", ScannedAt: time.Now().UTC()}
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
// FFmpeg outputs lines like:
//   [decklink @ 0x...] 'DeckLink Mini Recorder 4K'
//   [decklink @ 0x...] 'DeckLink SDI'
var deckLinkDeviceRe = regexp.MustCompile(`\[decklink[^]]*\]\s+'([^']+)'`)

func parseDeckLink(output string) DeviceList {
	list := DeviceList{Backend: "decklink", ScannedAt: time.Now().UTC()}
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		matches := deckLinkDeviceRe.FindStringSubmatch(scanner.Text())
		if len(matches) < 2 {
			continue
		}
		name := strings.TrimSpace(matches[1])
		list.Video = append(list.Video, Device{
			Index:   name,
			Name:    name,
			Kind:    "video",
			Backend: "decklink",
		})
		// DeckLink audio is embedded in the SDI signal.
		list.Audio = append(list.Audio, Device{
			Index:   name,
			Name:    name + " (embedded audio)",
			Kind:    "audio",
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
