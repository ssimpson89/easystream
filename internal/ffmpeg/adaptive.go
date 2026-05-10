package ffmpeg

import (
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ssimpson89/easystream/internal/quality"
)

// AdaptiveConfig controls automatic quality fallback.
//
// Design notes (intentionally conservative — should NOT be aggressive):
//   - DropRateThreshold/DropWindow: must sustain bad drops for the full window
//     before triggering. A single bad sample never causes a downgrade.
//   - StartupGrace: ignore the first N seconds after a stream starts so
//     FFmpeg's warmup and stream-key handshake don't trigger false positives.
//   - DowngradeCooldown: after a tier change, wait this long before
//     considering another downgrade. Prevents ratcheting down rapidly.
//   - StableForRecovery: must be healthy for this long before stepping UP.
//   - MaxDowngrades: hard cap to prevent endless flapping.
type AdaptiveConfig struct {
	Enabled           bool
	DropRateThreshold float64       // 0.03 = 3% of frames dropped
	DropWindow        time.Duration // must sustain for this long
	SpeedThreshold    float64       // 0.92 = sustained below 92% real-time = network congestion
	StartupGrace      time.Duration
	DowngradeCooldown time.Duration
	StableForRecovery time.Duration
	MaxDowngrades     int
	RestartRateWindow time.Duration
	RestartRateMax    int
}

// DefaultAdaptiveConfig returns conservative defaults that match industry
// practice (similar to OBS dynamic bitrate but slower to react).
func DefaultAdaptiveConfig() AdaptiveConfig {
	return AdaptiveConfig{
		Enabled:           true,
		DropRateThreshold: 0.03,             // 3% — significantly above ideal (0%) and worrying (2%)
		DropWindow:        60 * time.Second, // must be bad for a full minute
		SpeedThreshold:    0.92,             // <92% real-time for 60s = TCP send buffer full
		StartupGrace:      60 * time.Second,
		DowngradeCooldown: 5 * time.Minute,
		StableForRecovery: 10 * time.Minute,
		MaxDowngrades:     3,
		RestartRateWindow: 5 * time.Minute,
		RestartRateMax:    3,
	}
}

// AdaptiveState is the controller's externally visible status.
type AdaptiveState struct {
	Enabled         bool      `json:"enabled"`
	OriginalPreset  string    `json:"originalPreset,omitempty"`
	ActivePreset    string    `json:"activePreset,omitempty"`
	Reason          string    `json:"reason,omitempty"`
	DowngradeCount  int       `json:"downgradeCount"`
	LastTransition  time.Time `json:"lastTransition,omitempty"`
	IsFallback      bool      `json:"isFallback"`
}

// AdaptiveController watches a Supervisor's progress and adjusts quality.
type AdaptiveController struct {
	cfg    AdaptiveConfig
	logger *log.Logger
	sup    *Supervisor

	mu sync.Mutex

	// Target preset chosen by the user. Recovery never exceeds this tier.
	targetPreset string
	// Current preset actually running.
	currentPreset string
	// When the current stream started (for startup grace).
	streamStartedAt time.Time
	// Sliding-window samples of drop rate.
	dropSamples []dropSample
	// When degradation first appeared (zero = healthy).
	degradingSince time.Time
	// When stable conditions began (zero = not stable / has degradation).
	stableSince time.Time
	// When the last tier change happened (for cooldown).
	lastTransitionAt time.Time
	// Restart timestamps (sliding window).
	restartTimes []time.Time
	// Total downgrades this stream.
	downgradeCount int
	// Last reason text shown to user.
	reason string

	cancel chan struct{}
	done   chan struct{}
}

type dropSample struct {
	at      time.Time
	frame   int
	dropped int
	speed   float64 // playback speed (1.0 = real-time)
}

// NewAdaptiveController creates a controller for the given supervisor.
func NewAdaptiveController(sup *Supervisor, cfg AdaptiveConfig, logger *log.Logger) *AdaptiveController {
	if cfg.DropWindow <= 0 {
		cfg = DefaultAdaptiveConfig()
	}
	return &AdaptiveController{cfg: cfg, sup: sup, logger: logger}
}

// SetEnabled toggles auto-quality at runtime.
func (a *AdaptiveController) SetEnabled(enabled bool) {
	a.mu.Lock()
	a.cfg.Enabled = enabled
	a.mu.Unlock()
}

// OnStreamStart records the user-selected target and resets state.
func (a *AdaptiveController) OnStreamStart(presetID string) {
	a.mu.Lock()
	a.targetPreset = presetID
	a.currentPreset = presetID
	a.streamStartedAt = time.Now()
	a.dropSamples = a.dropSamples[:0]
	a.degradingSince = time.Time{}
	a.stableSince = time.Time{}
	a.lastTransitionAt = time.Time{}
	a.restartTimes = a.restartTimes[:0]
	a.downgradeCount = 0
	a.reason = ""
	a.mu.Unlock()
}

// OnStreamStop clears state so the next start begins fresh.
func (a *AdaptiveController) OnStreamStop() {
	a.mu.Lock()
	a.targetPreset = ""
	a.currentPreset = ""
	a.mu.Unlock()
}

// OnRestart records that FFmpeg restarted (used to detect restart storms).
func (a *AdaptiveController) OnRestart() {
	a.mu.Lock()
	now := time.Now()
	a.restartTimes = append(a.restartTimes, now)
	// Trim old entries.
	cutoff := now.Add(-a.cfg.RestartRateWindow)
	idx := 0
	for i, t := range a.restartTimes {
		if t.After(cutoff) {
			idx = i
			break
		}
	}
	a.restartTimes = a.restartTimes[idx:]
	a.mu.Unlock()
}

// Start begins the watchdog loop.
func (a *AdaptiveController) Start() {
	a.mu.Lock()
	if a.cancel != nil {
		a.mu.Unlock()
		return
	}
	a.cancel = make(chan struct{})
	a.done = make(chan struct{})
	a.mu.Unlock()
	go a.run()
}

// Stop halts the watchdog.
func (a *AdaptiveController) Stop() {
	a.mu.Lock()
	cancel := a.cancel
	done := a.done
	a.cancel = nil
	a.mu.Unlock()
	if cancel != nil {
		close(cancel)
	}
	if done != nil {
		<-done
	}
}

// State returns the externally visible adaptive state.
func (a *AdaptiveController) State() AdaptiveState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return AdaptiveState{
		Enabled:        a.cfg.Enabled,
		OriginalPreset: a.targetPreset,
		ActivePreset:   a.currentPreset,
		Reason:         a.reason,
		DowngradeCount: a.downgradeCount,
		LastTransition: a.lastTransitionAt,
		IsFallback:     a.currentPreset != "" && a.currentPreset != a.targetPreset,
	}
}

func (a *AdaptiveController) run() {
	defer func() {
		a.mu.Lock()
		done := a.done
		a.done = nil
		a.mu.Unlock()
		if done != nil {
			close(done)
		}
	}()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		a.mu.Lock()
		cancel := a.cancel
		a.mu.Unlock()
		if cancel == nil {
			return
		}
		select {
		case <-cancel:
			return
		case <-ticker.C:
			a.tick()
		}
	}
}

func (a *AdaptiveController) tick() {
	a.mu.Lock()
	if !a.cfg.Enabled || a.targetPreset == "" || a.currentPreset == "" {
		a.mu.Unlock()
		return
	}
	// Startup grace — don't react during the warmup window.
	if time.Since(a.streamStartedAt) < a.cfg.StartupGrace {
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()

	status := a.sup.Status()
	// Only act when the stream is actually live (running, degraded, restarting).
	if status.State != StateRunning && status.State != StateDegraded && status.State != StateRestarting {
		return
	}

	a.sampleProgress(status.LastProgress)

	dropping := a.isPersistentDrop()
	slow := a.isPersistentlySlow()
	stalling := status.State == StateDegraded
	restartStorm := a.hasRestartStorm()

	if dropping || slow || stalling || restartStorm {
		reason := ""
		switch {
		case stalling:
			reason = "FFmpeg progress stalled (network or encoder issue)"
		case restartStorm:
			reason = "Stream restarted repeatedly — network unstable"
		case dropping:
			reason = "Sustained dropped frames detected"
		case slow:
			reason = "Encoder falling behind real-time (upload bandwidth limited)"
		}
		a.handleDegradation(reason)
		return
	}

	// Healthy: consider stepping back up.
	a.maybeRecover()
}

// sampleProgress records the latest progress sample and trims the window.
func (a *AdaptiveController) sampleProgress(p Progress) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if p.UpdatedAt.IsZero() {
		return
	}
	if n := len(a.dropSamples); n > 0 && a.dropSamples[n-1].at.Equal(p.UpdatedAt) {
		return
	}
	a.dropSamples = append(a.dropSamples, dropSample{
		at:      p.UpdatedAt,
		frame:   p.Frame,
		dropped: p.Dropped,
		speed:   parseSpeed(p.Speed),
	})
	cutoff := time.Now().Add(-a.cfg.DropWindow)
	idx := 0
	for i, s := range a.dropSamples {
		if s.at.After(cutoff) {
			idx = i
			break
		}
	}
	a.dropSamples = a.dropSamples[idx:]
}

// isPersistentlySlow returns true if the encoder has been running below
// SpeedThreshold for the full DropWindow. For RTMP this signals that the
// TCP send buffer is full because the network can't keep up.
func (a *AdaptiveController) isPersistentlySlow() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.dropSamples) < 4 {
		return false
	}
	first := a.dropSamples[0]
	last := a.dropSamples[len(a.dropSamples)-1]
	if last.at.Sub(first.at) < a.cfg.DropWindow-(5*time.Second) {
		return false
	}
	// All samples must be below the threshold. A single recovery moment
	// resets the timer — we want sustained, not flapping.
	for _, s := range a.dropSamples {
		if s.speed <= 0 || s.speed >= a.cfg.SpeedThreshold {
			return false
		}
	}
	return true
}

// parseSpeed parses FFmpeg's speed string ("1.05x", "0.92x", "N/A") into a float.
// Returns 0 if unparseable.
func parseSpeed(s string) float64 {
	s = strings.TrimSpace(strings.TrimSuffix(s, "x"))
	if s == "" || s == "N/A" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// isPersistentDrop returns true if the drop rate has been above the
// threshold for the full DropWindow.
func (a *AdaptiveController) isPersistentDrop() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.dropSamples) < 2 {
		return false
	}
	first := a.dropSamples[0]
	last := a.dropSamples[len(a.dropSamples)-1]

	// Need at least DropWindow of coverage.
	if last.at.Sub(first.at) < a.cfg.DropWindow-(5*time.Second) {
		return false
	}
	frames := last.frame - first.frame
	dropped := last.dropped - first.dropped
	if frames <= 0 {
		return false
	}
	rate := float64(dropped) / float64(frames+dropped)
	return rate >= a.cfg.DropRateThreshold
}

func (a *AdaptiveController) hasRestartStorm() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	cutoff := time.Now().Add(-a.cfg.RestartRateWindow)
	count := 0
	for _, t := range a.restartTimes {
		if t.After(cutoff) {
			count++
		}
	}
	return count >= a.cfg.RestartRateMax
}

func (a *AdaptiveController) handleDegradation(reason string) {
	a.mu.Lock()
	// Check cooldown — don't downgrade too often.
	if !a.lastTransitionAt.IsZero() && time.Since(a.lastTransitionAt) < a.cfg.DowngradeCooldown {
		a.mu.Unlock()
		return
	}
	if a.downgradeCount >= a.cfg.MaxDowngrades {
		a.mu.Unlock()
		return
	}
	current := a.currentPreset
	a.mu.Unlock()

	next, ok := quality.LowerTier(current)
	if !ok {
		return // already at the floor
	}

	a.logger.Printf("adaptive: degradation detected (%s). Stepping down %s → %s", reason, current, next.ID)
	if err := a.transitionTo(next.ID, reason); err != nil {
		a.logger.Printf("adaptive: failed to step down: %v", err)
	}
}

func (a *AdaptiveController) maybeRecover() {
	a.mu.Lock()
	if a.currentPreset == a.targetPreset {
		a.mu.Unlock()
		return
	}
	if a.lastTransitionAt.IsZero() || time.Since(a.lastTransitionAt) < a.cfg.StableForRecovery {
		a.mu.Unlock()
		return
	}
	current := a.currentPreset
	target := a.targetPreset
	a.mu.Unlock()

	next, ok := quality.HigherTier(current, target)
	if !ok {
		return
	}
	a.logger.Printf("adaptive: stable for %s. Stepping back up %s → %s", a.cfg.StableForRecovery, current, next.ID)
	if err := a.transitionTo(next.ID, "stable conditions — restoring quality"); err != nil {
		a.logger.Printf("adaptive: failed to step up: %v", err)
	}
}

// transitionTo restarts FFmpeg at the given preset.
func (a *AdaptiveController) transitionTo(presetID, reason string) error {
	preset, ok := quality.ByID(presetID)
	if !ok {
		return nil
	}

	// Grab the current config from the supervisor and swap the preset.
	cfg := a.sup.CurrentConfig()
	cfg.Preset = preset

	// Restart cleanly.
	a.sup.Stop()
	if err := a.sup.Start(cfg); err != nil {
		return err
	}

	a.mu.Lock()
	wasDowngrade := quality.IndexOf(presetID) < quality.IndexOf(a.currentPreset)
	a.currentPreset = presetID
	a.lastTransitionAt = time.Now()
	a.streamStartedAt = time.Now() // reset grace window after a restart
	a.dropSamples = a.dropSamples[:0]
	a.reason = reason
	if wasDowngrade {
		a.downgradeCount++
	}
	a.mu.Unlock()
	return nil
}
