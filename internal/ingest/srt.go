// Package ingest supervises long-lived "stream receiver" processes that
// run independently of the main encoding pipeline. The first (and so far
// only) receiver is the SRT listener used by EasyStream's "Receive a
// stream from OBS / vMix / encoder" source kind.
//
// The lifecycle is intentionally different from the main supervisor:
//
//   - Main supervisor runs only while the operator is "live" — it
//     binds capture devices, encodes, and pushes to YouTube.
//   - Ingest receivers run as soon as the source is configured, so an
//     upstream encoder can connect and the preview can show the feed
//     BEFORE the operator commits to going live.
//
// The SRT receiver binds the configured port via libsrt and copies the
// incoming MPEG-TS to a local UDP relay. The preview and the main
// supervisor both consume the relay, so neither has to compete for the
// SRT port and the operator-facing "go live" press never interrupts
// the encoder's connection.
package ingest

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ssimpson89/easystream/internal/ffmpeg"
)

// State enumerates the receiver's lifecycle from the operator's
// perspective. Mirrors ffmpeg.State but uses a smaller set because
// the receiver has no "degraded" / "restarting" affordance — when it
// fails it just retries with backoff.
type State string

const (
	StateIdle     State = "idle"     // no receiver wanted / not running
	StateStarting State = "starting" // ffmpeg launched, not yet observed running
	StateRunning  State = "running"  // ffmpeg up; peer may or may not be pushing
	StateFailed   State = "failed"   // ffmpeg exited; in restart backoff
)

// Status is the snapshot returned to the SSE state feed. The UI keys
// the pre-flight Video pill off these fields when in SRT-listener
// mode, so they replace stream.state/fps for that source kind.
type Status struct {
	State         State     `json:"state"`
	Port          int       `json:"port,omitempty"`
	PeerConnected bool      `json:"peerConnected"`
	FPS           float64   `json:"fps"`
	BitrateKbps   float64   `json:"bitrateKbps"`
	LastFrameAt   time.Time `json:"lastFrameAt,omitempty"`
	LastError     string    `json:"lastError,omitempty"`
	StartedAt     time.Time `json:"startedAt,omitempty"`
	RestartCount  int       `json:"restartCount"`
}

// DesiredConfig describes the intended receiver state. Port==0 means
// "no receiver wanted" — Apply will tear any running one down.
type DesiredConfig struct {
	Binary     string
	Port       int
	Passphrase string
	RelayPort  int
}

// Equal reports whether two configs are semantically identical. Used
// by Apply to avoid a needless ffmpeg restart when the operator saves
// the source panel without changing the SRT-relevant fields.
func (c DesiredConfig) Equal(other DesiredConfig) bool {
	return c.Binary == other.Binary &&
		c.Port == other.Port &&
		c.Passphrase == other.Passphrase &&
		c.RelayPort == other.RelayPort
}

// peerStaleAfter declares the receiver "no peer connected" if no frame
// has been recorded for this long. ffmpeg's -progress output emits at
// ~2 Hz while a peer is pushing, so 5 s is well beyond the noise floor
// without making a freshly-stopped encoder linger on the UI.
const peerStaleAfter = 5 * time.Second

// restartBackoffInitial / restartBackoffMax bracket the exponential
// backoff between retry attempts when the receiver ffmpeg exits
// non-cleanly. Initial is fast so a transient port conflict (e.g. the
// receiver coming up before EasyStream's previous instance fully
// released the port) recovers quickly; max keeps the log from filling
// when something is genuinely wrong (e.g. ffmpeg not installed).
const (
	restartBackoffInitial = 500 * time.Millisecond
	restartBackoffMax     = 10 * time.Second
)

// Receiver owns the lifecycle of a single ffmpeg child that binds an
// SRT listener and relays its MPEG-TS output to a localhost UDP port.
//
// One Receiver is created per Server. The Apply method is the only
// way to change what's running; the run goroutine handles transitions
// (start, restart, stop) so callers don't have to coordinate them.
type Receiver struct {
	logger *log.Logger

	mu       sync.Mutex
	status   Status
	current  DesiredConfig
	onChange func()

	// runCancel terminates the current ffmpeg's context (and its
	// supervisory goroutine). runDone closes when that goroutine
	// exits; Apply blocks on it to guarantee no two ffmpegs are
	// ever fighting for the same SRT port at once.
	runCancel context.CancelFunc
	runDone   chan struct{}

	// Test hook: replaces exec.CommandContext so tests can swap in a
	// fake ffmpeg without touching the real binary. Production code
	// never sets this.
	cmdContext func(ctx context.Context, name string, args ...string) *exec.Cmd
}

// NewReceiver constructs a Receiver with no ffmpeg running. Call
// Apply(cfg) to start one, or Apply with Port==0 to stay idle.
func NewReceiver(logger *log.Logger) *Receiver {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Receiver{
		logger:     logger,
		status:     Status{State: StateIdle},
		cmdContext: exec.CommandContext,
	}
}

// SetOnChange registers a callback invoked when the receiver's status
// changes meaningfully (state transition or peer-connected toggle).
// Used by app.Server.publishState so SSE subscribers see receiver
// state changes immediately. The callback runs without the receiver's
// lock held.
func (r *Receiver) SetOnChange(fn func()) {
	r.mu.Lock()
	r.onChange = fn
	r.mu.Unlock()
}

// Status returns a snapshot of the receiver's current state. Safe to
// call from any goroutine.
func (r *Receiver) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.status
	// Recompute PeerConnected on read so the UI doesn't show a stale
	// "connected" badge after the encoder disconnects. ffmpeg won't
	// emit a fresh progress line when the peer stops pushing — it
	// will eventually exit, but until it does the last fps reading
	// is stale.
	if s.PeerConnected && !s.LastFrameAt.IsZero() && time.Since(s.LastFrameAt) > peerStaleAfter {
		s.PeerConnected = false
		s.FPS = 0
	}
	return s
}

// Apply reconciles the receiver to the new desired config. Synchronously
// tears down the previous ffmpeg (if any) before starting a new one, so
// the SRT port is never held by two processes at once. Returns once the
// new attempt has been scheduled — the run goroutine handles success or
// failure of the actual bind.
func (r *Receiver) Apply(cfg DesiredConfig) {
	r.mu.Lock()
	// Same config as last time and we're already running it? No-op.
	if r.runCancel != nil && r.current.Equal(cfg) {
		r.mu.Unlock()
		return
	}
	prevCancel := r.runCancel
	prevDone := r.runDone
	r.runCancel = nil
	r.runDone = nil
	r.mu.Unlock()

	if prevCancel != nil {
		prevCancel()
		<-prevDone
	}

	if cfg.Port == 0 {
		r.setStatus(Status{State: StateIdle})
		r.mu.Lock()
		r.current = DesiredConfig{}
		r.mu.Unlock()
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.mu.Lock()
	r.current = cfg
	r.runCancel = cancel
	r.runDone = done
	r.mu.Unlock()

	go r.runLoop(ctx, cfg, done)
}

// Stop tears the receiver down permanently. After Stop returns, Apply
// will start a fresh one if called.
func (r *Receiver) Stop() {
	r.Apply(DesiredConfig{})
}

// runLoop owns one configuration's ffmpeg lifecycle: spawn, wait,
// restart on exit with exponential backoff until ctx is cancelled.
func (r *Receiver) runLoop(ctx context.Context, cfg DesiredConfig, done chan struct{}) {
	defer close(done)

	backoff := restartBackoffInitial
	restarts := 0

	for {
		if ctx.Err() != nil {
			r.setStatus(Status{State: StateIdle, Port: cfg.Port})
			return
		}

		startedAt := time.Now()
		r.setStatus(Status{
			State:        StateStarting,
			Port:         cfg.Port,
			StartedAt:    startedAt,
			RestartCount: restarts,
		})

		err := r.runOnce(ctx, cfg, startedAt, restarts)

		if ctx.Err() != nil {
			r.setStatus(Status{State: StateIdle, Port: cfg.Port})
			return
		}

		// Process exited unexpectedly. Prefer the libsrt/ffmpeg stderr
		// line scanStderr captured (e.g. "Address already in use",
		// "Connection setup failure: Connection rejected, error: ...")
		// over cmd.Wait()'s less-useful "exit status N" string. Only
		// fall back to the exit-status form if stderr told us nothing.
		r.mu.Lock()
		stderrLast := r.status.LastError
		r.mu.Unlock()
		errMsg := stderrLast
		if errMsg == "" && err != nil {
			errMsg = err.Error()
		}
		r.logger.Printf("ingest/srt: receiver exited (%v) — retry in %s", err, backoff)
		r.setStatus(Status{
			State:        StateFailed,
			Port:         cfg.Port,
			LastError:    errMsg,
			RestartCount: restarts,
		})

		select {
		case <-ctx.Done():
			r.setStatus(Status{State: StateIdle, Port: cfg.Port})
			return
		case <-time.After(backoff):
		}
		restarts++
		backoff *= 2
		if backoff > restartBackoffMax {
			backoff = restartBackoffMax
		}
	}
}

// runOnce starts a single ffmpeg invocation and returns when it exits.
// The returned error describes the exit cause; ctx cancellation maps
// to nil so the caller can distinguish "operator changed config" from
// "ffmpeg crashed".
func (r *Receiver) runOnce(ctx context.Context, cfg DesiredConfig, startedAt time.Time, restarts int) error {
	binary := cfg.Binary
	if binary == "" {
		binary = "ffmpeg"
	}
	args := buildReceiverArgs(cfg)
	r.logger.Printf("ingest/srt: starting receiver on port %d → udp://127.0.0.1:%d (attempt %d)", cfg.Port, cfg.RelayPort, restarts+1)

	cmd := r.cmdContext(ctx, binary, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	// Parse -progress stream on stdout for fps / bitrate. ffmpeg
	// emits these regardless of whether the SRT peer is currently
	// pushing — but the values only become non-zero once frames
	// flow. We treat first-fps-non-zero as "peer connected".
	progressDone := make(chan struct{})
	go func() {
		defer close(progressDone)
		r.parseProgress(stdout, cfg.Port, startedAt, restarts)
	}()

	// Peer-stale watchdog. ffmpeg may not emit a progress block
	// when the SRT peer disconnects without ffmpeg also exiting
	// (e.g. libsrt holds the listener open between connections).
	// Without this ticker, PeerConnected would stay true on the UI
	// indefinitely — Status() recomputes staleness on read, but no
	// SSE push is ever fired because setStatus is only invoked from
	// the progress-block boundary. The ticker drives an explicit
	// re-publish so the operator sees "waiting for your encoder to
	// connect" again within a few seconds of OBS dropping out.
	watchdogCtx, watchdogCancel := context.WithCancel(ctx)
	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		ticker := time.NewTicker(peerStaleAfter / 2)
		defer ticker.Stop()
		for {
			select {
			case <-watchdogCtx.Done():
				return
			case <-ticker.C:
				r.republishIfPeerStale(cfg.Port, startedAt, restarts)
			}
		}
	}()

	// Capture stderr for the last error line. libsrt prints
	// connection failures (passphrase mismatch, port-in-use, etc.)
	// here. We don't log every line — too noisy — but the most
	// recent line is surfaced via Status.LastError so the UI can
	// show "Address already in use" instead of a generic failure.
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		r.scanStderr(stderr)
	}()

	err = cmd.Wait()
	watchdogCancel()
	<-progressDone
	<-stderrDone
	<-watchdogDone
	_ = stdout.Close()
	_ = stderr.Close()
	return err
}

// republishIfPeerStale checks whether the last frame is older than
// peerStaleAfter while the receiver believes it's still connected,
// and if so emits a fresh setStatus to drive an SSE push so the UI
// flips from "Encoder connected" back to "waiting for your encoder
// to connect". ffmpeg holds the SRT listener open between encoder
// sessions, so progress-block boundaries can't be relied on for
// disconnect detection.
func (r *Receiver) republishIfPeerStale(port int, startedAt time.Time, restarts int) {
	r.mu.Lock()
	cur := r.status
	stale := cur.PeerConnected && !cur.LastFrameAt.IsZero() && time.Since(cur.LastFrameAt) > peerStaleAfter
	r.mu.Unlock()
	if !stale {
		return
	}
	r.setStatus(Status{
		State:         StateRunning,
		Port:          port,
		PeerConnected: false,
		FPS:           0,
		BitrateKbps:   0,
		StartedAt:     startedAt,
		RestartCount:  restarts,
	})
}

// buildReceiverArgs constructs the ffmpeg command line. Kept in a
// standalone function so tests can verify it without spawning a
// process.
//
// Threat-model note: the SRT passphrase is embedded in the ffmpeg
// URL argv, which means any local user on the host can read it via
// `ps`, /proc/<pid>/cmdline, or similar. EasyStream's primary
// deployment is a single-operator desktop / Pi running this daemon
// under one account, where that exposure is acceptable. Hardening
// would require either replacing the ffmpeg-based receiver with a
// direct libsrt binding in Go (so the passphrase stays in process
// memory), or having ffmpeg consume the passphrase from an env
// var / file — neither of which libsrt currently accepts via its
// URL options. Documented here so a future maintainer doesn't
// quietly inherit the assumption.
func buildReceiverArgs(cfg DesiredConfig) []string {
	srtURL := ffmpeg.SRTListenerURL(cfg.Port, cfg.Passphrase)
	relayURL := fmt.Sprintf("udp://127.0.0.1:%d?pkt_size=1316", cfg.RelayPort)
	return []string{
		"-nostdin",
		"-hide_banner",
		"-loglevel", "warning",
		// -progress streams key=value to stdout at ~2 Hz; we parse
		// it to drive PeerConnected / FPS / BitrateKbps.
		"-progress", "pipe:1",
		// discardcorrupt+genpts on the SRT input handles MPEG-TS
		// discontinuities at re-connect cleanly. Same flag the main
		// pipeline uses for network inputs.
		"-fflags", "discardcorrupt+genpts",
		"-i", srtURL,
		// -c copy: no re-encode. We're just demuxing SRT → MPEG-TS
		// and pushing onto the local UDP relay. Keeps CPU near zero
		// when idle and preserves every byte the encoder sent for
		// the main pipeline to consume.
		"-c", "copy",
		"-f", "mpegts",
		relayURL,
	}
}

// parseProgress reads ffmpeg's -progress key=value stream and updates
// fps / bitrate / peer-connected as values arrive. Each "block" ends
// with a line `progress=continue` (running) or `progress=end` (exit);
// we ignore those markers and only act on the data keys.
func (r *Receiver) parseProgress(rc io.Reader, port int, startedAt time.Time, restarts int) {
	scanner := bufio.NewScanner(rc)
	// MPEG-TS progress lines are short, but ffmpeg uses a 64 KiB
	// internal buffer for stdout — match that so we don't truncate.
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024)

	var fps, bitrate float64
	var sawFirstFrame bool
	for scanner.Scan() {
		line := scanner.Text()
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "fps":
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				fps = f
			}
		case "bitrate":
			// "bitrate=2500.0kbits/s" or "N/A" — strip suffix
			val = strings.TrimSuffix(val, "kbits/s")
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				bitrate = f
			}
		case "frame":
			if n, err := strconv.Atoi(val); err == nil && n > 0 {
				sawFirstFrame = true
			}
		case "progress":
			// End of a progress block — publish the snapshot.
			peer := sawFirstFrame && fps > 0
			lastFrameAt := time.Time{}
			if peer {
				lastFrameAt = time.Now()
			}
			r.setStatus(Status{
				State:         StateRunning,
				Port:          port,
				PeerConnected: peer,
				FPS:           fps,
				BitrateKbps:   bitrate,
				LastFrameAt:   lastFrameAt,
				StartedAt:     startedAt,
				RestartCount:  restarts,
			})
		}
	}
	// A scanner error stops the loop silently, which means the receiver
	// would no longer update fps / peer state — surface it in the log
	// so the failure mode is debuggable. Common cases: progress line
	// exceeded the 64 KiB buffer (shouldn't happen), or the stdout
	// pipe got an I/O error.
	if err := scanner.Err(); err != nil {
		r.logger.Printf("ingest/srt: progress parser stopped: %v", err)
	}
}

// scanStderr keeps the most recent stderr line in Status.LastError so
// the UI can surface libsrt's actual error message (e.g. "Address
// already in use") instead of a generic failure code.
func (r *Receiver) scanStderr(rc io.Reader) {
	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 0, 16*1024), 64*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		// Redact URLs in case a libsrt log line echoes the passphrase
		// or full SRT URL. Same hardening the main supervisor applies
		// to its own stderr.
		clean := ffmpeg.RedactURLsInLog(line)
		r.mu.Lock()
		r.status.LastError = clean
		r.mu.Unlock()
	}
	if err := scanner.Err(); err != nil {
		r.logger.Printf("ingest/srt: stderr scanner stopped: %v", err)
	}
}

// setStatus replaces the entire status snapshot and fires the
// onChange callback when state or peer-connectedness flipped. We
// don't fire on every progress tick — that would flood the SSE hub.
//
// LastError is a sticky field: parseProgress writes a fresh Status
// every ~0.5 s with LastError="", and scanStderr writes the field
// directly in a separate goroutine. Without preservation, every
// progress tick would wipe the most recent stderr line — so we
// carry it over unless the caller is explicitly clearing it (next
// transitions to Idle/Starting, which want a fresh LastError).
func (r *Receiver) setStatus(next Status) {
	r.mu.Lock()
	prev := r.status
	if next.LastError == "" && next.State == StateRunning {
		next.LastError = prev.LastError
	}
	r.status = next
	cb := r.onChange
	r.mu.Unlock()

	if cb == nil {
		return
	}
	if prev.State != next.State || prev.PeerConnected != next.PeerConnected {
		cb()
	}
}
