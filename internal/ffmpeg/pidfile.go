package ffmpeg

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ssimpson89/easystream/internal/atomicfile"
)

// PidFile records the FFmpeg child PID on disk so we can detect EasyStream
// orphans after a crash or kill -9. We do not adopt those processes: once the
// parent dies their progress/audio/error pipes are gone, so startup reaps the
// orphan and starts a fresh, fully observable FFmpeg when live intent exists.
type PidFile struct {
	Path string
}

// Write atomically records the PID. Safe to call with Path="" (no-op).
func (p *PidFile) Write(pid int) error {
	if p == nil || p.Path == "" {
		return nil
	}
	return atomicfile.Write(p.Path, []byte(strconv.Itoa(pid)), 0600)
}

// Clear removes the PID file. Safe when no file exists.
func (p *PidFile) Clear() {
	if p == nil || p.Path == "" {
		return
	}
	_ = os.Remove(p.Path)
}

// ReapOrphan checks for a leftover FFmpeg process from a previous session.
// Returns the reaped PID (0 if nothing to reap) and any error.
//
// We verify the PID is actually ffmpeg before killing it — defensive in
// case PIDs got recycled to an unrelated process between EasyStream runs.
func (p *PidFile) ReapOrphan() (int, error) {
	if p == nil || p.Path == "" {
		return 0, nil
	}
	data, err := os.ReadFile(p.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		p.Clear()
		return 0, nil
	}

	proc, err := os.FindProcess(pid)
	if err != nil || proc == nil {
		p.Clear()
		return 0, nil
	}
	// Signal 0 = liveness probe without affecting the process.
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		// Already dead.
		p.Clear()
		return 0, nil
	}

	// Confirm it's an EasyStream-owned ffmpeg before killing. PIDs recycle, and
	// this machine may have unrelated FFmpeg jobs running.
	if !isEasyStreamFFmpegProcess(pid) {
		p.Clear()
		return 0, nil
	}

	// Graceful shutdown first, then force kill.
	_ = proc.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if proc.Signal(syscall.Signal(0)) != nil {
			p.Clear()
			return pid, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = proc.Kill()
	p.Clear()
	return pid, nil
}

// isEasyStreamFFmpegProcess checks whether a PID is an FFmpeg process started
// by EasyStream. It intentionally requires EasyStream-specific command-line
// markers, not just argv[0] == ffmpeg, so a stale PID cannot kill an unrelated
// user FFmpeg job after PID reuse.
func isEasyStreamFFmpegProcess(pid int) bool {
	line, err := processCommand(pid)
	if err != nil {
		return false
	}
	return isFFmpegCommandLine(line) && isEasyStreamCommandLine(line)
}

func processCommand(pid int) (string, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func isFFmpegCommandLine(line string) bool {
	if line == "" {
		return false
	}
	// `ps` returns the full command line; take the first whitespace-separated
	// field, which is argv[0], and check its basename.
	if idx := strings.IndexAny(line, " \t"); idx >= 0 {
		line = line[:idx]
	}
	base := line
	if idx := strings.LastIndex(line, "/"); idx >= 0 {
		base = line[idx+1:]
	}
	return strings.EqualFold(base, "ffmpeg")
}

func isEasyStreamCommandLine(line string) bool {
	markers := []string{
		"-progress pipe:1",
		"lavfi.astats.Overall.RMS_level",
		"rtp://127.0.0.1:52001",
		"rtp://127.0.0.1:52002",
	}
	for _, marker := range markers {
		if !strings.Contains(line, marker) {
			return false
		}
	}
	return true
}
