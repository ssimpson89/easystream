package ffmpeg

import (
	"math"
	"testing"
	"time"
)

func TestStatusProgressStalledBeforeFirstProgress(t *testing.T) {
	now := time.Now()
	status := Status{State: StateRunning, StartedAt: now.Add(-30 * time.Second)}
	if !status.progressStalled(20*time.Second, now) {
		t.Fatal("expected startup with no progress to be stalled after threshold")
	}
	if status.progressStalled(40*time.Second, now) {
		t.Fatal("did not expect startup to be stalled before threshold")
	}
}

func TestStatusProgressStalledAfterProgressStops(t *testing.T) {
	now := time.Now()
	status := Status{
		State:     StateRunning,
		StartedAt: now.Add(-2 * time.Minute),
		LastProgress: Progress{
			UpdatedAt: now.Add(-25 * time.Second),
		},
	}
	if !status.progressStalled(20*time.Second, now) {
		t.Fatal("expected stale progress to be stalled")
	}
	status.LastProgress.UpdatedAt = now.Add(-5 * time.Second)
	if status.progressStalled(20*time.Second, now) {
		t.Fatal("fresh progress should not be stalled")
	}
}

func TestParseAudioRMSClampsSilenceToFiniteValue(t *testing.T) {
	rms, ok := parseAudioRMS("lavfi.astats.Overall.RMS_level=-inf")
	if !ok {
		t.Fatal("expected RMS line to parse")
	}
	if math.IsInf(rms, 0) || math.IsNaN(rms) {
		t.Fatalf("expected finite silence value, got %v", rms)
	}
	if rms != -120 {
		t.Fatalf("expected silence floor -120, got %v", rms)
	}
}
