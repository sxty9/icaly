package store

import (
	"strings"
	"time"

	"icaly/internal/ical"
)

// DueReminder is one VALARM whose fire time (its anchor + trigger offset) is in the scan window.
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

// DueReminders returns every reminder across all calendars whose fire time lies in (after, until].
// It expands recurring series (via ListEvents) and scans occurrences over the WIDER range
// [after-back, until+lookAhead]: `lookAhead` covers a large "before" trigger (an occurrence that
// starts well after `until` can have a fire time inside the window), and `back` covers a rarer
// positive / RELATED=END trigger that fires after the event has already ended (the occurrence
// would otherwise be filtered out for having ended before the window). The precise fire-time gate
// below is independent of the scan range, so widening it only adds candidates — it never
// duplicates a fire. Cancelled occurrences and unparseable/absolute triggers are skipped.
//
// A non-nil error is returned if ANY calendar failed to scan, so the caller can retry the window
// instead of silently losing every reminder due in it (firing is idempotent via the dedupe key).
//
// Scale note: this scans every calendar each tick, which is fine at the current single-host scale;
// if the event count grows large, add an alarms index column and query it by fire time instead.
func (s *Store) DueReminders(after, until time.Time, lookAhead, back time.Duration) ([]DueReminder, error) {
	rows, err := s.db.Query(`SELECT owner, id FROM calendars`)
	if err != nil {
		return nil, err
	}
	var cals [][2]string
	for rows.Next() {
		var owner, id string
		if err := rows.Scan(&owner, &id); err != nil {
			rows.Close()
			return nil, err
		}
		cals = append(cals, [2]string{owner, id})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []DueReminder
	var scanErr error
	for _, c := range cals {
		occs, err := s.ListEvents(c[0], c[1], after.Add(-back), until.Add(lookAhead))
		if err != nil {
			scanErr = err // remember, but keep scanning the other calendars
			continue
		}
		for _, ev := range occs {
			if ev.Status == "CANCELLED" {
				continue
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
						User:       c[0],
						CalendarID: c[1],
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
		}
	}
	return out, scanErr
}

// localMidnight reinterprets a date-anchored UTC-midnight (all-day) instant as midnight in the
// server's local timezone, so an all-day reminder lands on the local calendar day instead of at a
// UTC offset. Best-effort for a single-region instance; a per-event TZID would make it exact.
func localMidnight(utcMidnight time.Time) time.Time {
	y, m, d := utcMidnight.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
}
