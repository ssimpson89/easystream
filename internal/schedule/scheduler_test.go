package schedule

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeStreamController struct {
	mu             sync.Mutex
	started        bool
	presetID       string
	ingestURL      string
	streamKey      string
	broadcastID    string
	streamID       string
	startCalls     int
	preflightErr   error
	preflightCalls int
}

func (f *fakeStreamController) StartWithIngest(presetID, ingestURL, streamKey, broadcastID, streamID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = true
	f.startCalls++
	f.presetID = presetID
	f.ingestURL = ingestURL
	f.streamKey = streamKey
	f.broadcastID = broadcastID
	f.streamID = streamID
	return nil
}

func (f *fakeStreamController) StopStream() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = false
}

func (f *fakeStreamController) IsStreaming() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started
}

func (f *fakeStreamController) Preflight() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.preflightCalls++
	return f.preflightErr
}

type fakeBroadcastController struct {
	mu                   sync.Mutex
	createBroadcastCalls int
	createStreamCalls    int
	transitionCalls      int
	cancelCalls          int
	completeCalls        int
}

func (f *fakeBroadcastController) IsAuthenticated() bool { return true }

func (f *fakeBroadcastController) CreateBroadcast(ctx context.Context, title, description string, scheduledStart time.Time, privacy string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createBroadcastCalls++
	return "broadcast-1", nil
}

func (f *fakeBroadcastController) CreateBoundStream(ctx context.Context, broadcastID, presetID string) (string, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createStreamCalls++
	return "stream-1", "rtmp://example/live", "key-1", nil
}

func (f *fakeBroadcastController) StartTransitionToLive(broadcastID, streamID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transitionCalls++
}

func (f *fakeBroadcastController) CancelTransition() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelCalls++
}

func (f *fakeBroadcastController) CompleteBroadcast(broadcastID, streamID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completeCalls++
}

// newTestScheduler builds a Scheduler with zero preroll so tests fire
// the lifecycle in a single tick. Prep lead is now per-event (set on
// the Schedule/Override), so it doesn't pass through here.
func newTestScheduler(t *testing.T, store *Store, stream StreamController, broadcast BroadcastController) *Scheduler {
	t.Helper()
	return NewSchedulerWithPreroll(store, stream, broadcast,
		log.New(testWriter{t}, "", 0), 0)
}

func TestSchedulerStartsDueEvent(t *testing.T) {
	store, _, err := NewStore(t.TempDir() + "/schedules.json")
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
	yt := &fakeBroadcastController{}
	s := newTestScheduler(t, store, stream, yt)
	s.tick(context.Background())

	if !stream.IsStreaming() {
		t.Fatal("expected due event to start")
	}
	if stream.broadcastID != "broadcast-1" || stream.streamID != "stream-1" {
		t.Fatalf("unexpected IDs: broadcast=%q stream=%q", stream.broadcastID, stream.streamID)
	}
	if stream.ingestURL != "rtmp://example/live" || stream.streamKey != "key-1" {
		t.Fatalf("unexpected ingest: url=%q key=%q", stream.ingestURL, stream.streamKey)
	}
	yt.mu.Lock()
	defer yt.mu.Unlock()
	if yt.createBroadcastCalls != 1 || yt.createStreamCalls != 1 || yt.transitionCalls != 1 {
		t.Fatalf("unexpected call counts: create=%d stream=%d transition=%d",
			yt.createBroadcastCalls, yt.createStreamCalls, yt.transitionCalls)
	}
}

// TestSchedulerJITDefaultDoesNotPrepareEarly locks in the contract
// that the default (PrepLeadMinutes=0) creates the YouTube broadcast
// only at StartTime — no scheduled-broadcast indicator appearing on
// the channel ahead of the service.
func TestSchedulerJITDefaultDoesNotPrepareEarly(t *testing.T) {
	store, _, err := NewStore(t.TempDir() + "/schedules.json")
	if err != nil {
		t.Fatal(err)
	}
	// No PrepLeadMinutes set → JIT default.
	_, err = store.CreateOverride(Override{
		Name:        "Soon",
		StartTime:   time.Now().UTC().Add(30 * time.Second),
		DurationMin: 30,
		PresetID:    "recommended",
		Title:       "Soon",
		Privacy:     "unlisted",
	})
	if err != nil {
		t.Fatal(err)
	}

	stream := &fakeStreamController{}
	yt := &fakeBroadcastController{}
	s := NewScheduler(store, stream, yt, log.New(testWriter{t}, "", 0))
	s.tick(context.Background())

	if stream.IsStreaming() {
		t.Fatal("did not expect future event to start before scheduled time")
	}
	yt.mu.Lock()
	defer yt.mu.Unlock()
	if yt.createBroadcastCalls != 0 {
		t.Fatalf("JIT default: did not expect broadcast to be pre-created, got %d creates", yt.createBroadcastCalls)
	}
}

// TestSchedulerHonorsPerEventPrepLead is the opt-in case: an operator
// who set PrepLeadMinutes on the override gets the broadcast created
// inside that window. FFmpeg still does not start until preroll.
func TestSchedulerHonorsPerEventPrepLead(t *testing.T) {
	store, _, err := NewStore(t.TempDir() + "/schedules.json")
	if err != nil {
		t.Fatal(err)
	}
	// Event starts in 30 seconds; per-event prep lead is 1 minute, so
	// "now" is inside the prep window.
	_, err = store.CreateOverride(Override{
		Name:            "Soon",
		StartTime:       time.Now().UTC().Add(30 * time.Second),
		DurationMin:     30,
		PrepLeadMinutes: 1,
		PresetID:        "recommended",
		Title:           "Soon",
		Privacy:         "unlisted",
	})
	if err != nil {
		t.Fatal(err)
	}
	stream := &fakeStreamController{}
	yt := &fakeBroadcastController{}
	// Preroll=0 so the test doesn't have to wait for the normal warm-up.
	s := newTestScheduler(t, store, stream, yt)
	s.tick(context.Background())

	if stream.IsStreaming() {
		t.Fatal("expected FFmpeg NOT to start yet — still before StartTime")
	}
	yt.mu.Lock()
	defer yt.mu.Unlock()
	if yt.createBroadcastCalls != 1 {
		t.Fatalf("expected broadcast to be pre-created from per-event lead, got %d", yt.createBroadcastCalls)
	}
	if yt.transitionCalls != 0 {
		t.Fatalf("transition should not start until preroll: got %d", yt.transitionCalls)
	}
}

// TestSchedulerEventsWithIndependentPrepLeads verifies one event with a
// lead and one without (both due "in the prep window of the leaded one")
// behave independently — only the leaded event pre-creates.
func TestSchedulerEventsWithIndependentPrepLeads(t *testing.T) {
	store, _, err := NewStore(t.TempDir() + "/schedules.json")
	if err != nil {
		t.Fatal(err)
	}
	soon := time.Now().UTC().Add(30 * time.Second)
	// Override A: has a 1-min lead → inside prep window now.
	_, err = store.CreateOverride(Override{
		Name: "With lead", StartTime: soon, DurationMin: 30,
		PrepLeadMinutes: 1, PresetID: "recommended", Title: "A", Privacy: "unlisted",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Override B: no lead → JIT, not yet due.
	_, err = store.CreateOverride(Override{
		Name: "JIT", StartTime: soon, DurationMin: 30,
		PresetID: "recommended", Title: "B", Privacy: "unlisted",
	})
	if err != nil {
		t.Fatal(err)
	}
	stream := &fakeStreamController{}
	yt := &fakeBroadcastController{}
	s := newTestScheduler(t, store, stream, yt)
	s.tick(context.Background())

	yt.mu.Lock()
	defer yt.mu.Unlock()
	if yt.createBroadcastCalls != 1 {
		t.Fatalf("expected exactly one pre-create (the leaded event), got %d", yt.createBroadcastCalls)
	}
}

func TestSchedulerStartsRecurringEventThatJustBecameDue(t *testing.T) {
	store, _, err := NewStore(t.TempDir() + "/schedules.json")
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
	yt := &fakeBroadcastController{}
	s := newTestScheduler(t, store, stream, yt)
	s.tick(context.Background())

	if !stream.IsStreaming() {
		t.Fatal("expected recurring event in its active window to start")
	}
	yt.mu.Lock()
	defer yt.mu.Unlock()
	if yt.createBroadcastCalls != 1 {
		t.Fatalf("expected one broadcast create, got %d", yt.createBroadcastCalls)
	}
}

func TestPausedScheduleDoesNotCreateUpcomingEvent(t *testing.T) {
	store, _, err := NewStore(t.TempDir() + "/schedules.json")
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
	store, _, err := NewStore(t.TempDir() + "/schedules.json")
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
	yt := &fakeBroadcastController{}
	s := newTestScheduler(t, store, stream, yt)
	s.tick(context.Background())
	if !stream.IsStreaming() {
		t.Fatal("expected first tick to start active occurrence")
	}
	s.StopActive()
	if stream.IsStreaming() {
		t.Fatal("expected StopActive to stop stream")
	}
	s.tick(context.Background())
	if stream.IsStreaming() {
		t.Fatal("did not expect stopped occurrence to restart inside same event window")
	}
	yt.mu.Lock()
	defer yt.mu.Unlock()
	if yt.createBroadcastCalls != 1 {
		t.Fatalf("expected exactly one broadcast creation, got %d", yt.createBroadcastCalls)
	}
	if yt.cancelCalls < 1 {
		t.Fatalf("expected at least one cancel call, got %d", yt.cancelCalls)
	}
	if yt.completeCalls != 1 {
		t.Fatalf("expected exactly one complete call, got %d", yt.completeCalls)
	}
}

// failingBroadcastController simulates a YouTube API outage on
// CreateBroadcast so we can verify the backoff doesn't create one
// orphan broadcast per tick.
type failingBroadcastController struct {
	mu            sync.Mutex
	attempts      int
	transitionMu  sync.Mutex
	cancelCalls   int
	completeCalls int
}

func (f *failingBroadcastController) IsAuthenticated() bool { return true }
func (f *failingBroadcastController) CreateBroadcast(ctx context.Context, title, description string, scheduledStart time.Time, privacy string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	return "", contextErr{}
}
func (f *failingBroadcastController) CreateBoundStream(ctx context.Context, broadcastID, presetID string) (string, string, string, error) {
	return "", "", "", contextErr{}
}
func (f *failingBroadcastController) StartTransitionToLive(broadcastID, streamID string) {}
func (f *failingBroadcastController) CancelTransition() {
	f.transitionMu.Lock()
	f.cancelCalls++
	f.transitionMu.Unlock()
}
func (f *failingBroadcastController) CompleteBroadcast(broadcastID, streamID string) {
	f.transitionMu.Lock()
	f.completeCalls++
	f.transitionMu.Unlock()
}

type contextErr struct{}

func (contextErr) Error() string { return "youtube api down" }

func TestSchedulerBacksOffOnPrepFailure(t *testing.T) {
	store, _, err := NewStore(t.TempDir() + "/schedules.json")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateOverride(Override{
		Name:        "Soon",
		StartTime:   time.Now().UTC().Add(-1 * time.Minute),
		DurationMin: 30,
		PresetID:    "recommended",
		Title:       "Soon",
		Privacy:     "unlisted",
	})
	if err != nil {
		t.Fatal(err)
	}
	stream := &fakeStreamController{}
	yt := &failingBroadcastController{}
	s := newTestScheduler(t, store, stream, yt)

	// Call tick many times — backoff should keep create attempts down.
	for i := 0; i < 50; i++ {
		s.tick(context.Background())
	}
	yt.mu.Lock()
	attempts := yt.attempts
	yt.mu.Unlock()
	// With 5s minimum backoff and zero real time elapsed, we expect at
	// most a small handful of attempts — definitely not 50.
	if attempts > 5 {
		t.Fatalf("expected backoff to limit attempts, got %d", attempts)
	}
	if attempts < 1 {
		t.Fatalf("expected at least one attempt, got %d", attempts)
	}
}

func TestSchedulerRefusesToGoLiveWhenPreflightFails(t *testing.T) {
	store, _, err := NewStore(t.TempDir() + "/schedules.json")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateOverride(Override{
		Name:        "Wrong source",
		StartTime:   time.Now().UTC().Add(-1 * time.Minute),
		DurationMin: 30,
		PresetID:    "recommended",
		Title:       "Wrong source",
		Privacy:     "unlisted",
	})
	if err != nil {
		t.Fatal(err)
	}

	stream := &fakeStreamController{
		preflightErr: errors.New("video source: configured AVFoundation device not found"),
	}
	yt := &fakeBroadcastController{}
	s := newTestScheduler(t, store, stream, yt)
	s.tick(context.Background())

	// FFmpeg must NOT have been started, because the configured source
	// is missing. The whole point of the preflight fix.
	if stream.IsStreaming() {
		t.Fatal("expected stream NOT to start when preflight fails")
	}
	if stream.startCalls != 0 {
		t.Fatalf("expected 0 StartWithIngest calls, got %d", stream.startCalls)
	}
	stream.mu.Lock()
	pc := stream.preflightCalls
	stream.mu.Unlock()
	if pc == 0 {
		t.Fatal("expected scheduler to call Preflight before going live")
	}
	// Status should surface the error so the operator sees what's wrong.
	if got := s.Status().LastError; got == "" {
		t.Fatal("expected scheduler status to report the preflight error")
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
