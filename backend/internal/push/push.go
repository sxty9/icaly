// Package push is icaly's in-process live hub. The store's change-log seq is the single
// spine: every committed mutation is handed to the hub (via store.OnChange) and fanned out
// to that owner's subscribers, each backing one SSE client. The hub holds no history —
// reconnecting clients replay missed changes from the change-log via Last-Event-ID — so a
// slow consumer is simply dropped rather than blocking the writer.
package push

import (
	"sync"

	"icaly/internal/store"
)

// Hub fans store changes out to per-user subscribers.
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[chan store.Change]struct{} // owner -> set of subscriber channels
}

// New builds a hub and wires it to the store's change stream.
func New(st *store.Store) *Hub {
	h := &Hub{subs: make(map[string]map[chan store.Change]struct{})}
	st.OnChange(h.Publish)
	return h
}

// Publish delivers a change to every subscriber of its owner. Non-blocking: a full channel
// (slow client) drops the frame; that client catches up via Last-Event-ID on reconnect.
func (h *Hub) Publish(c store.Change) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[c.Owner] {
		select {
		case ch <- c:
		default:
		}
	}
}

// Subscribe registers a new live subscriber for user and returns its channel plus an
// idempotent cancel that detaches and closes it. Cancel must be called exactly once.
func (h *Hub) Subscribe(user string) (<-chan store.Change, func()) {
	ch := make(chan store.Change, 64)
	h.mu.Lock()
	if h.subs[user] == nil {
		h.subs[user] = make(map[chan store.Change]struct{})
	}
	h.subs[user][ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs[user], ch)
			if len(h.subs[user]) == 0 {
				delete(h.subs, user)
			}
			close(ch)
			h.mu.Unlock()
		})
	}
	return ch, cancel
}
