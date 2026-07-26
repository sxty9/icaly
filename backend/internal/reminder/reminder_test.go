package reminder

import (
	"testing"
	"time"

	"icaly/internal/event"
	"icaly/internal/store"
)

func TestDueReminders(t *testing.T) {
	base := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	// due stores ev in a fresh store, then asks the scheduler which reminders fall in
	// (fire-1min, fire]. The scheduler performs the evaluation (the store no longer does), reading
	// the event back only through the store's passive access points.
	due := func(t *testing.T, ev *event.Event, fire time.Time) int {
		t.Helper()
		st, err := store.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if _, err := st.PutEvent("tester", "personal", ev); err != nil {
			t.Fatal(err)
		}
		sc := &Scheduler{st: st}
		rs, err := sc.dueReminders(fire.Add(-time.Minute), fire)
		if err != nil {
			t.Fatalf("dueReminders: %v", err)
		}
		return len(rs)
	}

	alarm := func(trigger, related string) []event.Alarm {
		return []event.Alarm{{Action: "DISPLAY", Trigger: trigger, Related: related}}
	}

	t.Run("relative before start", func(t *testing.T) {
		ev := &event.Event{UID: "a", Summary: "x", Start: base.Add(15 * time.Minute), End: base.Add(75 * time.Minute), Alarms: alarm("-PT15M", "")}
		if n := due(t, ev, base); n != 1 { // fire = start-15m = base
			t.Fatalf("want 1 due, got %d", n)
		}
	})

	t.Run("large lead beyond old 8d ceiling", func(t *testing.T) {
		ev := &event.Event{UID: "b", Summary: "x", Start: base.Add(30 * 24 * time.Hour), End: base.Add(30*24*time.Hour + time.Hour), Alarms: alarm("-P30D", "")}
		if n := due(t, ev, base); n != 1 { // fire = start-30d = base; must be found (lookAhead=40d)
			t.Fatalf("want 1 due (P30D lead), got %d", n)
		}
	})

	t.Run("positive trigger after event end", func(t *testing.T) {
		ev := &event.Event{UID: "c", Summary: "x", Start: base, End: base.Add(30 * time.Minute), Alarms: alarm("PT45M", "")}
		if n := due(t, ev, base.Add(45*time.Minute)); n != 1 { // fire = start+45m, 15m after end
			t.Fatalf("want 1 due (positive trigger), got %d", n)
		}
	})

	t.Run("RELATED=END fires relative to end", func(t *testing.T) {
		ev := &event.Event{UID: "d", Summary: "x", Start: base, End: base.Add(time.Hour), Alarms: alarm("-PT15M", "END")}
		// fire = END-15m = base+45m. At START-15m (base-15m) it must NOT be due.
		if n := due(t, ev, base.Add(45*time.Minute)); n != 1 {
			t.Fatalf("want 1 due at END-15m, got %d", n)
		}
		if n := due(t, ev, base.Add(-15*time.Minute)); n != 0 {
			t.Fatalf("must NOT fire at START-15m, got %d", n)
		}
	})

	t.Run("cancelled occurrence skipped", func(t *testing.T) {
		ev := &event.Event{UID: "e", Summary: "x", Start: base.Add(15 * time.Minute), End: base.Add(75 * time.Minute), Status: "CANCELLED", Alarms: alarm("-PT15M", "")}
		if n := due(t, ev, base); n != 0 {
			t.Fatalf("cancelled must be skipped, got %d", n)
		}
	})
}
