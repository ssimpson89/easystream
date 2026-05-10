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
	"strings"
	"sync"
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
	State          State     `json:"state"`
	StartedAt      time.Time `json:"startedAt,omitempty"`
	UpdatedAt      time.Time `json:"updatedAt"`
	RestartCount   int       `json:"restartCount"`
	LastExit       string    `json:"lastExit,omitempty"`
	LastError      string    `json:"lastError,omitempty"`
	LastLogLine    string    `json:"lastLogLine,omitempty"`
	LastProgress   Progress  `json:"lastProgress"`
	ActivePresetID string    `json:"activePresetId"`
	Command        []string  `json:"command,omitempty"`
}

type SupervisorConfig struct {
	RestartInitialDelay time.Duration
	RestartMaxDelay     time.Duration
	StableAfter         time.Duration
	ProgressStallAfter  time.Duration
	MaxRestarts         int
}

type Supervisor struct {
	mu     sync.Mutex
	cfg    SupervisorConfig
	logger *log.Logger

	cancel context.CancelFunc
	done   chan struct{}
	ffmpeg Config
	status Status

	// onRestart is called whenever FFmpeg exits non-cleanly and is about
	// to be restarted by the supervisor. Used by the adaptive controller
	// to detect restart storms.
	onRestart func()
}

// SetOnRestart installs a callback invoked when FFmpeg restarts.
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
	return &Supervisor{
		cfg:    cfg,
		logger: logger,
		status: Status{State: StateIdle, UpdatedAt: time.Now().UTC()},
	}
}

func (s *Supervisor) Start(config Config) error {
	if err := config.Validate(); err != nil {
		return err
	}

	args, err := config.Args()
	if err != nil {
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
	s.ffmpeg = config
	s.status = Status{
		State:          StateStarting,
		StartedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
		ActivePresetID: config.Preset.ID,
		Command:        append([]string{config.Binary}, args...),
	}

	go s.run(ctx)
	return nil
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
	if (status.State == StateRunning || status.State == StateDegraded) &&
		!status.LastProgress.UpdatedAt.IsZero() &&
		time.Since(status.LastProgress.UpdatedAt) > s.cfg.ProgressStallAfter {
		status.State = StateDegraded
		status.LastError = "FFmpeg progress has stalled"
	}
	return status
}

func (s *Supervisor) run(ctx context.Context) {
	defer func() {
		s.mu.Lock()
		done := s.done
		s.cancel = nil
		s.done = nil
		if s.status.State == StateStopping {
			s.status.State = StateIdle
			s.status.UpdatedAt = time.Now().UTC()
		}
		s.mu.Unlock()
		if done != nil {
			close(done)
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

	cmd := exec.CommandContext(ctx, s.ffmpeg.Binary, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		s.setExit(StateFailed, "", err.Error())
		return err
	}

	s.mu.Lock()
	s.status.State = StateRunning
	s.status.UpdatedAt = time.Now().UTC()
	s.mu.Unlock()

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

	err = cmd.Wait()
	wg.Wait()
	return err
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
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		hint := classifyFFmpegError(line)
		s.mu.Lock()
		s.status.LastLogLine = line
		if hint != "" {
			s.status.LastError = hint
		}
		s.status.UpdatedAt = time.Now().UTC()
		s.mu.Unlock()
		if s.logger != nil {
			s.logger.Printf("ffmpeg: %s", line)
		}
	}
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
		return "Permission denied. On macOS, grant camera/microphone access in System Settings > Privacy."
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
