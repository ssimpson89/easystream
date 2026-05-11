package schedule

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// StreamController is the interface the scheduler uses to start/stop FFmpeg.
type StreamController interface {
	// StartWithIngest starts FFmpeg with the given ingest details. broadcastID
	// and streamID are the YouTube resources this stream is bound to (empty
	// for non-YouTube destinations) so the controller can complete the
	// broadcast cleanly on stop.
	StartWithIngest(presetID, ingestURL, streamKey, broadcastID, streamID string) error
	StopStream()
	IsStreaming() bool
}

// YouTubeController is the interface for YouTube API operations.
type YouTubeController interface {
	IsAuthenticated() bool
	CreateBroadcast(title, description string, scheduledStart time.Time, privacy string) (broadcastID string, err error)
	EnsureStream(presetID string) (streamID, ingestURL, streamKey string, err error)
	BindBroadcast(broadcastID, streamID string) error
	TransitionBroadcast(broadcastID, status string) error
}

// Scheduler starts streams at the event time. It does not create YouTube
// broadcasts ahead of time; the scheduled start time is the operator's intent.
type Scheduler struct {
	store  *Store
	stream StreamController
	yt     YouTubeController
	logger *log.Logger

	mu              sync.Mutex
	activeEvent     *Event // currently live event
	activeBroadcast string // YouTube broadcast ID of active event
	extraMinutes    int    // extra minutes added to active event by Extend button
	lastError       string

	cancel context.CancelFunc
	done   chan struct{}
}

// NewScheduler creates a scheduler that checks events every minute.
func NewScheduler(store *Store, stream StreamController, yt YouTubeController, logger *log.Logger) *Scheduler {
	return &Scheduler{
		store:  store,
		stream: stream,
		yt:     yt,
		logger: logger,
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

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// Run immediately on start, then every 30s.
	s.tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick()
		}
	}
}

func (s *Scheduler) tick() {
	now := time.Now().UTC()
	events := s.store.NextEvents(10, now)

	// Check if we need to stop an active event.
	s.mu.Lock()
	active := s.activeEvent
	activeBroadcast := s.activeBroadcast
	extra := s.extraMinutes
	s.mu.Unlock()

	if active != nil {
		endTime := active.StartTime.Add(time.Duration(active.DurationMin+extra) * time.Minute)
		if now.After(endTime) {
			s.stopActiveEvent(activeBroadcast, false)
		}
	}

	if s.yt == nil || !s.yt.IsAuthenticated() {
		return
	}

	for _, event := range events {
		// Go live at start time. If the broadcast was not prepared by an
		// earlier app version, create/bind it just-in-time here.
		if !now.Before(event.StartTime) {
			endTime := event.StartTime.Add(time.Duration(event.DurationMin) * time.Minute)
			if now.Before(endTime) {
				s.mu.Lock()
				isActive := s.activeEvent != nil
				s.mu.Unlock()
				if !isActive && !s.stream.IsStreaming() {
					s.goLive(event)
				}
			}
		}
	}
}

func (s *Scheduler) prepareBroadcast(event Event) (Event, string, string, bool) {
	s.logger.Printf("scheduler: creating YouTube broadcast for %q at %s", event.Name, event.StartTime.Format(time.RFC822))

	broadcastID, err := s.yt.CreateBroadcast(event.Title, event.Description, event.StartTime, event.Privacy)
	if err != nil {
		s.setError(fmt.Sprintf("create broadcast for %q: %v", event.Name, err))
		return event, "", "", false
	}

	streamID, ingestURL, streamKey, err := s.yt.EnsureStream(event.PresetID)
	if err != nil {
		s.setError(fmt.Sprintf("ensure stream for %q: %v", event.Name, err))
		return event, "", "", false
	}

	if err := s.yt.BindBroadcast(broadcastID, streamID); err != nil {
		s.setError(fmt.Sprintf("bind broadcast for %q: %v", event.Name, err))
		return event, "", "", false
	}

	key := eventKey(event)
	if err := s.store.SetBroadcastID(key, broadcastID, streamID); err != nil {
		s.setError(fmt.Sprintf("save broadcast mapping: %v", err))
		return event, "", "", false
	}
	event.BroadcastID = broadcastID
	event.StreamID = streamID

	s.logger.Printf("scheduler: broadcast %s created and bound to stream %s for %q", broadcastID, streamID, event.Name)
	s.setError("")
	return event, ingestURL, streamKey, true
}

func (s *Scheduler) goLive(event Event) {
	s.logger.Printf("scheduler: going live with %q (broadcast %s)", event.Name, event.BroadcastID)
	var ingestURL, streamKey string
	if event.BroadcastID == "" {
		var ok bool
		event, ingestURL, streamKey, ok = s.prepareBroadcast(event)
		if !ok {
			return
		}
	} else {
		// Older scheduled events may have a pre-created broadcast. Bind a
		// fresh stream at the actual start time so FFmpeg and YouTube agree
		// on the same ingest endpoint.
		streamID, url, key, err := s.yt.EnsureStream(event.PresetID)
		if err != nil {
			s.setError(fmt.Sprintf("get stream for %q: %v", event.Name, err))
			return
		}
		if err := s.yt.BindBroadcast(event.BroadcastID, streamID); err != nil {
			s.setError(fmt.Sprintf("bind broadcast for %q: %v", event.Name, err))
			return
		}
		event.StreamID = streamID
		ingestURL = url
		streamKey = key
	}

	// Start FFmpeg, passing through the YouTube IDs so the stream controller
	// can complete the broadcast cleanly on stop.
	if err := s.stream.StartWithIngest(event.PresetID, ingestURL, streamKey, event.BroadcastID, event.StreamID); err != nil {
		s.setError(fmt.Sprintf("start stream for %q: %v", event.Name, err))
		return
	}

	s.mu.Lock()
	s.activeEvent = &event
	s.activeBroadcast = event.BroadcastID
	s.lastError = ""
	s.mu.Unlock()

	// Transition to testing then live in background.
	go s.transitionToLive(event.BroadcastID)
}

func (s *Scheduler) transitionToLive(broadcastID string) {
	// Wait for FFmpeg to start sending frames, then transition.
	// YouTube needs the stream to be active before we can transition.
	for i := 0; i < 30; i++ {
		time.Sleep(5 * time.Second)
		if !s.stream.IsStreaming() {
			return
		}
		if err := s.yt.TransitionBroadcast(broadcastID, "testing"); err != nil {
			s.logger.Printf("scheduler: transition to testing attempt %d: %v", i+1, err)
			continue
		}
		s.logger.Printf("scheduler: broadcast %s transitioned to testing", broadcastID)
		break
	}

	// Wait a moment then go live.
	time.Sleep(10 * time.Second)
	for i := 0; i < 10; i++ {
		if !s.stream.IsStreaming() {
			return
		}
		if err := s.yt.TransitionBroadcast(broadcastID, "live"); err != nil {
			s.logger.Printf("scheduler: transition to live attempt %d: %v", i+1, err)
			time.Sleep(5 * time.Second)
			continue
		}
		s.logger.Printf("scheduler: broadcast %s is LIVE", broadcastID)
		return
	}
	s.setError("could not transition broadcast to live")
}

func (s *Scheduler) stopActiveEvent(broadcastID string, suppress bool) {
	s.logger.Printf("scheduler: stopping active event (broadcast %s)", broadcastID)
	s.stream.StopStream()

	if broadcastID != "" {
		if err := s.yt.TransitionBroadcast(broadcastID, "complete"); err != nil {
			s.logger.Printf("scheduler: transition to complete: %v", err)
		}
	}

	s.mu.Lock()
	event := s.activeEvent
	extra := s.extraMinutes
	s.activeEvent = nil
	s.activeBroadcast = ""
	s.extraMinutes = 0
	s.mu.Unlock()

	if event != nil {
		key := eventKey(*event)
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
	s.mu.Unlock()
	s.stopActiveEvent(broadcast, true)
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
