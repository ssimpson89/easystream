package schedule

import (
	"testing"
	"time"
)

func TestDeleteScheduleClearsBroadcastMappings(t *testing.T) {
	store, err := NewStore(t.TempDir() + "/schedules.json")
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

func TestDeleteOverrideClearsBroadcastMappings(t *testing.T) {
	store, err := NewStore(t.TempDir() + "/schedules.json")
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
