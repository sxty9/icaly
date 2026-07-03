package push

import (
	"testing"
	"time"

	"icaly/internal/store"
)

func recv(t *testing.T, ch <-chan store.Change) store.Change {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a change")
		return store.Change{}
	}
}

func TestHubFanout(t *testing.T) {
	h := &Hub{subs: make(map[string]map[chan store.Change]struct{})}

	a1, cancelA1 := h.Subscribe("alice")
	a2, cancelA2 := h.Subscribe("alice")
	b1, cancelB1 := h.Subscribe("bob")
	defer cancelB1()

	// A change for alice reaches both of her subscribers, not bob's.
	h.Publish(store.Change{Seq: 1, Owner: "alice", UID: "x", Type: "put"})
	if recv(t, a1).Seq != 1 || recv(t, a2).Seq != 1 {
		t.Fatal("both alice subscribers should receive seq 1")
	}
	select {
	case c := <-b1:
		t.Fatalf("bob must not receive alice's change: %+v", c)
	case <-time.After(50 * time.Millisecond):
	}

	// After cancelling one, only the other still receives.
	cancelA1()
	h.Publish(store.Change{Seq: 2, Owner: "alice", UID: "y", Type: "put"})
	if recv(t, a2).Seq != 2 {
		t.Fatal("remaining alice subscriber should receive seq 2")
	}
	cancelA2()

	// Cancel is idempotent.
	cancelA2()
}

func TestPublishDropsOnFullChannel(t *testing.T) {
	h := &Hub{subs: make(map[string]map[chan store.Change]struct{})}
	_, cancel := h.Subscribe("alice")
	defer cancel()
	// Overflow the 64-deep buffer; Publish must never block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			h.Publish(store.Change{Seq: int64(i), Owner: "alice", Type: "put"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a full subscriber channel")
	}
}
