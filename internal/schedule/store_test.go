package schedule

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewStoreCorruptFileMovedAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schedules.json")
	// Half-written JSON simulating a power loss before atomic write replaced it.
	if err := os.WriteFile(path, []byte(`{"schedules":[{"id":"x"`), 0600); err != nil {
		t.Fatal(err)
	}
	store, recovery, err := NewStore(path)
	if err != nil {
		t.Fatalf("expected recovery, got fatal: %v", err)
	}
	if recovery == nil {
		t.Fatal("expected recovery warning")
	}
	if store == nil {
		t.Fatal("expected a fresh store after recovery")
	}
	if got := store.Schedules(); len(got) != 0 {
		t.Fatalf("expected empty fresh store, got %d schedules", len(got))
	}
	// Backup file should exist.
	entries, _ := os.ReadDir(dir)
	found := false
	for _, e := range entries {
		if strings.Contains(e.Name(), ".corrupt-") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected .corrupt- sidecar file to exist")
	}
}

func TestDeleteScheduleClearsBroadcastMappings(t *testing.T) {
	store, _, err := NewStore(t.TempDir() + "/schedules.json")
	if err != nil {
		t.Fatal(err)
	}
	sched, err := store.CreateSchedule(Schedule{
		Name:        "Cleanup",
		Days:        []string{"monday"},
		Time:        "17:42",
		Timezone:    "UTC",
		DurationMin: 120,
		Title:       "Cleanup",
		Privacy:     "unlisted",
		Enabled:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	key := EventKey(sched.ID, time.Date(2026, 5, 11, 17, 42, 0, 0, time.UTC))
	if err := store.SetBroadcastID(key, "broadcast-1", "stream-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSchedule(sched.ID); err != nil {
		t.Fatal(err)
	}
	if broadcast, stream := store.GetBroadcastID(key); broadcast != "" || stream != "" {
		t.Fatalf("expected mappings to be cleared, got broadcast=%q stream=%q", broadcast, stream)
	}
}

// TestLoadOldScheduleFileTreatsMissingPrepLeadAsJIT locks in the
// migration contract: schedules.json files written before
// PrepLeadMinutes existed must decode with PrepLeadMinutes=0 (JIT),
// NOT inherit the old 15-minute global default. Without this guard,
// dropping the global default to 0 would have no effect on persisted
// schedules and the scheduled-broadcast-appears-early bug would
// silently survive an upgrade.
func TestLoadOldScheduleFileTreatsMissingPrepLeadAsJIT(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schedules.json")
	old := `{
		"schedules": [{
			"id": "legacy-1",
			"name": "Sunday service",
			"days": ["sunday"],
			"time": "10:00",
			"timezone": "America/Chicago",
			"durationMin": 120,
			"presetId": "recommended",
			"title": "Sunday service",
			"privacy": "unlisted",
			"enabled": true
		}],
		"overrides": [{
			"id": "legacy-2",
			"name": "Christmas",
			"startTime": "2026-12-24T16:00:00Z",
			"durationMin": 90,
			"presetId": "recommended",
			"title": "Christmas Eve",
			"privacy": "unlisted"
		}]
	}`
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	store, _, err := NewStore(path)
	if err != nil {
		t.Fatalf("expected clean load, got %v", err)
	}
	scheds := store.Schedules()
	if len(scheds) != 1 {
		t.Fatalf("expected 1 schedule, got %d", len(scheds))
	}
	if scheds[0].PrepLeadMinutes != 0 {
		t.Errorf("legacy schedule must decode as PrepLeadMinutes=0 (JIT), got %d", scheds[0].PrepLeadMinutes)
	}
	overrides := store.Overrides()
	if len(overrides) != 1 {
		t.Fatalf("expected 1 override, got %d", len(overrides))
	}
	if overrides[0].PrepLeadMinutes != 0 {
		t.Errorf("legacy override must decode as PrepLeadMinutes=0 (JIT), got %d", overrides[0].PrepLeadMinutes)
	}
}

func TestDeleteOverrideClearsBroadcastMappings(t *testing.T) {
	store, _, err := NewStore(t.TempDir() + "/schedules.json")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 5, 11, 17, 42, 0, 0, time.UTC)
	o, err := store.CreateOverride(Override{
		Name:        "Cleanup",
		StartTime:   start,
		DurationMin: 120,
		Title:       "Cleanup",
		Privacy:     "unlisted",
	})
	if err != nil {
		t.Fatal(err)
	}
	key := EventKey(o.ID, start)
	if err := store.SetBroadcastID(key, "broadcast-1", "stream-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteOverride(o.ID); err != nil {
		t.Fatal(err)
	}
	if broadcast, stream := store.GetBroadcastID(key); broadcast != "" || stream != "" {
		t.Fatalf("expected mappings to be cleared, got broadcast=%q stream=%q", broadcast, stream)
	}
}
