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
	StartWithIngest(presetID, ingestURL, streamKey string) error
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

// Scheduler creates YouTube broadcasts ahead of time and auto-starts streams.
type Scheduler struct {
	store  *Store
	stream StreamController
	yt     YouTubeController
	logger *log.Logger

	mu              sync.Mutex
	activeEvent     *Event // currently live event
	activeBroadcast string // YouTube broadcast ID of active event
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
	Running         bool   `json:"running"`
	ActiveEventName string `json:"activeEventName,omitempty"`
	ActiveBroadcast string `json:"activeBroadcastId,omitempty"`
	LastError       string `json:"lastError,omitempty"`
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
	}
	return st
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

	ticker := time.NewTicker(30 * time.Second)
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
	s.mu.Unlock()

	if active != nil {
		endTime := active.StartTime.Add(time.Duration(active.DurationMin) * time.Minute)
		if now.After(endTime) {
			s.stopActiveEvent(activeBroadcast)
		}
	}

	if s.yt == nil || !s.yt.IsAuthenticated() {
		return
	}

	for _, event := range events {
		// Create broadcast 30 min before start if not yet created.
		if event.BroadcastID == "" && now.After(event.StartTime.Add(-30*time.Minute)) && now.Before(event.StartTime) {
			s.prepareBroadcast(event)
		}

		// Go live at start time.
		if event.BroadcastID != "" && !now.Before(event.StartTime) {
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

func (s *Scheduler) prepareBroadcast(event Event) {
	s.logger.Printf("scheduler: creating YouTube broadcast for %q at %s", event.Name, event.StartTime.Format(time.RFC822))

	broadcastID, err := s.yt.CreateBroadcast(event.Title, event.Description, event.StartTime, event.Privacy)
	if err != nil {
		s.setError(fmt.Sprintf("create broadcast for %q: %v", event.Name, err))
		return
	}

	streamID, _, _, err := s.yt.EnsureStream(event.PresetID)
	if err != nil {
		s.setError(fmt.Sprintf("ensure stream for %q: %v", event.Name, err))
		return
	}

	if err := s.yt.BindBroadcast(broadcastID, streamID); err != nil {
		s.setError(fmt.Sprintf("bind broadcast for %q: %v", event.Name, err))
		return
	}

	key := eventKey(event)
	if err := s.store.SetBroadcastID(key, broadcastID, streamID); err != nil {
		s.setError(fmt.Sprintf("save broadcast mapping: %v", err))
		return
	}

	s.logger.Printf("scheduler: broadcast %s created and bound to stream %s for %q", broadcastID, streamID, event.Name)
	s.setError("")
}

func (s *Scheduler) goLive(event Event) {
	s.logger.Printf("scheduler: going live with %q (broadcast %s)", event.Name, event.BroadcastID)

	// Get stream ingest details.
	_, ingestURL, streamKey, err := s.yt.EnsureStream(event.PresetID)
	if err != nil {
		s.setError(fmt.Sprintf("get stream for %q: %v", event.Name, err))
		return
	}

	// Start FFmpeg.
	if err := s.stream.StartWithIngest(event.PresetID, ingestURL, streamKey); err != nil {
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

func (s *Scheduler) stopActiveEvent(broadcastID string) {
	s.logger.Printf("scheduler: stopping active event (broadcast %s)", broadcastID)
	s.stream.StopStream()

	if broadcastID != "" {
		if err := s.yt.TransitionBroadcast(broadcastID, "complete"); err != nil {
			s.logger.Printf("scheduler: transition to complete: %v", err)
		}
	}

	s.mu.Lock()
	event := s.activeEvent
	s.activeEvent = nil
	s.activeBroadcast = ""
	s.mu.Unlock()

	if event != nil {
		key := eventKey(*event)
		_ = s.store.ClearBroadcast(key)
	}
}

// StopActive manually stops any active scheduled event.
func (s *Scheduler) StopActive() {
	s.mu.Lock()
	broadcast := s.activeBroadcast
	s.mu.Unlock()
	s.stopActiveEvent(broadcast)
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
