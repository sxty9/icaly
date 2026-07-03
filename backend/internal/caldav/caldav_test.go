package caldav

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"icaly/internal/store"
)

const davBase = "/dav/"

func newTestHandler(t *testing.T) (*Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(st, davBase, func(u string) string { return u + "@test.local" }), st
}

// serve runs one DAV request as principal "alice" (admin=false, canEdit=true) and returns the
// recorder. headers is optional (nil ok).
func serve(h *Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.Serve(w, r, "alice", false, true)
	return w
}

const evt1 = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//client//EN\r\n" +
	"BEGIN:VEVENT\r\nUID:evt1\r\nDTSTART:20260901T090000Z\r\nDTEND:20260901T100000Z\r\n" +
	"SUMMARY:Sprint review\r\nX-CLIENT-CUSTOM:keep-this-verbatim\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func TestOptionsAdvertisesCalDAV(t *testing.T) {
	h, _ := newTestHandler(t)
	w := serve(h, "OPTIONS", davBase, "", nil)
	if w.Code != 200 {
		t.Fatalf("OPTIONS status %d", w.Code)
	}
	if dav := w.Header().Get("DAV"); !strings.Contains(dav, "calendar-access") {
		t.Fatalf("DAV header missing calendar-access: %q", dav)
	}
}

func TestPropfindDiscovery(t *testing.T) {
	h, _ := newTestHandler(t)

	// Principal → calendar-home-set points at the user's calendars collection.
	body := `<D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">` +
		`<D:prop><C:calendar-home-set/><D:current-user-principal/></D:prop></D:propfind>`
	w := serve(h, "PROPFIND", davBase+"principals/alice/", body, map[string]string{"Depth": "0"})
	if w.Code != 207 {
		t.Fatalf("principal propfind status %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "<D:href>/dav/calendars/alice/</D:href>") {
		t.Fatalf("calendar-home-set missing:\n%s", w.Body.String())
	}

	// Home Depth:1 lists the auto-provisioned personal calendar, typed as a calendar collection.
	w = serve(h, "PROPFIND", davBase+"calendars/alice/", "", map[string]string{"Depth": "1"})
	out := w.Body.String()
	if w.Code != 207 || !strings.Contains(out, "/dav/calendars/alice/personal/") {
		t.Fatalf("home depth-1 missing personal collection (status %d):\n%s", w.Code, out)
	}
	if !strings.Contains(out, "<C:calendar/>") || !strings.Contains(out, "<CS:getctag>") {
		t.Fatalf("collection props missing calendar/getctag:\n%s", out)
	}
	if !strings.Contains(out, "<D:sync-token>urn:icaly:sync:") {
		t.Fatalf("collection missing sync-token:\n%s", out)
	}
}

func TestPutGetByteFidelity(t *testing.T) {
	h, _ := newTestHandler(t)
	path := davBase + "calendars/alice/personal/evt1.ics"

	w := serve(h, "PUT", path, evt1, nil)
	if w.Code != 201 {
		t.Fatalf("PUT new resource status %d (want 201)", w.Code)
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("PUT did not return an ETag")
	}

	// GET returns the exact bytes that were PUT, same ETag, X-prop intact (plan B1).
	g := serve(h, "GET", path, "", nil)
	if g.Code != 200 {
		t.Fatalf("GET status %d", g.Code)
	}
	if g.Body.String() != evt1 {
		t.Fatalf("GET body is not byte-identical to PUT:\n%q", g.Body.String())
	}
	if g.Header().Get("ETag") != etag {
		t.Fatalf("GET ETag %q != PUT ETag %q", g.Header().Get("ETag"), etag)
	}
	if !strings.Contains(g.Body.String(), "X-CLIENT-CUSTOM:keep-this-verbatim") {
		t.Fatal("custom X-prop was lost on round-trip")
	}

	// If-None-Match:* on an existing resource → 412 (no clobber).
	if w := serve(h, "PUT", path, evt1, map[string]string{"If-None-Match": "*"}); w.Code != 412 {
		t.Fatalf("If-None-Match * on existing: want 412, got %d", w.Code)
	}
	// If-Match with a stale validator → 412.
	if w := serve(h, "PUT", path, evt1, map[string]string{"If-Match": `"stale"`}); w.Code != 412 {
		t.Fatalf("If-Match stale: want 412, got %d", w.Code)
	}
	// If-Match with the right validator → 204 update.
	if w := serve(h, "PUT", path, evt1, map[string]string{"If-Match": etag}); w.Code != 204 {
		t.Fatalf("If-Match correct: want 204, got %d", w.Code)
	}
}

func TestReportMultigetAndQuery(t *testing.T) {
	h, _ := newTestHandler(t)
	put := func(uid, ics string) {
		if w := serve(h, "PUT", davBase+"calendars/alice/personal/"+uid+".ics", ics, nil); w.Code != 201 {
			t.Fatalf("seed %s: status %d", uid, w.Code)
		}
	}
	put("evt1", evt1)
	// A second event well outside the query window.
	put("old", strings.Replace(strings.Replace(evt1, "UID:evt1", "UID:old", 1), "20260901", "20250101", 2))

	// calendar-multiget returns the verbatim calendar-data for the requested href.
	mg := `<C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">` +
		`<D:prop><D:getetag/><C:calendar-data/></D:prop>` +
		`<D:href>/dav/calendars/alice/personal/evt1.ics</D:href></C:calendar-multiget>`
	w := serve(h, "REPORT", davBase+"calendars/alice/personal/", mg, nil)
	if w.Code != 207 || !strings.Contains(w.Body.String(), "X-CLIENT-CUSTOM:keep-this-verbatim") {
		t.Fatalf("multiget missing verbatim calendar-data (status %d):\n%s", w.Code, w.Body.String())
	}

	// calendar-query with a one-day time-range returns evt1 but not the out-of-range "old".
	q := `<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">` +
		`<D:prop><D:getetag/></D:prop>` +
		`<C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT">` +
		`<C:time-range start="20260901T000000Z" end="20260902T000000Z"/>` +
		`</C:comp-filter></C:comp-filter></C:filter></C:calendar-query>`
	w = serve(h, "REPORT", davBase+"calendars/alice/personal/", q, nil)
	out := w.Body.String()
	if w.Code != 207 || !strings.Contains(out, "/personal/evt1.ics") {
		t.Fatalf("calendar-query missing evt1 (status %d):\n%s", w.Code, out)
	}
	if strings.Contains(out, "/personal/old.ics") {
		t.Fatalf("calendar-query should have filtered out the out-of-range event:\n%s", out)
	}
}

func TestCalendarDataPreservesCRLF(t *testing.T) {
	h, _ := newTestHandler(t)
	if w := serve(h, "PUT", davBase+"calendars/alice/personal/evt1.ics", evt1, nil); w.Code != 201 {
		t.Fatalf("seed: %d", w.Code)
	}
	mg := `<C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">` +
		`<D:prop><C:calendar-data/></D:prop>` +
		`<D:href>/dav/calendars/alice/personal/evt1.ics</D:href></C:calendar-multiget>`
	w := serve(h, "REPORT", davBase+"calendars/alice/personal/", mg, nil)
	// The CR of each CRLF must be escaped as &#13; so the client's XML parser does not collapse
	// CRLF→LF (which would break byte-fidelity vs the GET / ETag preimage).
	if !strings.Contains(w.Body.String(), "&#13;") {
		t.Fatalf("calendar-data did not escape CR as &#13;:\n%s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "VERSION:2.0\r\n") {
		t.Fatalf("calendar-data carried a literal CRLF (will be normalised by the client)")
	}
}

func TestCalendarQueryIncludesUntimedEvent(t *testing.T) {
	h, _ := newTestHandler(t)
	noStart := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:nostart\r\nSUMMARY:No start\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	if w := serve(h, "PUT", davBase+"calendars/alice/personal/nostart.ics", noStart, nil); w.Code != 201 {
		t.Fatalf("seed no-start: %d", w.Code)
	}
	q := `<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">` +
		`<D:prop><D:getetag/></D:prop>` +
		`<C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT">` +
		`<C:time-range start="20260101T000000Z" end="20270101T000000Z"/>` +
		`</C:comp-filter></C:comp-filter></C:filter></C:calendar-query>`
	w := serve(h, "REPORT", davBase+"calendars/alice/personal/", q, nil)
	if !strings.Contains(w.Body.String(), "/personal/nostart.ics") {
		t.Fatalf("untimed event should still surface in a time-range query:\n%s", w.Body.String())
	}
}

func TestSyncCollectionReport(t *testing.T) {
	h, st := newTestHandler(t)
	put := func(uid string) {
		ics := strings.Replace(evt1, "UID:evt1", "UID:"+uid, 1)
		if w := serve(h, "PUT", davBase+"calendars/alice/personal/"+uid+".ics", ics, nil); w.Code != 201 {
			t.Fatalf("seed %s: status %d", uid, w.Code)
		}
	}
	put("a")
	put("b")

	initial := `<D:sync-collection xmlns:D="DAV:"><D:sync-token/><D:sync-level>1</D:sync-level>` +
		`<D:prop><D:getetag/></D:prop></D:sync-collection>`
	w := serve(h, "REPORT", davBase+"calendars/alice/personal/", initial, nil)
	if w.Code != 207 {
		t.Fatalf("initial sync status %d", w.Code)
	}
	tok := extractSyncToken(t, w.Body.String())

	// Change set: add c, delete a.
	put("c")
	if w := serve(h, "DELETE", davBase+"calendars/alice/personal/a.ics", "", nil); w.Code != 204 {
		t.Fatalf("delete a: status %d", w.Code)
	}

	delta := `<D:sync-collection xmlns:D="DAV:"><D:sync-token>` + tok + `</D:sync-token>` +
		`<D:sync-level>1</D:sync-level><D:prop><D:getetag/></D:prop></D:sync-collection>`
	w = serve(h, "REPORT", davBase+"calendars/alice/personal/", delta, nil)
	out := w.Body.String()
	if w.Code != 207 {
		t.Fatalf("delta sync status %d", w.Code)
	}
	if !strings.Contains(out, "/personal/c.ics") {
		t.Fatalf("delta missing new event c:\n%s", out)
	}
	// The deleted resource appears as a 404 tombstone.
	if !strings.Contains(out, "/personal/a.ics") || !strings.Contains(out, "404 Not Found") {
		t.Fatalf("delta missing tombstone for a:\n%s", out)
	}

	// After compaction past the old token, the server forces a full resync (HTTP 409).
	if err := st.Compact(time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("compact: %v", err)
	}
	w = serve(h, "REPORT", davBase+"calendars/alice/personal/", delta, nil)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "valid-sync-token") {
		t.Fatalf("stale token should be 409 valid-sync-token, got %d:\n%s", w.Code, w.Body.String())
	}
}

func TestPutAtSignUIDAndInvalidData(t *testing.T) {
	h, _ := newTestHandler(t)
	// A resource whose name carries an '@' (the RFC-5545-recommended UID shape) must store, not 409.
	path := davBase + "calendars/alice/personal/abc@example.com.ics"
	ics := strings.Replace(evt1, "UID:evt1", "UID:abc@example.com", 1)
	if w := serve(h, "PUT", path, ics, nil); w.Code != 201 {
		t.Fatalf("PUT @-uid: want 201, got %d", w.Code)
	}
	if g := serve(h, "GET", path, "", nil); g.Code != 200 || g.Body.String() != ics {
		t.Fatalf("GET @-uid not verbatim: code=%d", g.Code)
	}
	// A body that is not iCalendar gets the valid-calendar-data precondition (403), not 400/500.
	w := serve(h, "PUT", davBase+"calendars/alice/personal/junk.ics", "this is not a calendar", nil)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "valid-calendar-data") {
		t.Fatalf("invalid PUT: want 403 valid-calendar-data, got %d:\n%s", w.Code, w.Body.String())
	}
}

func TestProppatchIsAtomic(t *testing.T) {
	h, st := newTestHandler(t)
	if _, err := st.Calendars("alice"); err != nil { // provision personal
		t.Fatalf("calendars: %v", err)
	}
	// A request mixing a writable prop (displayname) with an unsupported one must apply NOTHING:
	// the unsupported prop is 403, the writable one is 424 Failed Dependency, and the name is kept.
	pp := `<D:propertyupdate xmlns:D="DAV:" xmlns:X="urn:example:x">` +
		`<D:set><D:prop><D:displayname>Renamed</D:displayname><X:bogus>v</X:bogus></D:prop></D:set>` +
		`</D:propertyupdate>`
	w := serve(h, "PROPPATCH", davBase+"calendars/alice/personal/", pp, nil)
	out := w.Body.String()
	if w.Code != 207 || !strings.Contains(out, "403 Forbidden") || !strings.Contains(out, "424 Failed Dependency") {
		t.Fatalf("proppatch not atomic (want 403+424):\n%s", out)
	}
	cals, _ := st.Calendars("alice")
	for _, c := range cals {
		if c.ID == "personal" && c.Name != "Personal" {
			t.Fatalf("atomic proppatch applied displayname despite a sibling failure: name=%q", c.Name)
		}
	}
}

func TestMkcalendarAndScoping(t *testing.T) {
	h, _ := newTestHandler(t)
	// MKCALENDAR creates the collection at the chosen URL.
	mk := `<C:mkcalendar xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:IC="http://apple.com/ns/ical/">` +
		`<D:set><D:prop><D:displayname>Work</D:displayname><IC:calendar-color>#ff0000</IC:calendar-color></D:prop></D:set>` +
		`</C:mkcalendar>`
	if w := serve(h, "MKCALENDAR", davBase+"calendars/alice/work/", mk, nil); w.Code != 201 {
		t.Fatalf("MKCALENDAR status %d", w.Code)
	}
	// It now shows up under the home collection.
	w := serve(h, "PROPFIND", davBase+"calendars/alice/", "", map[string]string{"Depth": "1"})
	if !strings.Contains(w.Body.String(), "/dav/calendars/alice/work/") {
		t.Fatalf("new calendar not listed:\n%s", w.Body.String())
	}

	// A principal may not act inside another user's tree (Schicht-2 path scoping).
	r := httptest.NewRequest("PROPFIND", davBase+"calendars/bob/", strings.NewReader(""))
	rr := httptest.NewRecorder()
	h.Serve(rr, r, "alice", false, true) // alice, not admin, asking for bob/
	if rr.Code != 403 {
		t.Fatalf("cross-user access should be 403, got %d", rr.Code)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────────

func extractSyncToken(t *testing.T, body string) string {
	t.Helper()
	const open, close = "<D:sync-token>", "</D:sync-token>"
	i := strings.Index(body, open)
	j := strings.Index(body, close)
	if i < 0 || j < 0 || j < i {
		t.Fatalf("no sync-token in response:\n%s", body)
	}
	return body[i+len(open) : j]
}
