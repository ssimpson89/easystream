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
	Index   string `json:"index"`   // "0", "1", "/dev/video0", etc.
	Name    string `json:"name"`    // Human-readable name
	Kind    string `json:"kind"`    // "video" or "audio"
	Backend string `json:"backend"` // "avfoundation", "dshow", "v4l2"
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

	mu     sync.Mutex
	cache  *DeviceList
	lastAt time.Time
}

// NewScanner creates a device scanner.
func NewScanner(ffmpegBinary string) *Scanner {
	if ffmpegBinary == "" {
		ffmpegBinary = "ffmpeg"
	}
	return &Scanner{binary: ffmpegBinary}
}

// Scan returns the current device list, using a short cache to avoid
// hammering FFmpeg on every poll. The cache TTL is 5 seconds so new
// devices are picked up quickly after plugging in.
func (s *Scanner) Scan() DeviceList {
	s.mu.Lock()
	if s.cache != nil && time.Since(s.lastAt) < 5*time.Second {
		cached := *s.cache
		s.mu.Unlock()
		return cached
	}
	s.mu.Unlock()

	list := s.scan()

	s.mu.Lock()
	s.cache = &list
	s.lastAt = time.Now()
	s.mu.Unlock()
	return list
}

// Invalidate clears the cache so the next Scan() does a fresh probe.
func (s *Scanner) Invalidate() {
	s.mu.Lock()
	s.cache = nil
	s.mu.Unlock()
}

func (s *Scanner) scan() DeviceList {
	switch runtime.GOOS {
	case "darwin":
		return s.scanAVFoundation()
	case "windows":
		return s.scanDshow()
	default:
		return s.scanV4L2()
	}
}

// macOS: ffmpeg -f avfoundation -list_devices true -i ""
func (s *Scanner) scanAVFoundation() DeviceList {
	output := s.runFFmpeg("-f", "avfoundation", "-list_devices", "true", "-i", "")
	return parseAVFoundation(output)
}

// Windows: ffmpeg -f dshow -list_devices true -i dummy
func (s *Scanner) scanDshow() DeviceList {
	output := s.runFFmpeg("-f", "dshow", "-list_devices", "true", "-i", "dummy")
	return parseDshow(output)
}

// Linux: enumerate /dev/video* and query names via v4l2-ctl
func (s *Scanner) scanV4L2() DeviceList {
	output := s.runFFmpeg("-f", "v4l2", "-list_devices", "true", "-i", "/dev/video0")
	list := parseV4L2(output)
	// Fallback: try to list /dev/video* files
	if len(list.Video) == 0 {
		list.Video = probeV4L2Devices()
	}
	list.Backend = "v4l2"
	list.ScannedAt = time.Now().UTC()
	return list
}

func (s *Scanner) runFFmpeg(args ...string) string {
	fullArgs := append([]string{"-hide_banner", "-nostdin", "-loglevel", "info"}, args...)
	cmd := exec.Command(s.binary, fullArgs...)
	// FFmpeg writes device lists to stderr.
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// avfoundation parser. Lines look like:
// [AVFoundation indev @ 0x...] AVFoundation video devices:
// [AVFoundation indev @ 0x...] [0] FaceTime HD Camera
// [AVFoundation indev @ 0x...] AVFoundation audio devices:
// [AVFoundation indev @ 0x...] [0] MacBook Air Microphone
var avfDeviceRe = regexp.MustCompile(`\[(\d+)\]\s+(.+)`)

func parseAVFoundation(output string) DeviceList {
	list := DeviceList{Backend: "avfoundation", ScannedAt: time.Now().UTC()}
	scanner := bufio.NewScanner(strings.NewReader(output))
	section := "" // "video" or "audio"
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

// dshow parser. Lines look like:
// [dshow @ 0x...] "HD Webcam" (video)
// [dshow @ 0x...] "Microphone (HD Webcam)" (audio)
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
			Index:   matches[1], // dshow uses names as identifiers
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

// v4l2 parser — try to find device names from output.
func parseV4L2(output string) DeviceList {
	list := DeviceList{Backend: "v4l2", ScannedAt: time.Now().UTC()}
	// v4l2 device listing format varies; just probe /dev/video*.
	return list
}

func probeV4L2Devices() []Device {
	// Try v4l2-ctl --list-devices first.
	cmd := exec.Command("v4l2-ctl", "--list-devices")
	out, err := cmd.Output()
	if err == nil {
		return parseV4L2Ctl(string(out))
	}
	// Fallback: look for /dev/video* files.
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

// Parse output from v4l2-ctl --list-devices:
//
//	HD Webcam (usb-0000:00:14.0-1):
//		/dev/video0
//		/dev/video1
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
			// Device name line (strip trailing colon and parens).
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
