package ffmpeg

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// PidFile records the FFmpeg child PID on disk so we can detect orphans
// after an EasyStream crash or kill -9.
//
// True "reattachment" to a still-running FFmpeg child isn't possible on
// Unix once the parent process dies — the child's stdout/stderr pipes are
// gone and we can't reacquire them. The next best thing is deterministic
// cleanup: on startup we find any leftover ffmpeg from our previous
// session and terminate it, so state matches reality before we start a
// new stream. Without this an orphan keeps pushing to the destination
// invisibly until someone notices.
type PidFile struct {
	Path string
}

// Write atomically records the PID. Safe to call with Path="" (no-op).
func (p *PidFile) Write(pid int) error {
	if p == nil || p.Path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p.Path), 0700); err != nil {
		return err
	}
	return os.WriteFile(p.Path, []byte(strconv.Itoa(pid)), 0600)
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

	// Confirm it's actually ffmpeg before killing. PIDs recycle.
	if !isFFmpegProcess(pid) {
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

// isFFmpegProcess checks whether a PID corresponds to an ffmpeg process.
// Uses `ps` for portability across macOS/Linux/BSD. Matches only when the
// executable name (basename of argv[0]) is exactly "ffmpeg" — substring
// matching would falsely match ffmpeg.test (the test binary), ffmpeg-go,
// any tail -f /tmp/ffmpeg.log, etc.
//
// Returns false on any error so we err on the side of NOT killing
// unrelated processes.
func isFFmpegProcess(pid int) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	line := strings.TrimSpace(string(out))
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
