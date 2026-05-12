package app

import (
	"sync"
	"testing"
	"time"
)

func TestHubPublishDelivers(t *testing.T) {
	h := newHub()
	s := h.subscribe()
	defer h.unsubscribe(s)

	h.publish("state", []byte("hello"))
	select {
	case <-s.wakeup:
	case <-time.After(time.Second):
		t.Fatal("expected wakeup")
	}
	frames, ok := s.drain()
	if !ok {
		t.Fatal("expected open subscription")
	}
	if string(frames["state"]) != "hello" {
		t.Fatalf("got %q", frames["state"])
	}
}

func TestHubCoalescesBurstPerTopic(t *testing.T) {
	h := newHub()
	s := h.subscribe()
	defer h.unsubscribe(s)

	for i := 0; i < 100; i++ {
		h.publish("state", []byte("x"))
	}
	h.publish("state", []byte("final"))
	<-s.wakeup
	frames, _ := s.drain()
	if string(frames["state"]) != "final" {
		t.Fatalf("expected only the latest snapshot, got %q", frames["state"])
	}
}

func TestHubDeliversMultipleTopics(t *testing.T) {
	h := newHub()
	s := h.subscribe()
	defer h.unsubscribe(s)

	h.publish("state", []byte("s"))
	h.publish("schedules", []byte("sc"))
	<-s.wakeup
	frames, _ := s.drain()
	if string(frames["state"]) != "s" || string(frames["schedules"]) != "sc" {
		t.Fatalf("missing frames: %+v", frames)
	}
}

func TestHubUnsubscribeStopsDelivery(t *testing.T) {
	h := newHub()
	s := h.subscribe()
	h.unsubscribe(s)
	// Should observe closed after unsubscribe wake.
	<-s.wakeup
	_, ok := s.drain()
	if ok {
		t.Fatal("expected drain to report closed")
	}
}

func TestHubManySubscribers(t *testing.T) {
	h := newHub()
	const n = 5
	subs := make([]*sub, n)
	for i := range subs {
		subs[i] = h.subscribe()
	}
	defer func() {
		for _, s := range subs {
			h.unsubscribe(s)
		}
	}()
	h.publish("state", []byte("x"))
	var wg sync.WaitGroup
	for _, s := range subs {
		wg.Add(1)
		go func(s *sub) {
			defer wg.Done()
			<-s.wakeup
			frames, _ := s.drain()
			if string(frames["state"]) != "x" {
				t.Errorf("sub missed payload: %q", frames["state"])
			}
		}(s)
	}
	wg.Wait()
}
