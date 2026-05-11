package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStreamIntent_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "intent.json")

	original := streamIntent{
		Live:        true,
		Mode:        "scheduled",
		BroadcastID: "abc123",
		StreamID:    "def456",
		StartedAt:   time.Now().UTC().Truncate(time.Second),
	}
	if err := saveStreamIntent(path, original); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := loadStreamIntent(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Live != original.Live || got.Mode != original.Mode ||
		got.BroadcastID != original.BroadcastID || got.StreamID != original.StreamID ||
		!got.StartedAt.Equal(original.StartedAt) {
		t.Fatalf("round-trip mismatch:\ngot:  %+v\nwant: %+v", got, original)
	}
}

func TestStreamIntent_MissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := loadStreamIntent(filepath.Join(dir, "nope.json"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("expected os.IsNotExist, got %v", err)
	}
}

func TestStreamIntent_Clear(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "intent.json")
	if err := saveStreamIntent(path, streamIntent{Live: true, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	clearStreamIntent(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected file to be removed, got err=%v", err)
	}
	// Idempotent.
	clearStreamIntent(path)
}

func TestStreamIntent_Fresh(t *testing.T) {
	cases := []struct {
		name string
		i    streamIntent
		want bool
	}{
		{"zero time", streamIntent{Live: true}, false},
		{"recent", streamIntent{Live: true, StartedAt: time.Now().Add(-30 * time.Minute)}, true},
		{"just under cap", streamIntent{Live: true, StartedAt: time.Now().Add(-(maxIntentAge - time.Minute))}, true},
		{"over cap", streamIntent{Live: true, StartedAt: time.Now().Add(-(maxIntentAge + time.Minute))}, false},
		{"way too old", streamIntent{Live: true, StartedAt: time.Now().Add(-72 * time.Hour)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.i.fresh(); got != tc.want {
				t.Errorf("fresh() = %v, want %v", got, tc.want)
			}
		})
	}
}
