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

// sub is one live subscriber: its channel, the user it belongs to, and the set of SHARED calendars
// (owner\x00calID) it may additionally see. Own changes (change owner == user) are always delivered;
// a shared change is delivered only if its (owner, calID) is in access. The access set is a snapshot
// taken at Subscribe time and refreshed when the client reconnects (EventSource auto-reconnects).
type sub struct {
	ch     chan store.Change
	user   string
	access map[string]bool
}

// Hub fans store changes out to subscribers, honouring calendar sharing: a change to a calendar is
// delivered to its owner AND to anyone the calendar is shared with (grantees).
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[*sub]struct{} // calendar owner -> subscribers registered to receive it
}

// New builds a hub and wires it to the store's change stream.
func New(st *store.Store) *Hub {
	h := &Hub{subs: make(map[string]map[*sub]struct{})}
	st.OnChange(h.Publish)
	return h
}

// Publish delivers a change to its owner's subscribers and to grantee subscribers who have access to
// the changed calendar. Non-blocking: a full channel (slow client) drops the frame; that client
// catches up via Last-Event-ID on reconnect.
func (h *Hub) Publish(c store.Change) {
	key := store.CalKey(c.Owner, c.CalendarID)
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs[c.Owner] {
		if c.Owner != s.user && !s.access[key] {
			continue // a grantee registered under this owner, but not for THIS calendar
		}
		select {
		case s.ch <- c:
		default:
		}
	}
}

// Subscribe registers a live subscriber for user, plus the SHARED calendars (owner\x00calID keys,
// e.g. via store.CalKey) it may also receive. It returns the channel and an idempotent cancel that
// detaches and closes it (call exactly once). The subscriber is registered under its own username
// and under every distinct owner in access, so changes from those owners reach it.
func (h *Hub) Subscribe(user string, access map[string]bool) (<-chan store.Change, func()) {
	s := &sub{ch: make(chan store.Change, 64), user: user, access: access}
	owners := map[string]struct{}{user: {}}
	for k := range access {
		if owner, _ := store.SplitCalKey(k); owner != "" {
			owners[owner] = struct{}{}
		}
	}
	h.mu.Lock()
	for owner := range owners {
		if h.subs[owner] == nil {
			h.subs[owner] = make(map[*sub]struct{})
		}
		h.subs[owner][s] = struct{}{}
	}
	h.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			for owner := range owners {
				delete(h.subs[owner], s)
				if len(h.subs[owner]) == 0 {
					delete(h.subs, owner)
				}
			}
			close(s.ch)
			h.mu.Unlock()
		})
	}
	return s.ch, cancel
}
