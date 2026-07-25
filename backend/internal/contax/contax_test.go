package contax

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestContaxMemberCacheAndTTL(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Contax-Internal-Secret") != "sekret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"grp-1","name":"Fam","usernames":["alice","bob"]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "sekret")
	now := time.Unix(1000, 0)
	c.nowFn = func() time.Time { return now }

	if !c.ContaxMember("grp-1", "alice") {
		t.Fatal("alice should be a member")
	}
	if c.ContaxMember("grp-1", "carol") {
		t.Fatal("carol should not be a member")
	}
	if c.ContaxMember("grp-1", "bob"); atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("expected 1 fetch (cache answers every user), got %d", calls)
	}

	// Past the TTL a new question refetches.
	now = now.Add(ttl + time.Second)
	_ = c.ContaxMember("grp-1", "alice")
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected 2 fetches after TTL, got %d", calls)
	}
}

func TestContaxDisabled(t *testing.T) {
	c := New("", "")
	if c.Enabled() {
		t.Fatal("client with no URL/secret should be disabled")
	}
	if c.ContaxMember("grp-1", "alice") {
		t.Fatal("disabled client resolves no members")
	}
}

func TestContaxStaleOnError(t *testing.T) {
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"usernames":["alice"]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "s")
	now := time.Unix(1000, 0)
	c.nowFn = func() time.Time { return now }
	if !c.ContaxMember("grp-1", "alice") {
		t.Fatal("alice should be a member on the fresh fetch")
	}
	// Expire the entry and break the server: a transient error serves the stale set, not a denial.
	now = now.Add(ttl + time.Second)
	fail.Store(true)
	if !c.ContaxMember("grp-1", "alice") {
		t.Fatal("should serve stale membership on a fetch error")
	}
}
