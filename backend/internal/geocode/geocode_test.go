package geocode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPhotonSuggest(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		_, _ = w.Write([]byte(`{"features":[
			{"geometry":{"coordinates":[9.9586,53.5754]},
			 "properties":{"name":"Friseursalon Ginza Matsunaga","street":"Eppendorfer Weg","housenumber":"211","postcode":"20253","city":"Hamburg","country":"Germany"}}
		]}`))
	}))
	defer srv.Close()

	svc := NewWith(&photon{client: srv.Client(), endpoint: srv.URL + "/"})
	if !svc.Enabled() || svc.Provider() != "photon" {
		t.Fatalf("provider: enabled=%v name=%q", svc.Enabled(), svc.Provider())
	}
	res, err := svc.Suggest(context.Background(), "Friseursalon Ginza", "")
	if err != nil {
		t.Fatalf("suggest: %v", err)
	}
	if gotQuery != "Friseursalon Ginza" {
		t.Fatalf("query not forwarded: %q", gotQuery)
	}
	if len(res) != 1 {
		t.Fatalf("want 1 suggestion, got %d", len(res))
	}
	s := res[0]
	// Photon results arrive resolved: coords + a built address, no second call needed.
	if !s.Resolved || s.Lat != 53.5754 || s.Lon != 9.9586 {
		t.Fatalf("coords/resolved wrong: %+v", s)
	}
	if s.Primary != "Friseursalon Ginza Matsunaga" {
		t.Fatalf("primary: %q", s.Primary)
	}
	if !strings.Contains(s.Label, "Eppendorfer Weg 211") || !strings.Contains(s.Label, "20253 Hamburg") {
		t.Fatalf("label missing address parts: %q", s.Label)
	}
}

func TestGoogleSuggestAndResolve(t *testing.T) {
	var autoHits, detailHits int
	var fieldMask, sentSession string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/places:autocomplete"):
			autoHits++
			if r.Header.Get("X-Goog-Api-Key") != "test-key" {
				t.Errorf("missing api key header")
			}
			_, _ = w.Write([]byte(`{"suggestions":[
				{"placePrediction":{"placeId":"ChIJabc","text":{"text":"Friseursalon Ginza Matsunaga, Eppendorfer Weg 211, Hamburg"},
				 "structuredFormat":{"mainText":{"text":"Friseursalon Ginza Matsunaga"},"secondaryText":{"text":"Eppendorfer Weg 211, Hamburg"}}}},
				{"queryPrediction":{"text":{"text":"ignored — no placeId"}}}
			]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/places/"):
			detailHits++
			fieldMask = r.Header.Get("X-Goog-FieldMask")
			sentSession = r.URL.Query().Get("sessionToken")
			_, _ = w.Write([]byte(`{"id":"ChIJabc","formattedAddress":"Eppendorfer Weg 211, 20253 Hamburg, Germany","location":{"latitude":53.5754,"longitude":9.9586}}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	svc := NewWith(&google{key: "test-key", client: srv.Client(), base: srv.URL})
	res, err := svc.Suggest(context.Background(), "Friseursalon Ginza", "sess-1")
	if err != nil {
		t.Fatalf("suggest: %v", err)
	}
	// The query prediction (no placeId) is dropped; only the place prediction survives, unresolved.
	if len(res) != 1 || res[0].ID != "ChIJabc" || res[0].Resolved {
		t.Fatalf("unexpected suggestions: %+v", res)
	}
	if res[0].Primary != "Friseursalon Ginza Matsunaga" {
		t.Fatalf("primary: %q", res[0].Primary)
	}

	place, err := svc.Resolve(context.Background(), "ChIJabc", "sess-1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if place.Lat != 53.5754 || place.Lon != 9.9586 || !strings.Contains(place.Address, "Hamburg") {
		t.Fatalf("resolved place wrong: %+v", place)
	}
	// Cost guards: Essentials field mask (no displayName) and the session token is forwarded.
	if strings.Contains(fieldMask, "displayName") {
		t.Fatalf("field mask must not request the Pro displayName field: %q", fieldMask)
	}
	if !strings.Contains(fieldMask, "formattedAddress") || !strings.Contains(fieldMask, "location") {
		t.Fatalf("field mask missing essentials: %q", fieldMask)
	}
	if sentSession != "sess-1" {
		t.Fatalf("session token not forwarded to details: %q", sentSession)
	}

	// A second identical resolve is served from cache (no extra upstream hit).
	if _, err := svc.Resolve(context.Background(), "ChIJabc", "sess-1"); err != nil {
		t.Fatalf("cached resolve: %v", err)
	}
	if detailHits != 1 {
		t.Fatalf("resolve cache miss: detailHits=%d", detailHits)
	}
}

func TestSuggestCacheAndDisabled(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"features":[{"geometry":{"coordinates":[1,2]},"properties":{"name":"X"}}]}`))
	}))
	defer srv.Close()
	svc := NewWith(&photon{client: srv.Client(), endpoint: srv.URL + "/"})
	for i := 0; i < 3; i++ {
		if _, err := svc.Suggest(context.Background(), "same query", ""); err != nil {
			t.Fatalf("suggest: %v", err)
		}
	}
	if hits != 1 {
		t.Fatalf("expected 1 upstream hit (cached), got %d", hits)
	}

	// Disabled service: Enabled() false and Suggest returns ErrDisabled.
	off := New("none", "", 0)
	if off.Enabled() {
		t.Fatal("provider 'none' should be disabled")
	}
	if _, err := off.Suggest(context.Background(), "x", ""); err != ErrDisabled {
		t.Fatalf("disabled suggest: want ErrDisabled, got %v", err)
	}
}

func TestDailyBudgetCap(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"features":[{"geometry":{"coordinates":[1,2]},"properties":{"name":"X"}}]}`))
	}))
	defer srv.Close()
	svc := NewWith(&photon{client: srv.Client(), endpoint: srv.URL + "/"})
	svc.dailyCap = 2

	// Distinct queries bypass the cache, so each consumes one budget unit.
	for _, q := range []string{"q1", "q2"} {
		if _, err := svc.Suggest(context.Background(), q, ""); err != nil {
			t.Fatalf("suggest %s: %v", q, err)
		}
	}
	if _, err := svc.Suggest(context.Background(), "q3", ""); err != ErrBudgetExceeded {
		t.Fatalf("over-cap query: want ErrBudgetExceeded, got %v", err)
	}
	if hits != 2 {
		t.Fatalf("expected exactly 2 upstream calls under the cap, got %d", hits)
	}
	// A cached query still resolves after the cap is hit (cache hits don't consume budget).
	if _, err := svc.Suggest(context.Background(), "q1", ""); err != nil {
		t.Fatalf("cached query should bypass the budget: %v", err)
	}
}

func TestProviderSelection(t *testing.T) {
	if New("", "", 0).Provider() != "photon" {
		t.Fatal("auto with no key should be photon")
	}
	if New("", "a-key", 0).Provider() != "google" {
		t.Fatal("auto with a key should be google")
	}
	if New("google", "", 0).Provider() != "photon" {
		t.Fatal("google with no key should fall back to photon")
	}
	if New("photon", "a-key", 0).Provider() != "photon" {
		t.Fatal("explicit photon should stay photon even with a key")
	}
}
