package ffmpeg

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type State string

const (
	StateIdle       State = "idle"
	StateStarting   State = "starting"
	StateRunning    State = "running"
	StateDegraded   State = "degraded"
	StateRestarting State = "restarting"
	StateStopping   State = "stopping"
	StateFailed     State = "failed"
)

type Status struct {
	State           State     `json:"state"`
	StartedAt       time.Time `json:"startedAt,omitempty"`
	UpdatedAt       time.Time `json:"updatedAt"`
	RestartCount    int       `json:"restartCount"`
	LastExit        string    `json:"lastExit,omitempty"`
	LastError       string    `json:"lastError,omitempty"`
	LastLogLine     string    `json:"lastLogLine,omitempty"`
	LastProgress    Progress  `json:"lastProgress"`
	ActivePresetID  string    `json:"activePresetId"`
	AudioRMSdB      float64   `json:"audioRmsDb"`      // finite dB value; lower = quieter, -120 = silence floor
	AudioRMSAt      time.Time `json:"audioRmsAt"`      // when AudioRMSdB was last updated
	AudioDetectedAt time.Time `json:"audioDetectedAt"` // when audio above silence floor was last seen
}

type SupervisorConfig struct {
	RestartInitialDelay time.Duration
	RestartMaxDelay     time.Duration
	StableAfter         time.Duration
	ProgressStallAfter  time.Duration
	MaxRestarts         int
	// PidFilePath records the FFmpeg child PID so orphans can be reaped
	// after a crash. Empty disables the feature.
	PidFilePath string
}

type Supervisor struct {
	mu     sync.Mutex
	cfg    SupervisorConfig
	logger *log.Logger

	cancel  context.CancelFunc
	done    chan struct{}
	ffmpeg  Config
	status  Status
	pidFile *PidFile
	restart chan string

	// onRestart is called whenever FFmpeg exits non-cleanly and is about
	// to be restarted by the supervisor. Used by the adaptive controller
	// to detect restart storms.
	onRestart func()
}

var errRestartRequested = errors.New("supervisor restart requested")

// SetOnRestart installs a callback invoked when FFmpeg restarts.
//
// Contract: the callback is invoked without the supervisor lock held, so
// the callback is free to call back into supervisor.Status() or anything
// that itself takes the supervisor lock. Do not change this invocation
// pattern without auditing every callback (adaptive controller +
// app.Server.publishState rely on it).
func (s *Supervisor) SetOnRestart(fn func()) {
	s.mu.Lock()
	s.onRestart = fn
	s.mu.Unlock()
}

// CurrentConfig returns a copy of the currently configured FFmpeg config.
func (s *Supervisor) CurrentConfig() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ffmpeg
}

func NewSupervisor(logger *log.Logger, cfg SupervisorConfig) *Supervisor {
	if cfg.RestartInitialDelay <= 0 {
		cfg.RestartInitialDelay = time.Second
	}
	if cfg.RestartMaxDelay <= 0 {
		cfg.RestartMaxDelay = 30 * time.Second
	}
	if cfg.StableAfter <= 0 {
		cfg.StableAfter = 2 * time.Minute
	}
	if cfg.ProgressStallAfter <= 0 {
		cfg.ProgressStallAfter = 20 * time.Second
	}
	if cfg.MaxRestarts <= 0 {
		cfg.MaxRestarts = 20
	}
	s := &Supervisor{
		cfg:    cfg,
		logger: logger,
		status: Status{State: StateIdle, UpdatedAt: time.Now().UTC()},
	}
	if cfg.PidFilePath != "" {
		s.pidFile = &PidFile{Path: cfg.PidFilePath}
		// Reap any orphan EasyStream-owned FFmpeg from a previous crash before
		// deciding whether intent should start a fresh stream. We never adopt a
		// blind process because that would lose progress, audio, and error data.
		if reaped, err := s.pidFile.ReapOrphan(); err != nil {
			logger.Printf("supervisor: pid file reap error: %v", err)
		} else if reaped > 0 {
			logger.Printf("supervisor: reaped orphan FFmpeg (pid %d) from previous session", reaped)
		}
	}
	return s
}

func (s *Supervisor) Start(config Config) error {
	if err := config.Validate(); err != nil {
		return err
	}
	// Build args once up front to surface configuration errors synchronously
	// to the caller, instead of finding them after the supervisor goroutine
	// has already moved into StateStarting.
	if _, err := config.Args(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return errors.New("stream is already running")
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	s.restart = make(chan string, 1)
	s.ffmpeg = config
	s.status = Status{
		State:          StateStarting,
		StartedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
		ActivePresetID: config.Preset.ID,
	}

	go s.run(ctx)
	return nil
}

// Restart asks the running supervisor loop to replace FFmpeg while keeping
// the stream intent active. It returns false when there is no active stream.
func (s *Supervisor) Restart(reason string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel == nil || s.restart == nil {
		return false
	}
	if reason == "" {
		reason = "restart requested"
	}
	s.status.State = StateDegraded
	s.status.LastError = reason
	s.status.UpdatedAt = time.Now().UTC()
	select {
	case s.restart <- reason:
	default:
	}
	return true
}

func (s *Supervisor) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	done := s.done
	if cancel != nil {
		s.status.State = StateStopping
		s.status.UpdatedAt = time.Now().UTC()
	}
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.status
	if (status.State == StateRunning || status.State == StateDegraded) && status.progressStalled(s.cfg.ProgressStallAfter, time.Now()) {
		status.State = StateDegraded
		status.LastError = "FFmpeg progress has stalled"
	}
	return status
}

func (s Status) progressStalled(after time.Duration, now time.Time) bool {
	if after <= 0 {
		return false
	}
	if !s.LastProgress.UpdatedAt.IsZero() {
		return now.Sub(s.LastProgress.UpdatedAt) > after
	}
	return !s.StartedAt.IsZero() && now.Sub(s.StartedAt) > after
}

func (s *Supervisor) progressStalledSince(started time.Time) (bool, string) {
	s.mu.Lock()
	status := s.status
	after := s.cfg.ProgressStallAfter
	s.mu.Unlock()
	now := time.Now()
	if !status.progressStalled(after, now) {
		return false, ""
	}
	if status.LastProgress.UpdatedAt.IsZero() {
		return true, fmt.Sprintf("FFmpeg reported no progress for %s", now.Sub(started).Round(time.Second))
	}
	return true, fmt.Sprintf("FFmpeg progress stalled for %s", now.Sub(status.LastProgress.UpdatedAt).Round(time.Second))
}

func (s *Supervisor) markDegraded(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.State = StateDegraded
	s.status.LastError = reason
	s.status.UpdatedAt = time.Now().UTC()
}

func (s *Supervisor) run(ctx context.Context) {
	defer func() {
		s.mu.Lock()
		done := s.done
		restart := s.restart
		s.cancel = nil
		s.done = nil
		s.restart = nil
		if s.status.State == StateStopping {
			s.status.State = StateIdle
			s.status.UpdatedAt = time.Now().UTC()
		}
		s.mu.Unlock()
		if done != nil {
			close(done)
		}
		if restart != nil {
			close(restart)
		}
	}()

	restarts := 0
	quickFailures := 0 // consecutive attempts that died within quickFailWindow
	const quickFailWindow = 5 * time.Second
	const quickFailLimit = 2 // 2 consecutive fast deaths = destination is rejecting us
	delay := s.cfg.RestartInitialDelay
	for {
		started := time.Now()
		err := s.runOnce(ctx)

		if ctx.Err() != nil {
			s.setExit(StateStopping, "stopped by user", "")
			return
		}

		restarts++
		elapsed := time.Since(started)
		lastExit := exitMessage(err)
		// Detect rapid back-to-back failures: an auth/connection rejection
		// (wrong stream key, ingest URL, disabled live input) makes FFmpeg
		// die within seconds. Network/encoder issues take longer.
		if elapsed < quickFailWindow {
			quickFailures++
		} else {
			quickFailures = 0
		}
		s.mu.Lock()
		s.status.RestartCount = restarts
		s.status.LastExit = lastExit
		s.status.UpdatedAt = time.Now().UTC()
		onRestart := s.onRestart
		s.mu.Unlock()
		if onRestart != nil {
			onRestart()
		}

		if quickFailures >= quickFailLimit {
			s.setExit(StateFailed, lastExit,
				"Destination rejected the connection. Check your stream key and ingest URL — "+
					"verify the live input is active in your dashboard, then click Go Live again.")
			return
		}
		if restarts > s.cfg.MaxRestarts {
			s.setExit(StateFailed, lastExit, "FFmpeg crashed too many times")
			return
		}

		if elapsed >= s.cfg.StableAfter {
			delay = s.cfg.RestartInitialDelay
		}

		wait := withJitter(delay)
		s.setRestarting(lastExit, wait)
		if !sleepContext(ctx, wait) {
			s.setExit(StateStopping, "stopped by user", "")
			return
		}
		delay *= 2
		if delay > s.cfg.RestartMaxDelay {
			delay = s.cfg.RestartMaxDelay
		}
	}
}

func (s *Supervisor) runOnce(ctx context.Context) error {
	args, err := s.ffmpeg.Args()
	if err != nil {
		return err
	}

	// Use plain exec.Command (not CommandContext) so shutdown/restart can send
	// SIGTERM before SIGKILL. Setpgid lets us signal the whole FFmpeg process
	// group and reap it deterministically on the next startup after kill -9.
	cmd := exec.Command(s.ffmpeg.Binary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	processStarted := time.Now()
	if err := cmd.Start(); err != nil {
		s.setExit(StateFailed, "", err.Error())
		return err
	}

	// Record the child PID so we can reap an orphan after a crash.
	if s.pidFile != nil {
		if err := s.pidFile.Write(cmd.Process.Pid); err != nil {
			s.logger.Printf("supervisor: failed to write pid file: %v", err)
		}
	}

	s.mu.Lock()
	s.status.State = StateRunning
	s.status.StartedAt = time.Now().UTC()
	s.status.LastProgress = Progress{}
	s.status.LastError = ""
	s.status.AudioRMSdB = 0
	s.status.AudioRMSAt = time.Time{}
	s.status.AudioDetectedAt = time.Time{}
	s.status.UpdatedAt = time.Now().UTC()
	preset := s.status.ActivePresetID
	outputMode := s.ffmpeg.OutputMode
	if outputMode == "" {
		outputMode = OutputRTMP
	}
	s.mu.Unlock()
	s.logger.Printf("stream-start: preset=%s output=%s pid=%d", preset, outputMode, cmd.Process.Pid)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = ParseProgress(stdout, s.recordProgress)
	}()
	go func() {
		defer wg.Done()
		s.recordLogs(stderr)
	}()

	// Wait for FFmpeg exit, explicit stop/restart, or stalled progress.
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	stallTicker := time.NewTicker(time.Second)
	defer stallTicker.Stop()
	s.mu.Lock()
	restartCh := s.restart
	s.mu.Unlock()

	for {
		select {
		case err := <-waitCh:
			wg.Wait()
			if s.pidFile != nil {
				s.pidFile.Clear()
			}
			s.logger.Printf("stream-end: ffmpeg exited after %s (%s)",
				time.Since(processStarted).Round(time.Second), exitMessage(err))
			return err

		case reason := <-restartCh:
			if reason == "" {
				reason = "restart requested"
			}
			s.logger.Printf("supervisor: restarting FFmpeg: %s", reason)
			err := terminateCommand(cmd, waitCh, 5*time.Second)
			wg.Wait()
			if s.pidFile != nil {
				s.pidFile.Clear()
			}
			if err != nil {
				return fmt.Errorf("%w: %s (%v)", errRestartRequested, reason, err)
			}
			return fmt.Errorf("%w: %s", errRestartRequested, reason)

		case <-stallTicker.C:
			if stalled, reason := s.progressStalledSince(processStarted); stalled {
				s.logger.Printf("supervisor: restarting FFmpeg: %s", reason)
				s.markDegraded(reason)
				err := terminateCommand(cmd, waitCh, 5*time.Second)
				wg.Wait()
				if s.pidFile != nil {
					s.pidFile.Clear()
				}
				if err != nil {
					return fmt.Errorf("%w: %s (%v)", errRestartRequested, reason, err)
				}
				return fmt.Errorf("%w: %s", errRestartRequested, reason)
			}

		case <-ctx.Done():
			s.logger.Printf("supervisor: stopping FFmpeg (pid %d)", cmd.Process.Pid)
			err := terminateCommand(cmd, waitCh, 5*time.Second)
			wg.Wait()
			if s.pidFile != nil {
				s.pidFile.Clear()
			}
			return err
		}
	}
}

func (s *Supervisor) recordProgress(progress Progress) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.LastProgress = progress
	s.status.UpdatedAt = time.Now().UTC()
	if s.status.State == StateDegraded {
		s.status.State = StateRunning
		s.status.LastError = ""
	}
}

func (s *Supervisor) recordLogs(r io.Reader) {
	scanner := bufio.NewScanner(r)
	s.mu.Lock()
	streamKey := s.ffmpeg.StreamName
	s.mu.Unlock()
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		// Audio level lines from ametadata file output look like:
		//   lavfi.astats.Overall.RMS_level=-22.45
		// When using file=/dev/stderr, ametadata also writes frame header
		// lines like "frame:N  pts:N  pts_time:N" — skip those silently.
		if rms, ok := parseAudioRMS(line); ok {
			now := time.Now().UTC()
			s.mu.Lock()
			s.status.AudioRMSdB = rms
			s.status.AudioRMSAt = now
			// Silence floor: RMS_level below -55 dB is effectively no audio.
			// FFmpeg returns "-inf" for absolute silence which parses to -math.Inf(-1).
			if rms > -55 {
				s.status.AudioDetectedAt = now
			}
			s.mu.Unlock()
			continue
		}
		// Skip ametadata frame-header lines (file=/dev/stderr output).
		if strings.HasPrefix(line, "frame:") && strings.Contains(line, "pts_time:") {
			continue
		}

		// FFmpeg echoes the full RTMP URL (including the stream key) when
		// the destination rejects the connection. Redact before storing or
		// logging so the key never leaks into log files or /status.
		redacted := redactStreamKey(line, streamKey)
		hint := classifyFFmpegError(redacted)
		s.mu.Lock()
		s.status.LastLogLine = redacted
		if hint != "" {
			s.status.LastError = hint
		}
		s.status.UpdatedAt = time.Now().UTC()
		s.mu.Unlock()
		if s.logger != nil {
			s.logger.Printf("ffmpeg: %s", redacted)
		}
	}
}

// permissionDeniedMessage returns OS-appropriate guidance for the
// "permission denied" FFmpeg error. macOS users need to grant TCC
// privacy permissions; Linux users typically need to be added to the
// `video` (and possibly `audio`) groups for /dev/video* access.
func permissionDeniedMessage() string {
	switch runtime.GOOS {
	case "darwin":
		return "Permission denied. On macOS, grant camera/microphone access in System Settings > Privacy & Security."
	case "linux":
		return "Permission denied. On Linux, ensure the easystream user is in the 'video' (and 'audio') groups: sudo usermod -aG video,audio $USER (then log out + back in)."
	default:
		return "Permission denied. The OS denied access to the capture device — check your platform's permission model."
	}
}

// redactStreamKey replaces occurrences of the stream key in a log line
// with "<redacted>" so the key never appears in logs or the /status API.
func redactStreamKey(line, key string) string {
	if key == "" || len(key) < 8 {
		return line
	}
	if !strings.Contains(line, key) {
		return line
	}
	return strings.ReplaceAll(line, key, "<redacted>")
}

// parseAudioRMS extracts the RMS_level value from an FFmpeg ametadata log line.
func parseAudioRMS(line string) (float64, bool) {
	idx := strings.Index(line, "lavfi.astats.Overall.RMS_level=")
	if idx < 0 {
		return 0, false
	}
	val := strings.TrimSpace(line[idx+len("lavfi.astats.Overall.RMS_level="):])
	// Trim anything after the number (FFmpeg occasionally appends spaces).
	if space := strings.IndexAny(val, " \t"); space >= 0 {
		val = val[:space]
	}
	if val == "-inf" || val == "nan" {
		// JSON cannot encode infinities/NaN. Keep the status API usable during
		// silence by clamping FFmpeg's -inf/nan to a finite silence floor.
		return -120, true
	}
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// classifyFFmpegError pattern-matches known fatal FFmpeg messages and returns
// a volunteer-friendly explanation. Empty string means "no useful hint".
func classifyFFmpegError(line string) string {
	l := strings.ToLower(line)
	switch {
	case strings.Contains(l, "tls") && strings.Contains(l, "broken pipe"),
		strings.Contains(l, "rtmp_sendpacket") && strings.Contains(l, "broken pipe"),
		strings.Contains(l, "rtmp server sent error"),
		strings.Contains(l, "rtmp_connect"):
		return "Destination rejected the connection. Verify your stream key and ingest URL."
	case strings.Contains(l, "no such file or directory") && strings.Contains(l, "input"):
		return "Capture device not found. Plug it in or pick a different source."
	case strings.Contains(l, "permission denied"),
		strings.Contains(l, "operation not permitted"):
		return permissionDeniedMessage()
	case strings.Contains(l, "device or resource busy"),
		strings.Contains(l, "device i/o error"),
		strings.Contains(l, "no av capture device"):
		return "Capture device is busy or unavailable. Close other apps using it (FaceTime, Zoom, OBS)."
	case strings.Contains(l, "connection refused"):
		return "Could not reach the destination. Check the ingest URL and your internet connection."
	case strings.Contains(l, "connection timed out"),
		strings.Contains(l, "timeout"):
		return "Connection to the destination timed out. Check your internet."
	}
	return ""
}

func (s *Supervisor) setRestarting(lastExit string, delay time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.State = StateRestarting
	s.status.LastExit = lastExit
	s.status.LastError = fmt.Sprintf("restarting FFmpeg in %s", delay.Round(time.Second))
	s.status.UpdatedAt = time.Now().UTC()
}

func (s *Supervisor) setExit(state State, lastExit, lastError string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.State = state
	s.status.LastExit = lastExit
	s.status.LastError = lastError
	s.status.UpdatedAt = time.Now().UTC()
}

func exitMessage(err error) string {
	if err == nil {
		return "FFmpeg exited cleanly"
	}
	return err.Error()
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func terminateCommand(cmd *exec.Cmd, waitCh <-chan error, grace time.Duration) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	_ = signalProcess(pid, syscall.SIGTERM)
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-waitCh:
		return err
	case <-timer.C:
		_ = signalProcess(pid, syscall.SIGKILL)
		return <-waitCh
	}
}

func signalProcess(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	// FFmpeg is started in its own process group. Signal the group first so
	// any helper children are not orphaned; fall back to the process itself.
	if err := syscall.Kill(-pid, sig); err == nil {
		return nil
	}
	return syscall.Kill(pid, sig)
}

func withJitter(delay time.Duration) time.Duration {
	if delay <= 0 {
		return delay
	}
	spread := int64(delay / 4)
	if spread <= 0 {
		return delay
	}
	return delay + time.Duration(rand.Int63n(spread))
}
