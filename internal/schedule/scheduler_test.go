package schedule

import (
	"log"
	"strings"
	"testing"
	"time"
)

type fakeStreamController struct {
	started     bool
	presetID    string
	ingestURL   string
	streamKey   string
	broadcastID string
	streamID    string
}

func (f *fakeStreamController) StartWithIngest(presetID, ingestURL, streamKey, broadcastID, streamID string) error {
	f.started = true
	f.presetID = presetID
	f.ingestURL = ingestURL
	f.streamKey = streamKey
	f.broadcastID = broadcastID
	f.streamID = streamID
	return nil
}

func (f *fakeStreamController) StopStream() { f.started = false }

func (f *fakeStreamController) IsStreaming() bool { return f.started }

type fakeYouTubeController struct {
	created int
	bound   int
}

func (f *fakeYouTubeController) IsAuthenticated() bool { return true }

func (f *fakeYouTubeController) CreateBroadcast(title, description string, scheduledStart time.Time, privacy string) (string, error) {
	f.created++
	return "broadcast-1", nil
}

func (f *fakeYouTubeController) EnsureStream(presetID string) (string, string, string, error) {
	return "stream-1", "rtmp://example/live", "key-1", nil
}

func (f *fakeYouTubeController) BindBroadcast(broadcastID, streamID string) error {
	f.bound++
	return nil
}

func (f *fakeYouTubeController) TransitionBroadcast(broadcastID, status string) error { return nil }

func TestSchedulerStartsDueEventWithoutThirtyMinutePreparation(t *testing.T) {
	store, err := NewStore(t.TempDir() + "/schedules.json")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateOverride(Override{
		Name:        "Due now",
		StartTime:   time.Now().UTC().Add(-1 * time.Minute),
		DurationMin: 30,
		PresetID:    "recommended",
		Title:       "Due now",
		Privacy:     "unlisted",
	})
	if err != nil {
		t.Fatal(err)
	}

	stream := &fakeStreamController{}
	yt := &fakeYouTubeController{}
	s := NewScheduler(store, stream, yt, log.New(testWriter{t}, "", 0))
	s.tick()

	if !stream.started {
		t.Fatal("expected due event to start without prior 30-minute broadcast preparation")
	}
	if stream.broadcastID != "broadcast-1" || stream.streamID != "stream-1" {
		t.Fatalf("unexpected IDs: broadcast=%q stream=%q", stream.broadcastID, stream.streamID)
	}
	if stream.ingestURL != "rtmp://example/live" || stream.streamKey != "key-1" {
		t.Fatalf("unexpected ingest: url=%q key=%q", stream.ingestURL, stream.streamKey)
	}
	if yt.created != 1 || yt.bound != 1 {
		t.Fatalf("expected one create and bind, got create=%d bind=%d", yt.created, yt.bound)
	}
}

func TestSchedulerDoesNotPrepareFutureEventEarly(t *testing.T) {
	store, err := NewStore(t.TempDir() + "/schedules.json")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateOverride(Override{
		Name:        "Soon",
		StartTime:   time.Now().UTC().Add(10 * time.Minute),
		DurationMin: 30,
		PresetID:    "recommended",
		Title:       "Soon",
		Privacy:     "unlisted",
	})
	if err != nil {
		t.Fatal(err)
	}

	stream := &fakeStreamController{}
	yt := &fakeYouTubeController{}
	s := NewScheduler(store, stream, yt, log.New(testWriter{t}, "", 0))
	s.tick()

	if stream.started {
		t.Fatal("did not expect future event to start before scheduled time")
	}
	if yt.created != 0 || yt.bound != 0 {
		t.Fatalf("did not expect future event to be prepared early, got create=%d bind=%d", yt.created, yt.bound)
	}
}

func TestSchedulerStartsRecurringEventThatJustBecameDue(t *testing.T) {
	store, err := NewStore(t.TempDir() + "/schedules.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	start := now.Add(-1 * time.Minute)
	_, err = store.CreateSchedule(Schedule{
		Name:        "Due recurring",
		Days:        []string{strings.ToLower(start.Weekday().String())},
		Time:        start.Format("15:04"),
		Timezone:    "UTC",
		DurationMin: 30,
		PresetID:    "recommended",
		Title:       "Due recurring",
		Privacy:     "unlisted",
		Enabled:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	stream := &fakeStreamController{}
	yt := &fakeYouTubeController{}
	s := NewScheduler(store, stream, yt, log.New(testWriter{t}, "", 0))
	s.tick()

	if !stream.started {
		t.Fatal("expected recurring event in its active window to start")
	}
	if yt.created != 1 || yt.bound != 1 {
		t.Fatalf("expected one create and bind, got create=%d bind=%d", yt.created, yt.bound)
	}
}

func TestPausedScheduleDoesNotCreateUpcomingEvent(t *testing.T) {
	store, err := NewStore(t.TempDir() + "/schedules.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	start := now.Add(-1 * time.Minute)
	_, err = store.CreateSchedule(Schedule{
		Name:        "Paused recurring",
		Days:        []string{strings.ToLower(start.Weekday().String())},
		Time:        start.Format("15:04"),
		Timezone:    "UTC",
		DurationMin: 30,
		PresetID:    "recommended",
		Title:       "Paused recurring",
		Privacy:     "unlisted",
		Enabled:     false,
	})
	if err != nil {
		t.Fatal(err)
	}

	if events := store.NextEvents(10, now); len(events) != 0 {
		t.Fatalf("expected paused schedule to be excluded, got %d events", len(events))
	}
}

func TestManualStopSuppressesCurrentRecurringOccurrence(t *testing.T) {
	store, err := NewStore(t.TempDir() + "/schedules.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	start := now.Add(-1 * time.Minute)
	_, err = store.CreateSchedule(Schedule{
		Name:        "Stop early",
		Days:        []string{strings.ToLower(start.Weekday().String())},
		Time:        start.Format("15:04"),
		Timezone:    "UTC",
		DurationMin: 30,
		PresetID:    "recommended",
		Title:       "Stop early",
		Privacy:     "unlisted",
		Enabled:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	stream := &fakeStreamController{}
	yt := &fakeYouTubeController{}
	s := NewScheduler(store, stream, yt, log.New(testWriter{t}, "", 0))
	s.tick()
	if !stream.started {
		t.Fatal("expected first tick to start active occurrence")
	}
	s.StopActive()
	if stream.started {
		t.Fatal("expected StopActive to stop stream")
	}
	s.tick()
	if stream.started {
		t.Fatal("did not expect stopped occurrence to restart inside same event window")
	}
	if yt.created != 1 {
		t.Fatalf("expected exactly one broadcast creation, got %d", yt.created)
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
