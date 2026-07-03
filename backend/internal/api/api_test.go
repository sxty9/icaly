package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/user"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"icaly/internal/apppass"
	"icaly/internal/auth"
	"icaly/internal/event"
	"icaly/internal/geocode"
	"icaly/internal/imip"
	"icaly/internal/instance"
	"icaly/internal/push"
	"icaly/internal/scheduling"
	"icaly/internal/store"
)

// fakeGeo is a canned geocode.Provider so the geocode endpoint can be exercised without network.
type fakeGeo struct{}

func (fakeGeo) Name() string { return "fake" }
func (fakeGeo) Suggest(_ context.Context, input, _ string) ([]geocode.Suggestion, error) {
	return []geocode.Suggestion{{ID: "p1", Label: input + " — Hamburg", Primary: input, Secondary: "Hamburg", Resolved: false}}, nil
}
func (fakeGeo) Resolve(_ context.Context, id, _ string) (geocode.Place, error) {
	return geocode.Place{Name: "Picked " + id, Address: "Eppendorfer Weg 211, Hamburg", Lat: 53.5754, Lon: 9.9586}, nil
}

// harness spins up the full HTTP surface backed by a temp store, authenticated as the current
// OS user. The user's own primary group is used as the admin group, so rights always pass —
// this exercises routing/handlers, not the (separately tested) rights resolution.
type harness struct {
	st    *store.Store
	srv   *httptest.Server
	user  string
	token string
}

const csrfVal = "csrf-test-value"

func newHarness(t *testing.T) *harness {
	t.Helper()
	cur, err := user.Current()
	if err != nil || cur.Username == "" {
		t.Skip("no current OS user")
	}
	gids, err := cur.GroupIds()
	if err != nil || len(gids) == 0 {
		t.Skip("cannot resolve current user's groups")
	}
	g, err := user.LookupGroupId(gids[0])
	if err != nil || g.Name == "" {
		t.Skip("cannot resolve a group name")
	}
	secret := []byte("test-secret-please-ignore")
	v := auth.NewVerifier(secret, g.Name) // user is in this group => admin => all rights pass

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	hub := push.New(st)
	inst := instance.New()
	sched := scheduling.New(st, inst, imip.New("", "")) // mailer disabled in tests
	ap := apppass.New(t.TempDir())
	geo := geocode.NewWith(fakeGeo{})
	srv := httptest.NewServer(New(v, st, inst, hub, sched, ap, geo, "test-inbound-secret").Handler())
	t.Cleanup(srv.Close)

	claims := jwt.MapClaims{"sub": cur.Username, "type": "access", "exp": time.Now().Add(time.Hour).Unix()}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return &harness{st: st, srv: srv, user: cur.Username, token: tok}
}

func (h *harness) req(t *testing.T, method, path string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+path, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: "h_access", Value: h.token})
	if method != http.MethodGet && method != http.MethodHead {
		req.AddCookie(&http.Cookie{Name: "h_csrf", Value: csrfVal})
		req.Header.Set("X-CSRF-Token", csrfVal)
	}
	return req
}

func (h *harness) do(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", req.Method, req.URL.Path, err)
	}
	return resp
}

func TestFeedHTTP(t *testing.T) {
	h := newHarness(t)
	start := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	if _, err := h.st.PutEvent(h.user, "personal", &event.Event{Summary: "Feed me", Start: start, End: start.Add(time.Hour)}); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	cals, err := h.st.Calendars(h.user)
	if err != nil || len(cals) == 0 || cals[0].FeedToken == "" {
		t.Fatalf("calendars: %+v err=%v", cals, err)
	}
	feedPath := base + "feeds/" + cals[0].FeedToken + ".ics"

	resp := h.do(t, h.req(t, http.MethodGet, feedPath, nil)) // no auth: token is the credential
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("feed status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/calendar") {
		t.Fatalf("feed content-type %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "METHOD:PUBLISH") || !strings.Contains(string(body), "Feed me") {
		t.Fatalf("feed body:\n%s", body)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("feed missing ETag")
	}

	// If-None-Match against the ctag-derived ETag yields a cheap 304.
	req := h.req(t, http.MethodGet, feedPath, nil)
	req.Header.Set("If-None-Match", etag)
	resp2 := h.do(t, req)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Fatalf("expected 304, got %d", resp2.StatusCode)
	}

	// A bad token is a 404, never a leak.
	resp3 := h.do(t, h.req(t, http.MethodGet, base+"feeds/deadbeef.ics", nil))
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("bad token: expected 404, got %d", resp3.StatusCode)
	}
}

func TestImportExport(t *testing.T) {
	h := newHarness(t)
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:imp-1\r\nDTSTART:20260801T090000Z\r\nDTEND:20260801T100000Z\r\nSUMMARY:Imported One\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:imp-2\r\nDTSTART:20260802T090000Z\r\nDTEND:20260802T100000Z\r\nSUMMARY:Imported Two\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n"
	resp := h.do(t, h.req(t, http.MethodPost, base+"events/import", strings.NewReader(ics)))
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("import status %d", resp.StatusCode)
	}
	var out map[string]int
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["imported"] != 2 || out["skipped"] != 0 {
		t.Fatalf("import result %+v", out)
	}

	resp2 := h.do(t, h.req(t, http.MethodGet, base+"events/export?calendar=personal", nil))
	defer resp2.Body.Close()
	body, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(body), "UID:imp-1") || !strings.Contains(string(body), "UID:imp-2") {
		t.Fatalf("export missing imported uids:\n%s", body)
	}
}

func TestFreebusyMerges(t *testing.T) {
	h := newHarness(t)
	base0 := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	put := func(sum string, s, e time.Time, transp string) {
		if _, err := h.st.PutEvent(h.user, "personal", &event.Event{Summary: sum, Start: s, End: e, Transparency: transp}); err != nil {
			t.Fatalf("put %s: %v", sum, err)
		}
	}
	put("Opaque1", base0, base0.Add(time.Hour), "")                          // 09:00-10:00
	put("Opaque2", base0.Add(30*time.Minute), base0.Add(90*time.Minute), "") // 09:30-10:30 (overlaps -> merge)
	put("Free", base0, base0.Add(3*time.Hour), "TRANSPARENT")                // excluded

	q := "?calendar=personal&start=" + base0.Add(-time.Hour).Format(time.RFC3339) + "&end=" + base0.Add(5*time.Hour).Format(time.RFC3339)
	resp := h.do(t, h.req(t, http.MethodGet, base+"freebusy"+q, nil))
	defer resp.Body.Close()
	var out struct {
		Busy []struct {
			Start time.Time `json:"start"`
			End   time.Time `json:"end"`
		} `json:"busy"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Busy) != 1 {
		t.Fatalf("expected 1 merged busy slot, got %d: %+v", len(out.Busy), out.Busy)
	}
	if !out.Busy[0].Start.Equal(base0) || !out.Busy[0].End.Equal(base0.Add(90*time.Minute)) {
		t.Fatalf("merged slot wrong: %+v", out.Busy[0])
	}
}

func TestDavRequiresAuthWithChallenge(t *testing.T) {
	h := newHarness(t)
	// No session, no Basic credentials → 401 with a Basic challenge so native clients prompt.
	req, _ := http.NewRequest("PROPFIND", h.srv.URL+base+"dav/", strings.NewReader(""))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth DAV: want 401, got %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("missing Basic challenge: %q", resp.Header.Get("WWW-Authenticate"))
	}
}

func TestDavSessionCookie(t *testing.T) {
	h := newHarness(t)
	body := `<D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">` +
		`<D:prop><C:calendar-home-set/></D:prop></D:propfind>`
	req := h.req(t, "PROPFIND", base+"dav/principals/"+h.user+"/", strings.NewReader(body))
	req.Header.Set("Depth", "0")
	resp := h.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("session DAV PROPFIND: want 207, got %d", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), base+"dav/calendars/"+h.user+"/") {
		t.Fatalf("calendar-home-set missing:\n%s", out)
	}
}

func TestDavAppPasswordBasic(t *testing.T) {
	h := newHarness(t)
	// Mint an app password through the session-authenticated endpoint.
	resp := h.do(t, h.req(t, http.MethodPost, base+"apppasswords", strings.NewReader(`{"label":"phone"}`)))
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("create app password: status %d", resp.StatusCode)
	}
	var created struct {
		Token    string `json:"token"`
		Username string `json:"username"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil || created.Token == "" {
		t.Fatalf("decode app password: %v (%+v)", err, created)
	}

	// Authenticate a native-client PROPFIND purely via HTTP Basic (no session cookie).
	req, _ := http.NewRequest("PROPFIND", h.srv.URL+base+"dav/calendars/"+h.user+"/", strings.NewReader(""))
	req.SetBasicAuth(created.Username, created.Token)
	req.Header.Set("Depth", "0")
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("basic PROPFIND: %v", err)
	}
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusMultiStatus {
		t.Fatalf("app-password DAV PROPFIND: want 207, got %d", r2.StatusCode)
	}

	// A wrong app password is rejected.
	req2, _ := http.NewRequest("PROPFIND", h.srv.URL+base+"dav/calendars/"+h.user+"/", strings.NewReader(""))
	req2.SetBasicAuth(created.Username, "0000000000000000000000000000000000000000000000bad")
	r3, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("bad app password PROPFIND: %v", err)
	}
	defer r3.Body.Close()
	if r3.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad app password: want 401, got %d", r3.StatusCode)
	}
}

func TestClientIPTrustsRightmostXFF(t *testing.T) {
	// Caddy appends the real peer to X-Forwarded-For, so the rightmost entry is trustworthy and
	// the leftmost is client-controlled. The throttle must key on the rightmost to resist spoofing.
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 9.9.9.9, 203.0.113.7")
	if got := clientIP(r); got != "203.0.113.7" {
		t.Fatalf("clientIP = %q, want the rightmost (proxy-appended) 203.0.113.7", got)
	}
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.RemoteAddr = "198.51.100.2:5555"
	if got := clientIP(r2); got != "198.51.100.2" {
		t.Fatalf("clientIP without XFF = %q, want 198.51.100.2", got)
	}
}

func TestRSVPPreservedAcrossOrganizerEdit(t *testing.T) {
	h := newHarness(t)
	start := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	ev := &event.Event{Summary: "Sync", Start: start, End: start.Add(time.Hour),
		Attendees: []event.Participant{{Email: "guest@example.com", Role: "REQ-PARTICIPANT", PartStat: "ACCEPTED"}}}
	if _, err := h.st.PutEvent(h.user, "personal", ev); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Organizer edits the title; the editor resubmits the guest as NEEDS-ACTION.
	body := `{"uid":"` + ev.UID + `","calendarId":"personal","summary":"Sync (renamed)",` +
		`"start":"2026-09-01T09:00:00Z","end":"2026-09-01T10:00:00Z",` +
		`"attendees":[{"email":"guest@example.com","role":"REQ-PARTICIPANT","partStat":"NEEDS-ACTION"}]}`
	resp := h.do(t, h.req(t, http.MethodPost, base+"events", strings.NewReader(body)))
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("edit status %d", resp.StatusCode)
	}
	got, _, err := h.st.GetEvent(h.user, "personal", ev.UID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Attendees) != 1 || got.Attendees[0].PartStat != "ACCEPTED" {
		t.Fatalf("collected RSVP must survive an organizer edit: %+v", got.Attendees)
	}
}

func TestAttendeeDedup(t *testing.T) {
	h := newHarness(t)
	body := `{"calendarId":"personal","summary":"Dup","start":"2026-09-02T09:00:00Z","end":"2026-09-02T10:00:00Z",` +
		`"attendees":[{"email":"x@example.com","role":"REQ-PARTICIPANT"},{"email":"X@example.com","role":"OPT-PARTICIPANT"},{"email":"y@example.com","role":"REQ-PARTICIPANT"}]}`
	resp := h.do(t, h.req(t, http.MethodPost, base+"events", strings.NewReader(body)))
	defer resp.Body.Close()
	var out struct {
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	got, _, err := h.st.GetEvent(h.user, "personal", out.UID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Attendees) != 2 { // x@ and X@ collapse to one; y@ distinct
		t.Fatalf("duplicate addresses should be deduped: %+v", got.Attendees)
	}
}

func TestGeocodeEndpoint(t *testing.T) {
	h := newHarness(t)
	// Suggest proxies the provider and returns its suggestions.
	resp := h.do(t, h.req(t, http.MethodGet, base+"geocode?q=Ginza&session=s1", nil))
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("geocode suggest status %d", resp.StatusCode)
	}
	var sug struct {
		Provider    string `json:"provider"`
		Suggestions []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"suggestions"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&sug)
	if sug.Provider != "fake" || len(sug.Suggestions) != 1 || sug.Suggestions[0].ID != "p1" {
		t.Fatalf("unexpected suggest result: %+v", sug)
	}

	// A 1-char query short-circuits to an empty list (no upstream call).
	r2 := h.do(t, h.req(t, http.MethodGet, base+"geocode?q=G", nil))
	defer r2.Body.Close()
	var sug2 struct {
		Suggestions []any `json:"suggestions"`
	}
	_ = json.NewDecoder(r2.Body).Decode(&sug2)
	if len(sug2.Suggestions) != 0 {
		t.Fatalf("1-char query should return no suggestions, got %d", len(sug2.Suggestions))
	}

	// Resolve returns the picked place with coordinates.
	r3 := h.do(t, h.req(t, http.MethodGet, base+"geocode/resolve?id=p1&session=s1", nil))
	defer r3.Body.Close()
	var place struct {
		Address string  `json:"address"`
		Lat     float64 `json:"lat"`
		Lon     float64 `json:"lon"`
	}
	_ = json.NewDecoder(r3.Body).Decode(&place)
	if place.Lat != 53.5754 || place.Lon != 9.9586 || place.Address == "" {
		t.Fatalf("unexpected resolve result: %+v", place)
	}
}

func TestStreamHello(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp := h.do(t, h.req(t, http.MethodGet, base+"events/stream", nil).WithContext(ctx))
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("stream content-type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.Contains(sc.Text(), "event: hello") {
			return // success
		}
	}
	t.Fatal("never received a hello frame")
}
