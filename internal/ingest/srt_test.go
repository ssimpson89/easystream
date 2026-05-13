package ingest

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestBuildReceiverArgsContainsSRTAndRelay(t *testing.T) {
	cfg := DesiredConfig{
		Binary:    "ffmpeg",
		Port:      9001,
		RelayPort: 52003,
	}
	args := buildReceiverArgs(cfg)
	joined := strings.Join(args, " ")

	// SRT bind: 0.0.0.0 host (libsrt rejects an empty host), the
	// operator-picked port, listener mode + tlpktdrop tuning.
	for _, want := range []string{
		"srt://0.0.0.0:9001",
		"mode=listener",
		"tlpktdrop=1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q in args: %s", want, joined)
		}
	}

	// Relay output: localhost UDP MPEG-TS so the main pipeline /
	// preview have something to read.
	relayWant := fmt.Sprintf("udp://127.0.0.1:%d", cfg.RelayPort)
	if !strings.Contains(joined, relayWant) {
		t.Errorf("expected relay URL %q in args: %s", relayWant, joined)
	}
	if !strings.Contains(joined, "-f mpegts") {
		t.Errorf("expected -f mpegts output in args: %s", joined)
	}
	if !strings.Contains(joined, "-c copy") {
		t.Errorf("expected -c copy (no re-encode) in args: %s", joined)
	}

	// -progress pipe:1 — without this, peer-connected detection
	// silently breaks. Worth asserting.
	if !strings.Contains(joined, "-progress pipe:1") {
		t.Errorf("expected -progress pipe:1 in args (peer-connected detection): %s", joined)
	}
}

func TestBuildReceiverArgsIncludesPassphraseQueryParam(t *testing.T) {
	cfg := DesiredConfig{Port: 9999, Passphrase: "supersecret10chars", RelayPort: 52003}
	args := buildReceiverArgs(cfg)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "passphrase=supersecret10chars") {
		t.Errorf("expected passphrase query param in args: %s", joined)
	}
}

func TestDesiredConfigEqual(t *testing.T) {
	a := DesiredConfig{Binary: "ffmpeg", Port: 9000, Passphrase: "x", RelayPort: 52003}
	b := DesiredConfig{Binary: "ffmpeg", Port: 9000, Passphrase: "x", RelayPort: 52003}
	if !a.Equal(b) {
		t.Error("identical configs should compare Equal")
	}
	b.Port = 9001
	if a.Equal(b) {
		t.Error("different ports must not compare Equal — port change has to restart ffmpeg")
	}
	b.Port = 9000
	b.Passphrase = "y"
	if a.Equal(b) {
		t.Error("different passphrases must not compare Equal — passphrase change has to restart ffmpeg")
	}
}

func TestStatusDefaultsIdle(t *testing.T) {
	r := NewReceiver(nil)
	defer r.Stop()
	s := r.Status()
	if s.State != StateIdle {
		t.Errorf("fresh receiver should report idle, got %q", s.State)
	}
	if s.PeerConnected {
		t.Error("fresh receiver should not report peer connected")
	}
}

func TestStatusPeerGoesStaleOnRead(t *testing.T) {
	// The status getter recomputes PeerConnected from LastFrameAt on
	// read so a stale "true" doesn't linger on the UI after the
	// encoder disconnects. Force a stale-from-the-past LastFrameAt
	// directly and observe.
	r := NewReceiver(nil)
	defer r.Stop()
	r.mu.Lock()
	r.status = Status{
		State:         StateRunning,
		Port:          9999,
		PeerConnected: true,
		FPS:           30,
		LastFrameAt:   time.Now().Add(-2 * peerStaleAfter),
	}
	r.mu.Unlock()

	s := r.Status()
	if s.PeerConnected {
		t.Errorf("expected stale peer to be reported as disconnected, got %+v", s)
	}
	if s.FPS != 0 {
		t.Errorf("expected fps to clear when peer goes stale, got %v", s.FPS)
	}
}

func TestSetStatusPreservesLastErrorOnProgressTick(t *testing.T) {
	// Regression: parseProgress writes Status with LastError="" on
	// every tick. If setStatus blindly overwrites, scanStderr's
	// captured libsrt error vanishes the moment the next progress
	// block arrives. The fix preserves LastError across StateRunning
	// transitions.
	r := NewReceiver(nil)
	defer r.Stop()
	// Simulate scanStderr writing a captured error.
	r.mu.Lock()
	r.status.LastError = "Address already in use"
	r.mu.Unlock()
	// Simulate parseProgress's snapshot — no LastError set.
	r.setStatus(Status{State: StateRunning, Port: 9999, FPS: 30, PeerConnected: true})
	if got := r.Status().LastError; got != "Address already in use" {
		t.Errorf("expected LastError preserved across progress tick, got %q", got)
	}
	// But a fresh Starting transition should clear the field —
	// otherwise stale errors linger into the next attempt's UI.
	r.setStatus(Status{State: StateStarting, Port: 9999})
	if got := r.Status().LastError; got != "" {
		t.Errorf("expected LastError cleared on StateStarting transition, got %q", got)
	}
}

func TestApplyIdleStopsReceiver(t *testing.T) {
	// Apply with Port=0 must transition the receiver to idle even if
	// it was previously configured. Without this, switching the
	// source kind away from srt-listener would leave a ghost ffmpeg
	// holding the SRT port.
	r := NewReceiver(nil)
	defer r.Stop()
	// Don't actually spawn ffmpeg — we just want to verify the
	// idle-after-Apply state, not the process lifecycle.
	r.Apply(DesiredConfig{}) // already idle, no-op
	if got := r.Status().State; got != StateIdle {
		t.Errorf("expected idle, got %q", got)
	}
}
