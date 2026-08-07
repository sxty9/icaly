// Package reminder is icaly's VALARM firing loop. On a fixed tick it reads events from the store,
// computes here which reminders just came due (the store is a passive pool and does not evaluate
// them), and raises each as a notification through the notify service. Firing is exactly-once per
// (user, calendar, event occurrence, trigger, action): the dedupe key handed to notify collapses a
// re-fire (an overlapping tick, or a retry after a failed scan) to a no-op. Reminders whose fire
// time elapsed while the daemon was down are intentionally not back-fired.
package reminder

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"icaly/internal/event"
	"icaly/internal/ical"
	"icaly/internal/notify"
	"icaly/internal/store"
)

// deepLink is the in-app route the notification opens (the shell routes root-relative paths).
const deepLink = "/app/icaly"

const (
	tickInterval = time.Minute
	// lookAhead bounds how far ahead an occurrence may start and still have a "before" trigger that
	// fires now — i.e. the largest supported reminder lead time (covers -P30D / "a month before").
	lookAhead = 40 * 24 * time.Hour
	// backWindow bounds how far after an occurrence's end a positive / RELATED=END trigger may fire
	// and still be found (such an occurrence has already ended, so it would otherwise be filtered).
	backWindow = 25 * time.Hour
	// maxRetryWindow caps how long a persistently failing scan window is retried before it is given
	// up on, so the scan range can't grow without bound.
	maxRetryWindow = time.Hour
)

// Notifier is the subset of the notify client the scheduler needs (an interface for testability).
type Notifier interface {
	Enabled() bool
	Emit(notify.EmitInput) error
}

// Scheduler periodically fires due reminders.
type Scheduler struct {
	st *store.Store
	nf Notifier
}

// New builds a scheduler.
func New(st *store.Store, nf Notifier) *Scheduler {
	return &Scheduler{st: st, nf: nf}
}

// Run scans on each tick until ctx is cancelled. It no-ops (logs once) when notify is unconfigured.
func (s *Scheduler) Run(ctx context.Context) {
	if s.nf == nil || !s.nf.Enabled() {
		log.Print("icalyd: reminder scheduler disabled (no notify service configured)")
		return
	}
	// Start the window "now": reminders already past at startup are not back-fired.
	last := time.Now()
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			// Advance the window only when the scan fully succeeded. On a transient failure keep
			// `last` so the next tick re-covers the same span (dedupe makes the re-fire a no-op) —
			// but give up once the retried span exceeds maxRetryWindow so the range stays bounded.
			if s.fire(last, now) || now.Sub(last) > maxRetryWindow {
				last = now
			}
		}
	}
}

// fire scans (after, until] and emits each due reminder. It returns false if any calendar failed
// to scan (so the caller can retry the window).
func (s *Scheduler) fire(after, until time.Time) bool {
	due, err := s.dueReminders(after, until)
	if err != nil {
		log.Printf("icalyd: reminder scan (partial): %v", err)
	}
	for _, r := range due {
		title := r.Summary
		if title == "" {
			title = "(ohne Titel)"
		}
		emitErr := s.nf.Emit(notify.EmitInput{
			User:    r.User,
			Service: "icaly",
			Title:   title,
			Body:    reminderBody(r.Base, r.Fire),
			URL:     deepLink,
			Icon:    "📅",
			Level:   "info",
			// One notification per (user, calendar, occurrence, trigger, action), ever (within
			// notify's retention) — so the same UID in two calendars, or a DISPLAY+EMAIL pair, do
			// not collapse into one.
			Dedupe: fmt.Sprintf("%s|%s|%s|%d|%s|%s", r.User, r.CalendarID, r.UID, r.Start.Unix(), r.Trigger, r.Action),
		})
		if emitErr != nil {
			log.Printf("icalyd: reminder emit (%s/%s): %v", r.User, r.UID, emitErr)
		}
	}
	return err == nil
}

// DueReminder is one VALARM whose fire time (its anchor + trigger offset) fell in the scan window.
type DueReminder struct {
	User       string
	CalendarID string
	UID        string
	Summary    string
	Trigger    string
	Action     string    // DISPLAY | EMAIL
	Start      time.Time // the occurrence's start (stable key component, even for END/all-day alarms)
	Base       time.Time // the anchor the trigger is relative to (START, END, or local all-day midnight)
	Fire       time.Time // when the reminder became due (Base + trigger offset)
}

// dueReminders scans every calendar for reminders whose fire time lies in (after, until]. It reads
// events only through the store's passive access points — CalendarRefs to enumerate calendars and
// ListEvents to expand each — and performs the reminder evaluation (trigger math, anchor selection,
// fire-time gating, cancelled-skip) HERE, outside the store: the pool holds events, it does not
// decide which reminders are due.
//
// Occurrences are scanned over the WIDER range [after-backWindow, until+lookAhead]: lookAhead covers
// a large "before" trigger (an occurrence starting well after `until` can fire inside the window),
// and backWindow covers a rarer positive / RELATED=END trigger that fires after the event has ended
// (the occurrence would otherwise be filtered out for having ended before the window). The precise
// fire-time gate below is independent of the scan range, so widening it only adds candidates — it
// never duplicates a fire.
//
// A non-nil error is returned if ANY calendar failed to scan, so the caller can retry the window
// instead of silently losing every reminder due in it (firing is idempotent via the dedupe key).
func (s *Scheduler) dueReminders(after, until time.Time) ([]DueReminder, error) {
	refs, err := s.st.CalendarRefs()
	if err != nil {
		return nil, err
	}
	var out []DueReminder
	var scanErr error
	for _, ref := range refs {
		occs, err := s.st.ListEvents(ref.Owner, ref.ID, after.Add(-backWindow), until.Add(lookAhead))
		if err != nil {
			scanErr = err // remember, but keep scanning the other calendars
			continue
		}
		for _, ev := range occs {
			out = appendDue(out, ref, ev, after, until)
		}
	}
	return out, scanErr
}

// appendDue evaluates one occurrence's alarms and appends any whose fire time falls in (after, until].
// Cancelled occurrences and unparseable/absolute triggers are skipped.
func appendDue(out []DueReminder, ref store.CalendarRef, ev *event.Event, after, until time.Time) []DueReminder {
	if ev.Status == "CANCELLED" {
		return out
	}
	for _, al := range ev.Alarms {
		off, ok := ical.ParseTrigger(al.Trigger)
		if !ok {
			continue // empty / absolute / malformed trigger
		}
		base := ev.Start
		if strings.EqualFold(al.Related, "END") && !ev.End.IsZero() {
			base = ev.End
		}
		if ev.AllDay {
			base = localMidnight(base) // land all-day reminders on the local calendar day
		}
		fire := base.Add(off)
		if fire.After(after) && !fire.After(until) {
			out = append(out, DueReminder{
				User:       ref.Owner,
				CalendarID: ref.ID,
				UID:        ev.UID,
				Summary:    ev.Summary,
				Trigger:    al.Trigger,
				Action:     al.Action,
				Start:      ev.Start,
				Base:       base,
				Fire:       fire,
			})
		}
	}
	return out
}

// localMidnight reinterprets a date-anchored UTC-midnight (all-day) instant as midnight in the
// server's local timezone, so an all-day reminder lands on the local calendar day instead of at a
// UTC offset. Best-effort for a single-region instance; a per-event TZID would make it exact.
func localMidnight(utcMidnight time.Time) time.Time {
	y, m, d := utcMidnight.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
}

// reminderBody phrases the lead time from the reminder's fire moment to the event start.
func reminderBody(base, fire time.Time) string {
	lead := base.Sub(fire)
	switch {
	case lead < time.Minute:
		return "Beginnt jetzt"
	case lead < time.Hour:
		return fmt.Sprintf("Beginnt in %d Minuten", int(lead.Minutes()+0.5))
	case lead < 24*time.Hour:
		return fmt.Sprintf("Beginnt in %d Stunden", int(lead.Hours()+0.5))
	default:
		return fmt.Sprintf("Beginnt in %d Tagen", int(lead.Hours()/24+0.5))
	}
}
