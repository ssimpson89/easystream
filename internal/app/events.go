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
// Safe to call from any goroutine. Non-blocking: hub coalesces, and
// publish becomes a no-op after hub.Close.
func (s *Server) publishState() {
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
	if s.schedStore == nil {
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
	if s.schedStore == nil {
		return
	}
	data, err := json.Marshal(s.schedStore.Overrides())
	if err != nil {
		s.logger.Printf("events: marshal overrides: %v", err)
		return
	}
	s.hub.publish(topicOverrides, data)
}

// handleEventStream implements GET /api/stream/state as Server-Sent
// Events. The connection writes an initial snapshot for every topic,
// then loops on hub wakeups, flushing the latest payloads. A 15 s
// heartbeat comment keeps idle-proxy timeouts from closing the
// connection silently.
//
// Per-write deadlines via http.ResponseController bound the goroutine
// leak when a client TCP half-closes (laptop suspend, NAT eviction):
// a stalled write fails within writeDeadline instead of hanging until
// OS keepalive.
const writeDeadline = 10 * time.Second

func (s *Server) handleEventStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	rc := http.NewResponseController(w)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	// nginx/cloudflare may buffer text/event-stream by default.
	w.Header().Set("X-Accel-Buffering", "no")

	// Defense in depth against a runaway client opening many SSE
	// connections. subscribe() returns nil at maxSubscribers or after
	// hub.Close, in which case we surface 503 so the client backs off.
	sub := s.hub.subscribe()
	if sub == nil {
		http.Error(w, "too many SSE subscribers", http.StatusServiceUnavailable)
		return
	}
	defer s.hub.unsubscribe(sub)

	// Tell EventSource to retry promptly on disconnect.
	if !writeWithDeadline(rc, w, flusher, "retry: 3000\n\n") {
		return
	}

	// Initial snapshot so the new client doesn't wait for the first event.
	s.writeInitialSnapshot(rc, w, flusher)

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if !writeWithDeadline(rc, w, flusher, ": keep-alive\n\n") {
				return
			}
		case <-sub.wakeup:
			frames, ok := sub.drain()
			if !ok {
				return
			}
			for topic, payload := range frames {
				if !writeEventWithDeadline(rc, w, topic, payload) {
					return
				}
			}
			if err := flushWithDeadline(rc, flusher); err != nil {
				return
			}
		}
	}
}

// writeWithDeadline sets a write deadline, writes a literal string, and
// flushes. Returns false on any error so the caller can exit its loop.
func writeWithDeadline(rc *http.ResponseController, w http.ResponseWriter, flusher http.Flusher, s string) bool {
	_ = rc.SetWriteDeadline(time.Now().Add(writeDeadline))
	if _, err := fmt.Fprint(w, s); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

func writeEventWithDeadline(rc *http.ResponseController, w http.ResponseWriter, topic string, data []byte) bool {
	_ = rc.SetWriteDeadline(time.Now().Add(writeDeadline))
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", topic, data); err != nil {
		return false
	}
	return true
}

func flushWithDeadline(rc *http.ResponseController, flusher http.Flusher) error {
	_ = rc.SetWriteDeadline(time.Now().Add(writeDeadline))
	flusher.Flush()
	return nil
}

// writeInitialSnapshot pushes one event per topic so a fresh subscriber
// paints immediately instead of waiting for the next publish.
func (s *Server) writeInitialSnapshot(rc *http.ResponseController, w http.ResponseWriter, flusher http.Flusher) {
	if state, err := json.Marshal(s.statusSnapshot()); err == nil {
		writeEventWithDeadline(rc, w, topicState, state)
	}
	if s.schedStore != nil {
		if d, err := json.Marshal(s.schedStore.Schedules()); err == nil {
			writeEventWithDeadline(rc, w, topicSchedules, d)
		}
		if d, err := json.Marshal(s.schedStore.Overrides()); err == nil {
			writeEventWithDeadline(rc, w, topicOverrides, d)
		}
	}
	_ = flushWithDeadline(rc, flusher)
}
