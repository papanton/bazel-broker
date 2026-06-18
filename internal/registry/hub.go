package registry

import (
	"sync"

	"github.com/antoniospapantoniou/bazel-broker/internal/api"
)

// subBufferSize is the per-subscriber buffer. A subscriber whose buffer fills
// (slow consumer) is dropped — its channel is closed — rather than blocking
// registry mutations. Clients reconnect and re-read the snapshot to resync.
const subBufferSize = 64

// Hub is the non-blocking WS fan-out. Mutations broadcast an api.Event to every
// subscriber; a full buffer drops that subscriber instead of blocking.
type Hub struct {
	mu   sync.Mutex
	subs map[*Subscriber]struct{}
}

// Subscriber is one connected WS client's event feed.
type Subscriber struct {
	ch     chan api.Event
	closed bool
}

// Events returns the receive channel; it is closed when the subscriber is
// dropped (slow consumer) or unsubscribed.
func (s *Subscriber) Events() <-chan api.Event { return s.ch }

// NewHub constructs an empty Hub.
func NewHub() *Hub {
	return &Hub{subs: make(map[*Subscriber]struct{})}
}

// Subscribe registers a new subscriber and returns it plus an unsubscribe func.
func (h *Hub) Subscribe() (*Subscriber, func()) {
	sub := &Subscriber{ch: make(chan api.Event, subBufferSize)}
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			h.mu.Lock()
			h.drop(sub)
			h.mu.Unlock()
		})
	}
	return sub, unsub
}

// Broadcast sends ev to every subscriber, dropping any whose buffer is full.
// Non-blocking by design: registry mutations never wait on a slow client.
func (h *Hub) Broadcast(ev api.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		select {
		case sub.ch <- ev:
		default:
			// Slow consumer: drop it. The client reconnects and re-snapshots.
			h.drop(sub)
		}
	}
}

// SubscriberCount reports how many subscribers are currently attached (tests).
func (h *Hub) SubscriberCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// drop removes and closes a subscriber. Caller must hold h.mu.
func (h *Hub) drop(sub *Subscriber) {
	if _, ok := h.subs[sub]; !ok {
		return
	}
	delete(h.subs, sub)
	if !sub.closed {
		sub.closed = true
		close(sub.ch)
	}
}
