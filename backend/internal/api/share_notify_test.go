package api

import (
	"testing"

	"icaly/internal/auth"
	"icaly/internal/event"
	"icaly/internal/imip"
	"icaly/internal/instance"
	"icaly/internal/scheduling"
	"icaly/internal/store"
)

type fakeMailer struct{ sent []imip.SendInput }

func (f *fakeMailer) Enabled() bool                { return true }
func (f *fakeMailer) Send(in imip.SendInput) error { f.sent = append(f.sent, in); return nil }

func TestNotifyShareExternalEmail(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fm := &fakeMailer{}
	s := &Server{st: st, inst: instance.New(), sched: scheduling.New(st, instance.New(), fm)}

	// admin ⇒ passes the hp_icaly_invite + hp_mail_send gate.
	u := &auth.User{Username: "alice", IsAdmin: true}
	cal := event.Calendar{ID: "personal", Owner: "alice", Name: "Fam", FeedToken: "tok123"}
	g := store.Grant{ID: "g1", Kind: "adhoc", PublicExt: true}
	goodURL := "webcal://h/api/services/icaly/feeds/tok123.ics"

	// Consent + rights + a URL bound to this feed token ⇒ one email to the external.
	s.notifyShare(u, cal, g, []string{"ext@example.com", "ext@example.com"}, goodURL)
	if len(fm.sent) != 1 {
		t.Fatalf("expected 1 external email, got %d", len(fm.sent))
	}
	if len(fm.sent[0].To) != 1 || fm.sent[0].To[0] != "ext@example.com" || fm.sent[0].FromUser != "alice" {
		t.Fatalf("wrong recipient/sender: %+v", fm.sent[0])
	}

	// A URL NOT pointing at this calendar's token is rejected (anti-phishing) → no email.
	fm.sent = nil
	s.notifyShare(u, cal, g, []string{"ext@example.com"}, "webcal://evil/feeds/OTHER.ics")
	if len(fm.sent) != 0 {
		t.Fatalf("mismatched feed URL must send nothing, got %+v", fm.sent)
	}

	// No consent (public_ext off) → no email even with a valid URL.
	g.PublicExt = false
	s.notifyShare(u, cal, g, []string{"ext@example.com"}, goodURL)
	if len(fm.sent) != 0 {
		t.Fatalf("no consent must send nothing, got %+v", fm.sent)
	}

	// Without the invite/mail-send rights (non-admin, no groups) → no email.
	g.PublicExt = true
	s.notifyShare(&auth.User{Username: "alice"}, cal, g, []string{"ext@example.com"}, goodURL)
	if len(fm.sent) != 0 {
		t.Fatalf("without rights must send nothing, got %+v", fm.sent)
	}
}

func TestShareFeedLink(t *testing.T) {
	if got := shareFeedLink("webcal://h/x/feeds/tok.ics", "tok"); got == "" {
		t.Fatal("matching token should be accepted")
	}
	if got := shareFeedLink("webcal://h/x/feeds/other.ics", "tok"); got != "" {
		t.Fatalf("non-matching token should be rejected, got %q", got)
	}
	if got := shareFeedLink("", "tok"); got != "" {
		t.Fatal("empty url rejected")
	}
	if got := shareFeedLink("webcal://h/x/feeds/tok.ics", ""); got != "" {
		t.Fatal("empty token rejected")
	}
}

func TestCleanExternals(t *testing.T) {
	got := cleanExternals([]string{"A@X.com", "a@x.com", " b@x.com ", "notanemail", "c d@x.com", ""})
	if len(got) != 2 || got[0] != "a@x.com" || got[1] != "b@x.com" {
		t.Fatalf("unexpected cleanup: %+v", got)
	}
}
