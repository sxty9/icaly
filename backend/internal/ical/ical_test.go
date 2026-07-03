package ical

import (
	"strings"
	"testing"
	"time"

	"icaly/internal/event"
)

func TestRecurrenceScopedEdits(t *testing.T) {
	start := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC) // Mon
	master := &event.Event{UID: "r1", Summary: "Standup", Start: start, End: start.Add(30 * time.Minute), RRule: "FREQ=WEEKLY;COUNT=5"}
	b, err := Encode(master)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	from, to := start.Add(-24*time.Hour), start.Add(60*24*time.Hour)
	w := func(n int) time.Time { return start.Add(time.Duration(n) * 7 * 24 * time.Hour) }

	if occ, _ := ExpandSeries(b, from, to); len(occ) != 5 {
		t.Fatalf("baseline: want 5 occurrences, got %d", len(occ))
	}

	// "this only" edit: override w1 — new title, moved +1h.
	rid := w(1)
	edited := master.Clone()
	edited.Summary, edited.RecurrenceID = "Standup (moved)", &rid
	edited.Start, edited.End = w(1).Add(time.Hour), w(1).Add(time.Hour+30*time.Minute)
	b, err = Override(b, edited)
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	occ, _ := ExpandSeries(b, from, to)
	if len(occ) != 5 {
		t.Fatalf("after override: want 5 (replace, not add), got %d", len(occ))
	}
	var moved *event.Event
	for _, e := range occ {
		if e.Summary == "Standup (moved)" {
			moved = e
		}
	}
	if moved == nil || !moved.Start.Equal(w(1).Add(time.Hour)) {
		t.Fatalf("override instance not reflected: %+v", moved)
	}

	// "this only" delete: EXDATE w2.
	b, err = Exclude(b, w(2))
	if err != nil {
		t.Fatalf("exclude: %v", err)
	}
	occ, _ = ExpandSeries(b, from, to)
	if len(occ) != 4 {
		t.Fatalf("after exclude: want 4, got %d", len(occ))
	}
	for _, e := range occ {
		if e.Start.Equal(w(2)) {
			t.Fatalf("excluded occurrence w2 still present")
		}
	}

	// "this and following" truncate before w3: keeps w0 + w1(override); w2 excluded; w3,w4 gone.
	b, err = Truncate(b, w(3).Add(-time.Second))
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	occ, _ = ExpandSeries(b, from, to)
	if len(occ) != 2 {
		t.Fatalf("after truncate: want 2, got %d", len(occ))
	}
}

func TestExpandKeepsSeriesRule(t *testing.T) {
	start := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)
	m := &event.Event{UID: "k", Summary: "S", Start: start, End: start.Add(time.Hour), RRule: "FREQ=WEEKLY;COUNT=3"}
	b, _ := Encode(m)
	occ, _ := ExpandSeries(b, start.Add(-time.Hour), start.Add(60*24*time.Hour))
	if len(occ) != 3 {
		t.Fatalf("want 3, got %d", len(occ))
	}
	for _, e := range occ {
		// The instance must keep the series rule (so the UI shows the scope chooser) AND its
		// recurrenceId — clearing the rule was the critical data-loss regression.
		if e.RRule != "FREQ=WEEKLY;COUNT=3" || e.RecurrenceID == nil {
			t.Fatalf("instance lost series identity: rrule=%q recurrenceId=%v", e.RRule, e.RecurrenceID)
		}
	}
}

func TestAllEditPreservesOverridesAndExdate(t *testing.T) {
	start := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)
	m := &event.Event{UID: "a", Summary: "S", Start: start, End: start.Add(time.Hour), RRule: "FREQ=WEEKLY;COUNT=5"}
	b, _ := Encode(m)
	w := func(n int) time.Time { return start.Add(time.Duration(n) * 7 * 24 * time.Hour) }
	rid := w(1)
	ed := m.Clone()
	ed.Summary, ed.RecurrenceID = "OV", &rid
	ed.Start, ed.End = w(1).Add(time.Hour), w(1).Add(2*time.Hour)
	b, _ = Override(b, ed)
	b, _ = Exclude(b, w(2))

	// "all" edit: rename the whole series, preserving the override + the EXDATE.
	cur, _ := Decode(b) // master carries the EXDATE we just added
	newMaster := m.Clone()
	newMaster.Summary, newMaster.ExDates = "ALL", cur.ExDates
	b2, err := ReplaceMaster(b, newMaster)
	if err != nil {
		t.Fatalf("replace master: %v", err)
	}
	occ, _ := ExpandSeries(b2, start.Add(-time.Hour), start.Add(60*24*time.Hour))
	if len(occ) != 4 { // w0,w1(override),w3,w4 ; w2 still excluded
		t.Fatalf("after all-edit want 4 (override+exdate kept), got %d", len(occ))
	}
	var ov, all bool
	for _, e := range occ {
		if e.Summary == "OV" {
			ov = true
		}
		if e.Summary == "ALL" {
			all = true
		}
		if e.Start.Equal(w(2)) {
			t.Fatal("all-edit resurrected the EXDATE-deleted occurrence")
		}
	}
	if !ov {
		t.Fatal("all-edit dropped the per-occurrence override")
	}
	if !all {
		t.Fatal("all-edit field change not applied to the series")
	}
}

func TestAllDayExcludeUsesDateValue(t *testing.T) {
	start := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	m := &event.Event{UID: "ad", Summary: "AllDay", Start: start, AllDay: true, RRule: "FREQ=DAILY;COUNT=4"}
	b, _ := Encode(m)
	b2, err := Exclude(b, start.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("exclude: %v", err)
	}
	// EXDATE must match the all-day DTSTART value type (DATE, not DATE-TIME).
	if !strings.Contains(string(b2), "VALUE=DATE:20260707") {
		t.Fatalf("all-day EXDATE not date-valued:\n%s", b2)
	}
	occ, _ := ExpandSeries(b2, start.AddDate(0, 0, -1), start.AddDate(0, 0, 10))
	if len(occ) != 3 {
		t.Fatalf("all-day exclude: want 3, got %d", len(occ))
	}
}

func TestTailRuleRebasesCount(t *testing.T) {
	start := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)
	m := &event.Event{UID: "t", Start: start, End: start.Add(time.Hour), RRule: "FREQ=WEEKLY;COUNT=5"}
	b, _ := Encode(m)
	tail, err := TailRule(b, start.Add(2*7*24*time.Hour)) // split before the 3rd occurrence
	if err != nil {
		t.Fatalf("tail rule: %v", err)
	}
	if !strings.Contains(tail, "COUNT=3") { // 5 total − 2 before = 3 remaining
		t.Fatalf("tail rule should rebase COUNT to 3, got %q", tail)
	}
}

func TestGeoRoundTrip(t *testing.T) {
	start := time.Date(2026, 7, 9, 14, 0, 0, 0, time.UTC)
	in := &event.Event{
		UID: "geo-1", Summary: "Friseurtermin", Start: start, End: start.Add(time.Hour),
		Location: "Friseursalon Ginza Matsunaga", Geo: &event.GeoPoint{Lat: 53.5754, Lon: 9.9586},
	}
	b, err := Encode(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// RFC 5545: GEO:<lat>;<lon> (semicolon, latitude first), no VALUE= param.
	if !strings.Contains(string(b), "GEO:53.5754;9.9586") {
		t.Fatalf("GEO line wrong:\n%s", b)
	}
	if strings.Contains(string(b), "GEO;VALUE") {
		t.Fatalf("GEO must not carry a VALUE= param:\n%s", b)
	}
	out, err := Decode(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Geo == nil || out.Geo.Lat != 53.5754 || out.Geo.Lon != 9.9586 {
		t.Fatalf("geo did not round-trip: %+v", out.Geo)
	}
}

func TestRoundTripTimed(t *testing.T) {
	start := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	in := &event.Event{
		UID:         "abc123",
		Summary:     "Q4 Planning",
		Description: "Agenda inside",
		Location:    "Room 1",
		Start:       start,
		End:         start.Add(90 * time.Minute),
		Status:      "CONFIRMED",
		Class:       "PRIVATE",
		Priority:    5,
		Color:       "turquoise",
		Categories:  []string{"work", "planning"},
		Organizer:   &event.Participant{Email: "alice@example.com", Name: "Alice"},
		Attendees: []event.Participant{
			{Email: "bob@example.com", Name: "Bob", Role: "REQ-PARTICIPANT", PartStat: "NEEDS-ACTION", RSVP: true},
		},
		Alarms: []event.Alarm{{Action: "DISPLAY", Trigger: "-PT15M"}},
	}
	b, err := Encode(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	s := string(b)
	// Timed events must be emitted in UTC (Z form), with no TZID param (plan B2 avoidance).
	if !strings.Contains(s, "DTSTART:20260628T120000Z") {
		t.Errorf("expected UTC DTSTART, got:\n%s", s)
	}
	if strings.Contains(s, "TZID=") {
		t.Errorf("unexpected TZID param:\n%s", s)
	}

	out, err := Decode(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Summary != in.Summary || out.Location != in.Location || out.Status != "CONFIRMED" || out.Class != "PRIVATE" {
		t.Errorf("text fields mismatch: %+v", out)
	}
	if !out.Start.Equal(in.Start) || !out.End.Equal(in.End) {
		t.Errorf("times mismatch: start=%v end=%v", out.Start, out.End)
	}
	if out.Priority != 5 || out.Color != "turquoise" {
		t.Errorf("priority/color mismatch: %d %q", out.Priority, out.Color)
	}
	if len(out.Categories) != 2 {
		t.Errorf("categories: %v", out.Categories)
	}
	if out.Organizer == nil || out.Organizer.Email != "alice@example.com" {
		t.Errorf("organizer: %+v", out.Organizer)
	}
	if len(out.Attendees) != 1 || out.Attendees[0].Email != "bob@example.com" || !out.Attendees[0].RSVP {
		t.Errorf("attendees: %+v", out.Attendees)
	}
	if len(out.Alarms) != 1 || out.Alarms[0].Trigger != "-PT15M" {
		t.Errorf("alarms: %+v", out.Alarms)
	}
}

func TestAllDayDTENDExclusive(t *testing.T) {
	// m5 regression: all-day DTEND is exclusive (next day).
	day := time.Date(2026, 6, 28, 0, 0, 0, 0, time.UTC)
	in := &event.Event{UID: "d1", Summary: "Holiday", Start: day, AllDay: true}
	b, err := Encode(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, "DTSTART;VALUE=DATE:20260628") {
		t.Errorf("expected date-valued DTSTART:\n%s", s)
	}
	if !strings.Contains(s, "DTEND;VALUE=DATE:20260629") {
		t.Errorf("expected exclusive DTEND (next day):\n%s", s)
	}
	out, err := Decode(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.AllDay {
		t.Errorf("expected all-day")
	}
	if !out.End.Equal(day.Add(24 * time.Hour)) {
		t.Errorf("DTEND not exclusive: %v", out.End)
	}
}

func TestFeedAndDecodeAll(t *testing.T) {
	mk := func(uid, sum string) []byte {
		b, err := Encode(&event.Event{UID: uid, Summary: sum,
			Start: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC), End: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)})
		if err != nil {
			t.Fatalf("encode %s: %v", uid, err)
		}
		return b
	}
	// An override instance (carries RECURRENCE-ID) must be carried in the feed but skipped by DecodeAll.
	rid := time.Date(2026, 7, 2, 9, 0, 0, 0, time.UTC)
	override, err := Encode(&event.Event{UID: "u1", Summary: "Override",
		Start: rid, End: rid.Add(time.Hour), RecurrenceID: &rid})
	if err != nil {
		t.Fatalf("encode override: %v", err)
	}

	feed, err := Feed("My Cal", [][]byte{mk("u1", "First"), mk("u2", "Second"), override})
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	s := string(feed)
	for _, want := range []string{"METHOD:PUBLISH", "X-WR-CALNAME:My Cal", "REFRESH-INTERVAL", "UID:u1", "UID:u2"} {
		if !strings.Contains(s, want) {
			t.Errorf("feed missing %q:\n%s", want, s)
		}
	}

	evs, err := DecodeAll(feed)
	if err != nil {
		t.Fatalf("decodeAll: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("expected 2 masters (override skipped), got %d", len(evs))
	}
}

func TestOccurrences(t *testing.T) {
	start := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	in := &event.Event{UID: "r1", Summary: "Daily", Start: start, End: start.Add(time.Hour), RRule: "FREQ=DAILY;COUNT=3"}
	b, err := Encode(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	occ, err := Occurrences(b, time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("occurrences: %v", err)
	}
	if len(occ) != 3 {
		t.Fatalf("expected 3 occurrences, got %d: %v", len(occ), occ)
	}
	if !occ[0].Equal(start) || !occ[2].Equal(start.AddDate(0, 0, 2)) {
		t.Errorf("occurrence times: %v", occ)
	}
}
