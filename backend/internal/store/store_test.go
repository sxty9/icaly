package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaly/internal/event"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestAutoProvisionDefaultCalendar(t *testing.T) {
	st := openTest(t)
	cals, err := st.Calendars("alice")
	if err != nil {
		t.Fatalf("calendars: %v", err)
	}
	if len(cals) != 1 || cals[0].ID != "personal" {
		t.Fatalf("expected one default 'personal' calendar, got %+v", cals)
	}
}

func TestEventCRUDAndChangeLog(t *testing.T) {
	st := openTest(t)
	start := time.Date(2026, 6, 28, 9, 0, 0, 0, time.UTC)
	ev := &event.Event{Summary: "Standup", Start: start, End: start.Add(30 * time.Minute)}

	etag, err := st.PutEvent("alice", "personal", ev)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if etag == "" || ev.UID == "" {
		t.Fatalf("expected etag and assigned uid, got etag=%q uid=%q", etag, ev.UID)
	}

	// ctag advanced past the initial "0".
	if ct, _ := st.CTag("alice", "personal"); ct == "0" || ct == "" {
		t.Fatalf("ctag did not advance: %q", ct)
	}

	got, etag2, err := st.GetEvent("alice", "personal", ev.UID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Summary != "Standup" || etag2 != etag {
		t.Fatalf("get mismatch: summary=%q etag=%q", got.Summary, etag2)
	}

	// Update bumps sequence and changes the etag.
	got.Summary = "Standup (moved)"
	etag3, err := st.PutEvent("alice", "personal", got)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if etag3 == etag {
		t.Errorf("etag should change on update")
	}
	if got.Sequence != 1 {
		t.Errorf("expected sequence 1 after one update, got %d", got.Sequence)
	}

	// Range listing includes it.
	list, err := st.ListEvents("alice", "personal", start.Add(-time.Hour), start.Add(time.Hour))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].UID != ev.UID {
		t.Fatalf("expected the event in range, got %+v", list)
	}

	// Delete then gone.
	if err := st.DeleteEvent("alice", "personal", ev.UID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, _, err := st.GetEvent("alice", "personal", ev.UID); err == nil {
		t.Fatalf("expected ErrNotFound after delete")
	}
}

func TestRecurringExpansionInList(t *testing.T) {
	st := openTest(t)
	start := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	ev := &event.Event{Summary: "Daily", Start: start, End: start.Add(time.Hour), RRule: "FREQ=DAILY;COUNT=3"}
	if _, err := st.PutEvent("alice", "personal", ev); err != nil {
		t.Fatalf("put: %v", err)
	}
	list, err := st.ListEvents("alice", "personal", start.Add(-24*time.Hour), start.Add(10*24*time.Hour))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("expected 3 expanded instances, got %d", len(list))
	}
	for _, inst := range list {
		if inst.RecurrenceID == nil {
			t.Errorf("expanded instance missing RecurrenceID")
		}
	}
}

func TestChangeLogFeedAndRaw(t *testing.T) {
	st := openTest(t)
	start := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)

	// Capture live emissions while we mutate.
	var emitted []Change
	st.OnChange(func(c Change) { emitted = append(emitted, c) })

	a := &event.Event{Summary: "A", Start: start, End: start.Add(time.Hour)}
	if _, err := st.PutEvent("alice", "personal", a); err != nil {
		t.Fatalf("put a: %v", err)
	}
	b := &event.Event{Summary: "B", Start: start.Add(2 * time.Hour), End: start.Add(3 * time.Hour)}
	if _, err := st.PutEvent("alice", "personal", b); err != nil {
		t.Fatalf("put b: %v", err)
	}

	if len(emitted) != 2 || emitted[0].Type != "put" || emitted[1].Seq <= emitted[0].Seq {
		t.Fatalf("expected 2 monotonic put emissions, got %+v", emitted)
	}

	max, err := st.MaxSeq("alice")
	if err != nil || max != emitted[1].Seq {
		t.Fatalf("MaxSeq=%d err=%v, want %d", max, err, emitted[1].Seq)
	}

	// ChangesSince(first) replays only the second mutation.
	missed, err := st.ChangesSince("alice", emitted[0].Seq)
	if err != nil {
		t.Fatalf("changesSince: %v", err)
	}
	if len(missed) != 1 || missed[0].UID != b.UID {
		t.Fatalf("expected only B replayed, got %+v", missed)
	}

	// Feed token round-trips to the owning calendar; raw .ics files are present.
	cals, err := st.Calendars("alice")
	if err != nil || len(cals) == 0 || cals[0].FeedToken == "" {
		t.Fatalf("calendars/feed token: %+v err=%v", cals, err)
	}
	cal, err := st.CalendarByFeedToken(cals[0].FeedToken)
	if err != nil || cal.Owner != "alice" || cal.ID != "personal" {
		t.Fatalf("CalendarByFeedToken: %+v err=%v", cal, err)
	}
	if _, err := st.CalendarByFeedToken("nope"); err != ErrNotFound {
		t.Fatalf("bad token should be ErrNotFound, got %v", err)
	}
	raws, err := st.RawEvents("alice", "personal")
	if err != nil || len(raws) != 2 {
		t.Fatalf("RawEvents: got %d err=%v", len(raws), err)
	}

	// A delete emits a tombstone change and drops a raw file.
	if err := st.DeleteEvent("alice", "personal", a.UID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(emitted) != 3 || emitted[2].Type != "delete" {
		t.Fatalf("expected delete emission, got %+v", emitted)
	}
	if raws, _ := st.RawEvents("alice", "personal"); len(raws) != 1 {
		t.Fatalf("expected 1 raw after delete, got %d", len(raws))
	}
}

const rawICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//dav-client//EN\r\n" +
	"BEGIN:VEVENT\r\nUID:dav-1\r\nDTSTART:20260901T090000Z\r\nDTEND:20260901T100000Z\r\n" +
	"SUMMARY:From a DAV client\r\nX-CLIENT-CUSTOM:keep-this-verbatim\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func TestPutRawVerbatimAndPreconditions(t *testing.T) {
	st := openTest(t)
	// First write creates the resource and reports created=true.
	etag, created, err := st.PutRaw("alice", "personal", "dav-1", []byte(rawICS), "", "")
	if err != nil || !created || etag == "" {
		t.Fatalf("put raw: etag=%q created=%v err=%v", etag, created, err)
	}
	// RawEvent returns the bytes byte-identically (plan B1: no re-encode, no X-prop loss).
	got, etag2, err := st.RawEvent("alice", "personal", "dav-1")
	if err != nil {
		t.Fatalf("raw event: %v", err)
	}
	if string(got) != rawICS {
		t.Fatalf("stored bytes are not verbatim:\nwant %q\ngot  %q", rawICS, got)
	}
	if etag2 != etag {
		t.Fatalf("etag mismatch on read: %q vs %q", etag2, etag)
	}
	if etagOf(got) != etag {
		t.Fatalf("served-bytes hash != ETag (B1 broken): %q vs %q", etagOf(got), etag)
	}

	// If-None-Match "*" must fail now the resource exists.
	if _, _, err := st.PutRaw("alice", "personal", "dav-1", []byte(rawICS), "", "*"); err != ErrPreconditionFailed {
		t.Fatalf("If-None-Match * on existing should be precondition-failed, got %v", err)
	}
	// If-Match with a stale etag must fail; with the right etag must succeed (and created=false).
	if _, _, err := st.PutRaw("alice", "personal", "dav-1", []byte(rawICS), `"deadbeef"`, ""); err != ErrPreconditionFailed {
		t.Fatalf("If-Match stale should fail, got %v", err)
	}
	_, created2, err := st.PutRaw("alice", "personal", "dav-1", []byte(rawICS), etag, "")
	if err != nil || created2 {
		t.Fatalf("If-Match correct should update (created=false), got created=%v err=%v", created2, err)
	}

	// DeleteRaw honours If-Match too.
	cur, _, _ := st.RawEvent("alice", "personal", "dav-1")
	if err := st.DeleteRaw("alice", "personal", "dav-1", `"wrong"`); err != ErrPreconditionFailed {
		t.Fatalf("delete with stale If-Match should fail, got %v", err)
	}
	if err := st.DeleteRaw("alice", "personal", "dav-1", etagOf(cur)); err != nil {
		t.Fatalf("delete with right If-Match: %v", err)
	}
	if _, _, err := st.RawEvent("alice", "personal", "dav-1"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestSyncCollectionDeltaAndTombstones(t *testing.T) {
	st := openTest(t)
	start := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	mk := func(uid string) {
		ev := &event.Event{UID: uid, Summary: uid, Start: start, End: start.Add(time.Hour)}
		if _, err := st.PutEvent("alice", "personal", ev); err != nil {
			t.Fatalf("put %s: %v", uid, err)
		}
	}
	mk("a")
	mk("b")

	// Initial sync (token 0) enumerates all current members as puts.
	changes, tok0, err := st.SyncCollection("alice", "personal", 0)
	if err != nil || len(changes) != 2 || tok0 == 0 {
		t.Fatalf("initial sync: changes=%d tok=%d err=%v", len(changes), tok0, err)
	}

	// A new event and a deletion produce exactly one put + one tombstone since tok0.
	mk("c")
	if err := st.DeleteEvent("alice", "personal", "a"); err != nil {
		t.Fatalf("delete a: %v", err)
	}
	delta, tok1, err := st.SyncCollection("alice", "personal", tok0)
	if err != nil {
		t.Fatalf("delta sync: %v", err)
	}
	if tok1 <= tok0 {
		t.Fatalf("token did not advance: %d -> %d", tok0, tok1)
	}
	var puts, dels int
	for _, c := range delta {
		if c.Deleted {
			dels++
			if c.UID != "a" {
				t.Errorf("unexpected tombstone for %q", c.UID)
			}
		} else {
			puts++
			if c.UID != "c" {
				t.Errorf("unexpected put for %q", c.UID)
			}
		}
	}
	if puts != 1 || dels != 1 {
		t.Fatalf("expected 1 put + 1 tombstone, got %d/%d: %+v", puts, dels, delta)
	}

	// After compaction trims the log, the stale tok0 is rejected (plan M2 → caller answers 409).
	if err := st.Compact(time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if _, _, err := st.SyncCollection("alice", "personal", tok0); err != ErrSyncTokenTooOld {
		t.Fatalf("stale token after compaction should be ErrSyncTokenTooOld, got %v", err)
	}
	// Initial sync (token 0) still works after compaction.
	if _, _, err := st.SyncCollection("alice", "personal", 0); err != nil {
		t.Fatalf("initial sync after compaction: %v", err)
	}
}

func TestMkcalendarAndProppatch(t *testing.T) {
	st := openTest(t)
	cal, err := st.CreateCalendarID("alice", "work", "Work", "#ff0000", "")
	if err != nil || cal.ID != "work" {
		t.Fatalf("create with id: %+v err=%v", cal, err)
	}
	// Duplicate id is a precondition failure (MKCALENDAR on existing → 405 at the handler).
	if _, err := st.CreateCalendarID("alice", "work", "Work", "", ""); err != ErrPreconditionFailed {
		t.Fatalf("duplicate id should be precondition-failed, got %v", err)
	}
	// Unsafe id is rejected.
	if _, err := st.CreateCalendarID("alice", "../escape", "x", "", ""); err == nil {
		t.Fatalf("unsafe id should be rejected")
	}
	// PROPPATCH-style update changes only the provided fields.
	newName := "Office"
	if err := st.UpdateCalendar("alice", "work", &newName, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	cals, _ := st.Calendars("alice")
	var found *event.Calendar
	for i := range cals {
		if cals[i].ID == "work" {
			found = &cals[i]
		}
	}
	if found == nil || found.Name != "Office" || found.Color != "#ff0000" {
		t.Fatalf("update result: %+v", found)
	}
}

func TestReconcilerHealsIndex(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := st.Calendars("alice"); err != nil { // provision the personal calendar
		t.Fatalf("calendars: %v", err)
	}
	if _, _, err := st.PutRaw("alice", "personal", "a", []byte(rawICS), "", ""); err != nil {
		t.Fatalf("put a: %v", err)
	}
	// Simulate a crash after the .ics write but before the index commit: drop a file straight
	// into the caldir, with no index row, and an out-of-band deletion of a's file.
	calDir := filepath.Join(dir, "calendars", "alice", "personal")
	icsB := strings.Replace(rawICS, "UID:dav-1", "UID:b", 1)
	if err := os.WriteFile(filepath.Join(calDir, "b.ics"), []byte(icsB), 0o644); err != nil {
		t.Fatalf("write b.ics: %v", err)
	}
	if err := os.Remove(filepath.Join(calDir, "a.ics")); err != nil {
		t.Fatalf("rm a.ics: %v", err)
	}
	_ = st.Close()

	// Re-open: the reconciler indexes the new file and tombstones the orphaned row.
	st2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	metas, err := st2.EventMetas("alice", "personal")
	if err != nil {
		t.Fatalf("metas: %v", err)
	}
	got := map[string]bool{}
	for _, m := range metas {
		got[m.UID] = true
	}
	if !got["b"] || got["a"] || len(metas) != 1 {
		t.Fatalf("reconcile: expected only b indexed, got %+v", metas)
	}
	// b is served byte-identically from the file that was reindexed.
	raw, _, err := st2.RawEvent("alice", "personal", "b")
	if err != nil || string(raw) != icsB {
		t.Fatalf("reconciled b not verbatim: err=%v", err)
	}
}

func TestZeroStartStaysVisible(t *testing.T) {
	st := openTest(t)
	// A VEVENT with no parseable DTSTART must still be stored verbatim AND remain discoverable by
	// time-range queries (indexed with a maximally-wide span), not silently filed at year 1.
	noStart := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:nostart\r\nSUMMARY:No start\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	if _, _, err := st.PutRaw("alice", "personal", "nostart", []byte(noStart), "", ""); err != nil {
		t.Fatalf("put no-start: %v", err)
	}
	metas, err := st.EventMetas("alice", "personal")
	if err != nil || len(metas) != 1 {
		t.Fatalf("metas: %d err=%v", len(metas), err)
	}
	winFrom := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	winTo := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	m := metas[0]
	if !m.Start.Before(winFrom) || !m.End.After(winTo) {
		t.Fatalf("zero-start event not indexed wide: start=%v end=%v", m.Start, m.End)
	}
}

func TestUIDWithAtSignRoundTripsAndSurvivesReconcile(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := st.Calendars("alice"); err != nil {
		t.Fatalf("calendars: %v", err)
	}
	// RFC 5545 recommends localpart@domain UIDs, and clients mirror the UID into the href.
	uid := "19960401T080045Z-4000F192713@example.com"
	ics := strings.Replace(rawICS, "UID:dav-1", "UID:"+uid, 1)
	etag, created, err := st.PutRaw("alice", "personal", uid, []byte(ics), "", "")
	if err != nil || !created {
		t.Fatalf("put @-uid should succeed: err=%v created=%v", err, created)
	}
	got, e2, err := st.RawEvent("alice", "personal", uid)
	if err != nil || string(got) != ics || e2 != etag {
		t.Fatalf("verbatim round-trip by raw uid failed: err=%v match=%v", err, string(got) == ics)
	}
	// On disk the '@' is percent-encoded, so the file identity and the index key cannot clash.
	if _, err := os.Stat(filepath.Join(dir, "calendars", "alice", "personal", encodeName(uid)+".ics")); err != nil {
		t.Fatalf("expected percent-encoded filename on disk: %v", err)
	}
	_ = st.Close()

	// After a restart the reconciler must keep the exact UID — no re-keying, no phantom tombstone.
	st2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	if got2, _, err := st2.RawEvent("alice", "personal", uid); err != nil || string(got2) != ics {
		t.Fatalf("after reconcile the @-uid event was lost/rekeyed: err=%v", err)
	}
	metas, _ := st2.EventMetas("alice", "personal")
	if len(metas) != 1 || metas[0].UID != uid {
		t.Fatalf("reconcile changed the index key: %+v", metas)
	}
}

func TestStoreRecurrenceScopedEdits(t *testing.T) {
	st := openTest(t)
	start := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)
	ev := &event.Event{Summary: "Standup", Start: start, End: start.Add(30 * time.Minute), RRule: "FREQ=WEEKLY;COUNT=5"}
	if _, err := st.PutEvent("alice", "personal", ev); err != nil {
		t.Fatalf("put recurring: %v", err)
	}
	from, to := start.Add(-24*time.Hour), start.Add(60*24*time.Hour)
	w := func(n int) time.Time { return start.Add(time.Duration(n) * 7 * 24 * time.Hour) }
	count := func() int {
		l, err := st.ListEvents("alice", "personal", from, to)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		return len(l)
	}
	if count() != 5 {
		t.Fatalf("baseline want 5, got %d", count())
	}

	// this-only edit → override w1.
	rid := w(1)
	edited := ev.Clone()
	edited.Summary, edited.RecurrenceID = "Moved", &rid
	edited.Start, edited.End = w(1).Add(time.Hour), w(1).Add(time.Hour+30*time.Minute)
	if err := st.OverrideOccurrence("alice", "personal", ev.UID, edited); err != nil {
		t.Fatalf("override: %v", err)
	}
	list, _ := st.ListEvents("alice", "personal", from, to)
	if len(list) != 5 {
		t.Fatalf("after override want 5, got %d", len(list))
	}
	seen := false
	for _, e := range list {
		if e.Summary == "Moved" && e.Start.Equal(w(1).Add(time.Hour)) {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("override not reflected in ListEvents")
	}

	// this-only delete → EXDATE w2.
	if err := st.ExcludeOccurrence("alice", "personal", ev.UID, w(2)); err != nil {
		t.Fatalf("exclude: %v", err)
	}
	if count() != 4 {
		t.Fatalf("after exclude want 4, got %d", count())
	}

	// this-and-following delete → truncate before w3 (keeps w0 + w1 override).
	if err := st.TruncateSeries("alice", "personal", ev.UID, w(3)); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if count() != 2 {
		t.Fatalf("after truncate want 2, got %d", count())
	}
}

func TestDeletedEventStaysDeletedAfterReconcile(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	start := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	if _, err := st.PutEvent("alice", "personal", &event.Event{UID: "gone", Summary: "Gone", Start: start, End: start.Add(time.Hour)}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := st.DeleteEvent("alice", "personal", "gone"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// The source .ics must be gone, or a restart reconcile would re-index the leftover file and
	// resurrect the deleted event (the delete must remove the source of truth, not just the index).
	if _, statErr := os.Stat(filepath.Join(dir, "calendars", "alice", "personal", "gone.ics")); !os.IsNotExist(statErr) {
		t.Fatalf("source .ics must be removed by delete; stat err=%v", statErr)
	}
	_ = st.Close()

	st2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	if _, _, err := st2.GetEvent("alice", "personal", "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted event resurrected after reconcile: err=%v", err)
	}
	if metas, _ := st2.EventMetas("alice", "personal"); len(metas) != 0 {
		t.Fatalf("expected no events after reconcile, got %+v", metas)
	}
}

func TestDeleteAbortsWhenSourceFileRemovalFails(t *testing.T) {
	st := openTest(t)
	start := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	if _, err := st.PutEvent("alice", "personal", &event.Event{UID: "keepme", Summary: "Keep", Start: start, End: start.Add(time.Hour)}); err != nil {
		t.Fatalf("put: %v", err)
	}
	var emitted []Change
	st.OnChange(func(c Change) { emitted = append(emitted, c) })

	// Force the .ics removal to fail: the delete must abort with the event fully intact — never
	// commit a tombstone (which a reconcile could not undo) while the source file survives.
	st.removeFile = func(string) error { return errors.New("simulated removal failure") }
	if err := st.DeleteEvent("alice", "personal", "keepme"); err == nil {
		t.Fatal("expected DeleteEvent to fail when the source file cannot be removed")
	}
	if _, _, err := st.GetEvent("alice", "personal", "keepme"); err != nil {
		t.Fatalf("event must remain after an aborted delete, got %v", err)
	}
	if len(emitted) != 0 {
		t.Fatalf("an aborted delete must emit no change, got %+v", emitted)
	}
	// Index and file still agree, so a reconcile would be a no-op (no resurrection, no phantom put).
	if raws, err := st.RawEvents("alice", "personal"); err != nil || len(raws) != 1 {
		t.Fatalf("source file must survive an aborted delete: got %d err=%v", len(raws), err)
	}
}

func TestCrossUserIsolation(t *testing.T) {
	st := openTest(t)
	start := time.Date(2026, 6, 28, 9, 0, 0, 0, time.UTC)
	ev := &event.Event{Summary: "Alice only", Start: start, End: start.Add(time.Hour)}
	if _, err := st.PutEvent("alice", "personal", ev); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Bob's auto-provisioned calendar must not see Alice's event.
	list, err := st.ListEvents("bob", "personal", start.Add(-time.Hour), start.Add(time.Hour))
	if err != nil {
		t.Fatalf("list bob: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("cross-user leak: bob sees %d events", len(list))
	}
}
