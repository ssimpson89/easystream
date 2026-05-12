package app

import (
	"sync"
)

// hub is a small in-process pub/sub for server-sent state changes.
//
// Each subscriber keeps the latest payload per topic (coalescing) so a
// burst of publishes — a transition firing the health poller + supervisor
// callback within milliseconds — collapses into a single wire frame.
// A slow consumer can never block a publisher: the writer goroutine
// drains its own per-sub map; publishers only touch the index under the
// hub mutex briefly.
//
// Scale: 1–3 subscribers ever (browser tabs on a single host).
type hub struct {
	mu   sync.Mutex
	subs map[*sub]struct{}
}

// sub is a single subscriber's view of the hub.
type sub struct {
	mu      sync.Mutex
	latest  map[string][]byte // topic → last payload
	wakeup  chan struct{}     // signal: there is new data
	closed  bool
}

func newHub() *hub {
	return &hub{subs: make(map[*sub]struct{})}
}

// subscribe creates a new subscriber. The caller owns the returned *sub
// and must call hub.unsubscribe when finished.
func (h *hub) subscribe() *sub {
	s := &sub{
		latest: make(map[string][]byte),
		// Buffered: a writer that already has the signal pending
		// doesn't block here; the next wake will see all latest values.
		wakeup: make(chan struct{}, 1),
	}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *hub) unsubscribe(s *sub) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	// Wake any waiter so it can observe closed=true and exit.
	select {
	case s.wakeup <- struct{}{}:
	default:
	}
}

// publish stores payload as the latest value for topic on every
// subscriber. Old payloads for the same topic are dropped — clients only
// ever care about the current snapshot. Non-blocking.
func (h *hub) publish(topic string, payload []byte) {
	h.mu.Lock()
	subs := make([]*sub, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()

	for _, s := range subs {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			continue
		}
		s.latest[topic] = payload
		s.mu.Unlock()
		select {
		case s.wakeup <- struct{}{}:
		default:
			// Wakeup already pending; the reader will drain everything.
		}
	}
}

// drain returns and clears all pending payloads for s. Returns ok=false
// when the subscription has been cancelled — the caller should exit its
// loop in that case.
func (s *sub) drain() (frames map[string][]byte, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false
	}
	frames = s.latest
	s.latest = make(map[string][]byte)
	return frames, true
}
