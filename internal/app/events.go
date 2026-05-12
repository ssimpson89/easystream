package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// SSE topics. Keep this small: each topic is a full snapshot replacement
// on the client side, so a "diff" model isn't needed.
const (
	topicState     = "state"
	topicSchedules = "schedules"
	topicOverrides = "overrides"
)

// publishState marshals the full status snapshot and pushes it to all
// subscribers. Called from every in-process write site that changes
// observable state (stream lifecycle, health, config, scheduler).
//
// Safe to call from any goroutine. Non-blocking: hub coalesces.
func (s *Server) publishState() {
	if s.hub == nil {
		return
	}
	data, err := json.Marshal(s.statusSnapshot())
	if err != nil {
		s.logger.Printf("events: marshal state: %v", err)
		return
	}
	s.hub.publish(topicState, data)
}

// publishSchedules pushes the recurring-schedule list. Called from the
// schedule CRUD handlers.
func (s *Server) publishSchedules() {
	if s.hub == nil || s.schedStore == nil {
		return
	}
	data, err := json.Marshal(s.schedStore.Schedules())
	if err != nil {
		s.logger.Printf("events: marshal schedules: %v", err)
		return
	}
	s.hub.publish(topicSchedules, data)
}

// publishOverrides pushes the one-time override list.
func (s *Server) publishOverrides() {
	if s.hub == nil || s.schedStore == nil {
		return
	}
	data, err := json.Marshal(s.schedStore.Overrides())
	if err != nil {
		s.logger.Printf("events: marshal overrides: %v", err)
		return
	}
	s.hub.publish(topicOverrides, data)
}

// handleEventStream implements GET /api/events as Server-Sent Events.
// The connection writes an initial snapshot for every topic, then loops
// on hub wakeups, flushing the latest payloads. A 15 s heartbeat comment
// keeps idle-proxy timeouts from closing the connection silently.
func (s *Server) handleEventStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	// nginx/cloudflare may buffer text/event-stream by default.
	w.Header().Set("X-Accel-Buffering", "no")
	// Tell EventSource to retry promptly on disconnect.
	if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil {
		return
	}
	flusher.Flush()

	sub := s.hub.subscribe()
	defer s.hub.unsubscribe(sub)

	// Initial snapshot so the new client doesn't wait for the first event.
	s.writeInitialSnapshot(w, flusher)

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-sub.wakeup:
			frames, ok := sub.drain()
			if !ok {
				return
			}
			for topic, payload := range frames {
				if !writeEvent(w, topic, payload) {
					return
				}
			}
			flusher.Flush()
		}
	}
}

// writeInitialSnapshot pushes one event per topic so a fresh subscriber
// paints immediately instead of waiting for the next publish.
func (s *Server) writeInitialSnapshot(w http.ResponseWriter, flusher http.Flusher) {
	if state, err := json.Marshal(s.statusSnapshot()); err == nil {
		writeEvent(w, topicState, state)
	}
	if s.schedStore != nil {
		if d, err := json.Marshal(s.schedStore.Schedules()); err == nil {
			writeEvent(w, topicSchedules, d)
		}
		if d, err := json.Marshal(s.schedStore.Overrides()); err == nil {
			writeEvent(w, topicOverrides, d)
		}
	}
	flusher.Flush()
}

// writeEvent writes a single SSE frame. Returns false if the connection
// has been dropped (writer error), letting callers exit their loops.
//
// SSE format: "event: <name>\ndata: <payload>\n\n". data lines can't
// contain raw newlines, but our JSON payloads are produced by
// json.Marshal with no indentation, so this is safe.
func writeEvent(w http.ResponseWriter, topic string, data []byte) bool {
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", topic, data); err != nil {
		return false
	}
	return true
}
