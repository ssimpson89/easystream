package schedule

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// Lead times. Tuned for church-service streaming where the
// scheduled-broadcast indicator must NOT appear on the channel ahead
// of the actual service, and we need a brief warmup so YouTube sees
// ingest before transitioning to live.
//
// Prep lead is per-event (Event.PrepLeadMinutes), zero = JIT. There
// is no global DefaultPrepLead constant — an earlier global default
// would surface a "premiering soon" indicator on the channel before
// the service, which the operator never wanted. Per-schedule opt-in
// via Schedule / Override .PrepLeadMinutes lets a specific event
// (e.g. a Christmas Eve service whose watch URL goes in a bulletin)
// carry its own lead time.
const (
	// DefaultPreroll: how far before StartTime to start FFmpeg pushing.
	// 10 s is enough for YouTube's ingest to register and report
	// streamStatus="active" before the scheduled start time, so the
	// transition to "live" can happen at (or very close to) StartTime.
	// Note: viewers don't see anything until we transition to "live".
	DefaultPreroll = 10 * time.Second

	// MaxPrepLead is the upper bound for the operator-configurable
	// per-event prep lead. Anything longer than an hour is almost
	// certainly a typo; reject at validate time with a clear error.
	MaxPrepLead = 60 * time.Minute

	// Scheduler tick interval. Small enough to start within tickInterval
	// of preroll/prep boundaries; large enough not to hammer the store.
	tickInterval = 1 * time.Second
)

// maxPrepLeadMinutes is the upper bound in minute units, derived from
// the canonical MaxPrepLead Duration so the validators and their
// error messages stay in lockstep with the constant. Co-located with
// MaxPrepLead so the duration ↔ int conversion is one read away.
func maxPrepLeadMinutes() int {
	return int(MaxPrepLead / time.Minute)
}

// StreamController is the interface the scheduler uses to start/stop FFmpeg.
type StreamController interface {
	// StartWithIngest starts FFmpeg pushing to the given destination.
	// broadcastID and streamID are recorded so completion logic can run
	// when the stream stops (broadcast → complete, stream → deleted).
	StartWithIngest(presetID, ingestURL, streamKey, broadcastID, streamID string) error
	StopStream()
	IsStreaming() bool
	// Preflight verifies the configured capture source is present and
	// the config is valid for FFmpeg, without spawning anything. The
	// scheduler runs this just before goLive so a missing source aborts
	// the scheduled event with a clear error instead of going live on
	// whatever device happens to be at the stale persisted index.
	Preflight() error
}

// BroadcastController is the YouTube-side lifecycle the scheduler drives.
// Implementations live in the app package (ytControllerAdapter) so the
// schedule package stays free of HTTP/OAuth dependencies.
type BroadcastController interface {
	IsAuthenticated() bool
	// CreateBroadcast creates the YouTube broadcast resource. Called
	// at the prep boundary, which is StartTime - eventPrepLead(event)
	// — zero by default (JIT), set per-event when an operator wants
	// the watch URL available in advance.
	CreateBroadcast(ctx context.Context, title, description string, scheduledStart time.Time, privacy string) (broadcastID string, err error)
	// CreateBoundStream creates a non-reusable stream for this broadcast
	// and binds it. Returns the ingest details.
	CreateBoundStream(ctx context.Context, broadcastID, presetID string) (streamID, ingestURL, streamKey string, err error)
	// StartTransitionToLive kicks off the testing→live transition in the
	// background. Cancellable via CancelTransition.
	StartTransitionToLive(broadcastID, streamID string)
	// CancelTransition aborts any in-flight transition goroutine.
	CancelTransition()
	// CompleteBroadcast transitions the broadcast to "complete" and
	// deletes the bound stream resource. Best-effort; logs but does not
	// surface partial failures.
	CompleteBroadcast(broadcastID, streamID string)
}

// Scheduler drives the event lifecycle: prepare → preroll → live → stop.
type Scheduler struct {
	store     *Store
	stream    StreamController
	broadcast BroadcastController
	logger    *log.Logger

	// preroll is overridable for tests; the prep lead is per-event
	// (Event.PrepLeadMinutes), not a global, so there's no prepLead
	// field on the Scheduler anymore.
	preroll time.Duration

	mu              sync.Mutex
	activeEvent     *Event // currently live event
	activeBroadcast string // YouTube broadcast ID of active event
	activeStream    string // YouTube stream ID of active event
	extraMinutes    int    // extra minutes added to active event by Extend button
	lastError       string

	// prepFailures tracks consecutive prepare failures per event key so
	// transient YouTube outages back off instead of creating an orphan
	// broadcast every tick.
	prepFailures map[string]*backoffState

	// ingestCache maps eventKey → ingest URL+key. The stream key is
	// intentionally NOT persisted to the on-disk store. We re-create the
	// stream on app restart if the cache is cold.
	ingestCache map[string]ingestDetails

	cancel context.CancelFunc
	done   chan struct{}
}

type ingestDetails struct{ url, key string }

type backoffState struct {
	count   int
	nextTry time.Time
}

// NewScheduler creates a scheduler with the default preroll
// (DefaultPreroll). Prep lead is per-event (Event.PrepLeadMinutes,
// 0 = JIT) so the scheduler itself doesn't carry a global default.
// The scheduler checks events every tickInterval (1s) so it can hit
// the preroll boundary within one tick.
func NewScheduler(store *Store, stream StreamController, broadcast BroadcastController, logger *log.Logger) *Scheduler {
	return NewSchedulerWithPreroll(store, stream, broadcast, logger, DefaultPreroll)
}

// NewSchedulerWithPreroll is like NewScheduler but takes a custom
// preroll. Use this from tests so you don't have to wait the full 10 s
// for real ingest warm-up. Per-event prep lead is set on the Schedule /
// Override structs and doesn't pass through here.
func NewSchedulerWithPreroll(store *Store, stream StreamController, broadcast BroadcastController, logger *log.Logger, preroll time.Duration) *Scheduler {
	return &Scheduler{
		store:        store,
		stream:       stream,
		broadcast:    broadcast,
		logger:       logger,
		preroll:      preroll,
		prepFailures: make(map[string]*backoffState),
		ingestCache:  make(map[string]ingestDetails),
	}
}

// SchedulerStatus is returned by the status API.
type SchedulerStatus struct {
	Running         bool      `json:"running"`
	ActiveEventName string    `json:"activeEventName,omitempty"`
	ActiveBroadcast string    `json:"activeBroadcastId,omitempty"`
	ActiveEndsAt    time.Time `json:"activeEndsAt,omitempty"`
	ExtraMinutes    int       `json:"extraMinutes,omitempty"`
	LastError       string    `json:"lastError,omitempty"`
}

// Status returns the scheduler's current state.
func (s *Scheduler) Status() SchedulerStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := SchedulerStatus{
		Running:   s.cancel != nil,
		LastError: s.lastError,
	}
	if s.activeEvent != nil {
		st.ActiveEventName = s.activeEvent.Name
		st.ActiveBroadcast = s.activeBroadcast
		st.ExtraMinutes = s.extraMinutes
		dur := time.Duration(s.activeEvent.DurationMin+s.extraMinutes) * time.Minute
		st.ActiveEndsAt = s.activeEvent.StartTime.Add(dur)
	}
	return st
}

// Extend adds extra minutes to the currently-active event's effective duration.
// Use this when a service runs long — pushes the auto-stop time out without
// stopping/restarting the stream. Returns the new end time.
func (s *Scheduler) Extend(minutes int) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeEvent == nil {
		return time.Time{}, fmt.Errorf("no active scheduled event to extend")
	}
	if minutes <= 0 {
		minutes = 15
	}
	s.extraMinutes += minutes
	dur := time.Duration(s.activeEvent.DurationMin+s.extraMinutes) * time.Minute
	return s.activeEvent.StartTime.Add(dur), nil
}

// Start begins the background scheduler loop.
func (s *Scheduler) Start() {
	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	s.mu.Unlock()

	go s.run(ctx)
}

// Stop halts the scheduler.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	done := s.done
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (s *Scheduler) run(ctx context.Context) {
	defer func() {
		s.mu.Lock()
		s.cancel = nil
		done := s.done
		s.done = nil
		s.mu.Unlock()
		if done != nil {
			close(done)
		}
	}()

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	// Run immediately on start so the first tick isn't delayed by tickInterval.
	s.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// tick runs the lifecycle state machine once. It is structured as:
//  1. End the active event if it has run past its window.
//  2. Look at the next few upcoming events; for each, advance whichever
//     phase is due (prepare → preroll → transition).
//
// The function holds s.mu only for very short read/write segments —
// long-running operations (API calls, FFmpeg start) happen lock-free.
func (s *Scheduler) tick(ctx context.Context) {
	now := time.Now().UTC()

	// Stop active event if past end.
	s.mu.Lock()
	active := s.activeEvent
	activeBroadcast := s.activeBroadcast
	activeStream := s.activeStream
	extra := s.extraMinutes
	s.mu.Unlock()

	if active != nil {
		endTime := active.StartTime.Add(time.Duration(active.DurationMin+extra) * time.Minute)
		if now.After(endTime) {
			s.stopActiveEvent(activeBroadcast, activeStream, false)
		}
	}

	if s.broadcast == nil || !s.broadcast.IsAuthenticated() {
		return
	}

	// Drive lifecycle for upcoming events.
	for _, event := range s.store.NextEvents(10, now) {
		if ctx.Err() != nil {
			return
		}
		s.advanceEvent(ctx, event, now)
	}
}

// eventPrepLead returns the per-event prep lead as a Duration. Zero
// means "create the broadcast at StartTime" (JIT — the default).
//
// Two-layer bounds:
//
//  1. normalizeSchedule / normalizeOverride reject out-of-range values
//     at API time with a clear error, so the operator finds out
//     immediately. That's the authoritative gate.
//  2. This function clamps anything that slipped past the normalizer
//     (a hand-edited schedules.json, a config produced by a future
//     version with a wider bound, a future code path that builds an
//     Event without going through the normalizer) to [0, MaxPrepLead].
//     The scheduler should never create a broadcast hours in advance
//     just because the upstream gate failed.
//
// Treat this clamp as the real safety net, not as redundancy. Even if
// every current code path is normalized today, the clamp is what
// makes that guarantee robust against future drift.
func eventPrepLead(event Event) time.Duration {
	if event.PrepLeadMinutes <= 0 {
		return 0
	}
	d := time.Duration(event.PrepLeadMinutes) * time.Minute
	if d > MaxPrepLead {
		d = MaxPrepLead
	}
	return d
}

// advanceEvent moves a single event through its lifecycle phases.
func (s *Scheduler) advanceEvent(ctx context.Context, event Event, now time.Time) {
	prepAt := event.StartTime.Add(-eventPrepLead(event))
	prerollAt := event.StartTime.Add(-s.preroll)
	endAt := event.StartTime.Add(time.Duration(event.DurationMin) * time.Minute)

	// Past end: nothing to do.
	if !now.Before(endAt) {
		return
	}

	// Phase 1: prepare. Create the broadcast + stream once the prep
	// window opens and persist the IDs so a restart finds them.
	// Pre-flight the capture source first so we don't create an orphan
	// YouTube broadcast for an event whose camera isn't connected.
	if !now.Before(prepAt) && event.BroadcastID == "" {
		if !s.canTryPrep(event) {
			return
		}
		if err := s.stream.Preflight(); err != nil {
			s.setError(fmt.Sprintf("preflight failed for %q: %v", event.Name, err))
			s.recordPrepFailure(eventKey(event))
			return
		}
		if prepared, ok := s.prepareBroadcast(ctx, event); ok {
			event = prepared
		} else {
			return
		}
	}

	// Phase 2: preroll. Start FFmpeg pushing to the bound stream so
	// YouTube sees ingest and reports streamStatus=active by StartTime.
	if !now.Before(prerollAt) && event.BroadcastID != "" {
		s.mu.Lock()
		alreadyActive := s.activeEvent != nil
		s.mu.Unlock()
		if alreadyActive || s.stream.IsStreaming() {
			return
		}
		s.goLive(ctx, event)
	}
}

// canTryPrep returns true when we haven't recently failed to prepare
// this event. Provides exponential backoff so transient YouTube API
// outages don't create one orphan broadcast every tick.
func (s *Scheduler) canTryPrep(event Event) bool {
	key := eventKey(event)
	s.mu.Lock()
	st, ok := s.prepFailures[key]
	s.mu.Unlock()
	if !ok || st == nil {
		return true
	}
	return time.Now().UTC().After(st.nextTry)
}

func (s *Scheduler) recordPrepFailure(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.prepFailures[key]
	if !ok || st == nil {
		st = &backoffState{}
		s.prepFailures[key] = st
	}
	st.count++
	// Exponential backoff: 5s, 10s, 20s, 40s, 80s … capped at 5 min.
	shift := min(st.count-1, 6)
	delay := time.Duration(1<<shift) * 5 * time.Second
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	st.nextTry = time.Now().UTC().Add(delay)
}

func (s *Scheduler) clearPrepFailure(key string) {
	s.mu.Lock()
	delete(s.prepFailures, key)
	s.mu.Unlock()
}

// prepareBroadcast creates the YouTube broadcast + stream and persists
// the IDs to the store. Returns the updated event and ok=true on success.
// On failure the event is recorded against the backoff state and ok=false.
func (s *Scheduler) prepareBroadcast(ctx context.Context, event Event) (Event, bool) {
	s.logger.Printf("scheduler: preparing broadcast for %q (starts %s)",
		event.Name, event.StartTime.Format(time.RFC822))

	prepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	broadcastID, err := s.broadcast.CreateBroadcast(prepCtx,
		event.Title, event.Description, event.StartTime, event.Privacy)
	if err != nil {
		s.setError(fmt.Sprintf("create broadcast for %q: %v", event.Name, err))
		s.recordPrepFailure(eventKey(event))
		return event, false
	}

	streamID, ingestURL, streamKey, err := s.broadcast.CreateBoundStream(prepCtx, broadcastID, event.PresetID)
	if err != nil {
		s.setError(fmt.Sprintf("create+bind stream for %q: %v", event.Name, err))
		s.recordPrepFailure(eventKey(event))
		return event, false
	}

	key := eventKey(event)
	if err := s.store.SetBroadcastID(key, broadcastID, streamID); err != nil {
		s.setError(fmt.Sprintf("persist broadcast mapping: %v", err))
		return event, false
	}
	event.BroadcastID = broadcastID
	event.StreamID = streamID

	// Stash the ingest details on the event so goLive doesn't have to
	// recreate the stream. Round-trip through the store would persist
	// the key on disk; keep these in-memory only.
	s.cacheIngest(key, ingestURL, streamKey)
	s.clearPrepFailure(key)
	s.setError("")

	s.logger.Printf("scheduler: prepared broadcast %s for %q (watch URL ready)", broadcastID, event.Name)
	return event, true
}

func (s *Scheduler) cacheIngest(eventKey, url, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ingestCache[eventKey] = ingestDetails{url: url, key: key}
}

func (s *Scheduler) lookupIngest(eventKey string) (string, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.ingestCache[eventKey]
	if !ok {
		return "", "", false
	}
	return v.url, v.key, true
}

func (s *Scheduler) clearIngest(eventKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ingestCache, eventKey)
}

func (s *Scheduler) goLive(parent context.Context, event Event) {
	// Preflight the capture source BEFORE creating any YouTube resources
	// or starting FFmpeg. If the configured camera is unplugged or its
	// AVFoundation name no longer matches, refuse to go live rather
	// than streaming from whatever device sits at the stale index.
	if err := s.stream.Preflight(); err != nil {
		s.setError(fmt.Sprintf("preflight failed for %q: %v", event.Name, err))
		s.logger.Printf("scheduler: REFUSING to go live for %q — %v", event.Name, err)
		// Back off this event for 30 s so we don't loop on every tick
		// while the operator fixes the source.
		s.recordPrepFailure(eventKey(event))
		return
	}

	key := eventKey(event)
	ingestURL, streamKey, ok := s.lookupIngest(key)
	if !ok {
		// App restarted between prepare and preroll. Re-create the
		// stream now and re-bind it; the broadcast survived in the store.
		// Derive from the scheduler context so Stop() cancels in-flight
		// API calls instead of blocking for the 30s timeout.
		s.logger.Printf("scheduler: ingest cache miss for %q — recreating stream", event.Name)
		ctx, cancel := context.WithTimeout(parent, 30*time.Second)
		streamID, url, k, err := s.broadcast.CreateBoundStream(ctx, event.BroadcastID, event.PresetID)
		cancel()
		if err != nil {
			s.setError(fmt.Sprintf("recreate stream for %q: %v", event.Name, err))
			return
		}
		if err := s.store.SetBroadcastID(key, event.BroadcastID, streamID); err != nil {
			s.setError(fmt.Sprintf("persist re-bound stream: %v", err))
			return
		}
		event.StreamID = streamID
		ingestURL = url
		streamKey = k
		s.cacheIngest(key, ingestURL, streamKey)
	}

	s.logger.Printf("scheduler: starting FFmpeg for %q (broadcast %s, scheduled %s)",
		event.Name, event.BroadcastID, event.StartTime.Format(time.RFC822))

	if err := s.stream.StartWithIngest(event.PresetID, ingestURL, streamKey, event.BroadcastID, event.StreamID); err != nil {
		s.setError(fmt.Sprintf("start stream for %q: %v", event.Name, err))
		return
	}

	s.mu.Lock()
	s.activeEvent = &event
	s.activeBroadcast = event.BroadcastID
	s.activeStream = event.StreamID
	s.lastError = ""
	s.mu.Unlock()

	// The host (Server) drives the testing→live transition. It owns the
	// cancellable goroutine and the YouTube health-polling needed to gate
	// the transition on streamStatus=="active".
	s.broadcast.StartTransitionToLive(event.BroadcastID, event.StreamID)
}

func (s *Scheduler) stopActiveEvent(broadcastID, streamID string, suppress bool) {
	s.logger.Printf("scheduler: stopping active event (broadcast %s)", broadcastID)

	// Cancel the in-flight transition first so it can't fire after we
	// transition the broadcast to complete.
	s.broadcast.CancelTransition()
	s.stream.StopStream()

	if broadcastID != "" {
		s.broadcast.CompleteBroadcast(broadcastID, streamID)
	}

	s.mu.Lock()
	event := s.activeEvent
	extra := s.extraMinutes
	s.activeEvent = nil
	s.activeBroadcast = ""
	s.activeStream = ""
	s.extraMinutes = 0
	s.mu.Unlock()

	if event != nil {
		key := eventKey(*event)
		s.clearIngest(key)
		if suppress {
			until := event.StartTime.Add(time.Duration(event.DurationMin+extra) * time.Minute)
			if err := s.store.SkipEvent(key, until); err != nil {
				s.logger.Printf("scheduler: suppress stopped event: %v", err)
			}
		} else {
			_ = s.store.ClearBroadcast(key)
		}
	}
}

// StopActive manually stops any active scheduled event.
func (s *Scheduler) StopActive() {
	s.mu.Lock()
	broadcast := s.activeBroadcast
	stream := s.activeStream
	s.mu.Unlock()
	s.stopActiveEvent(broadcast, stream, true)
}

func (s *Scheduler) setError(msg string) {
	s.mu.Lock()
	s.lastError = msg
	s.mu.Unlock()
	if msg != "" {
		s.logger.Printf("scheduler: error: %s", msg)
	}
}

func eventKey(event Event) string {
	id := event.ScheduleID
	if id == "" {
		id = event.OverrideID
	}
	return EventKey(id, event.StartTime)
}
