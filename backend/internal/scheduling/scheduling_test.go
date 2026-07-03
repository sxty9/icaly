package scheduling

import (
	"os/user"
	"strings"
	"testing"
	"time"

	"icaly/internal/event"
	"icaly/internal/imip"
	"icaly/internal/instance"
	"icaly/internal/store"
)

type fakeMailer struct {
	enabled bool
	sent    []imip.SendInput
}

func (f *fakeMailer) Enabled() bool { return f.enabled }
func (f *fakeMailer) Send(in imip.SendInput) error {
	f.sent = append(f.sent, in)
	return nil
}

func setup(t *testing.T, mailer Mailer) (*Scheduler, *store.Store, string) {
	t.Helper()
	cur, err := user.Current()
	if err != nil || cur.Username == "" {
		t.Skip("no current OS user")
	}
	t.Setenv("HOLISTIC_MAIL_DOMAIN", "test.local")
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(st, instance.New(), mailer), st, cur.Username
}

func TestExternalInviteGate(t *testing.T) {
	m := &fakeMailer{enabled: true}
	s, _, organizer := setup(t, m)
	start := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	ev := &event.Event{UID: "ext-1", Summary: "Kickoff", Start: start, End: start.Add(time.Hour),
		Attendees: []event.Participant{{Email: "ext@gmail.com"}}}

	// Without the external-invite right, nothing is mailed.
	out, err := s.OnOrganizerSave(organizer, ev, false)
	if err != nil || out.ExternalSkipped != 1 || out.ExternalSent != 0 || len(m.sent) != 0 {
		t.Fatalf("gated save: out=%+v err=%v sent=%d", out, err, len(m.sent))
	}

	// With the right (and an enabled transport), a REQUEST goes out.
	out, err = s.OnOrganizerSave(organizer, ev, true)
	if err != nil || out.ExternalSent != 1 {
		t.Fatalf("permitted save: out=%+v err=%v", out, err)
	}
	if len(m.sent) != 1 || m.sent[0].Method != "REQUEST" || m.sent[0].FromUser != organizer {
		t.Fatalf("unexpected send: %+v", m.sent)
	}
	if len(m.sent[0].To) != 1 || m.sent[0].To[0] != "ext@gmail.com" {
		t.Fatalf("recipient: %+v", m.sent[0].To)
	}
}

func TestExternalInviteSkippedWhenTransportDisabled(t *testing.T) {
	m := &fakeMailer{enabled: false}
	s, _, organizer := setup(t, m)
	ev := &event.Event{UID: "ext-2", Summary: "x", Start: time.Now().UTC(),
		Attendees: []event.Participant{{Email: "ext@gmail.com"}}}
	out, _ := s.OnOrganizerSave(organizer, ev, true)
	if out.ExternalSkipped != 1 || len(m.sent) != 0 {
		t.Fatalf("disabled transport should skip: out=%+v sent=%d", out, len(m.sent))
	}
}

func TestAttendeeRolesInInvite(t *testing.T) {
	m := &fakeMailer{enabled: true}
	s, _, organizer := setup(t, m)
	start := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	ev := &event.Event{UID: "roles-1", Summary: "Kickoff", Start: start, End: start.Add(time.Hour),
		Attendees: []event.Participant{
			{Email: "req@gmail.com", Role: "REQ-PARTICIPANT"},
			{Email: "opt@gmail.com", Role: "OPT-PARTICIPANT"},
		}}
	out, err := s.OnOrganizerSave(organizer, ev, true)
	if err != nil || out.ExternalSent != 2 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if len(m.sent) != 1 || len(m.sent[0].To) != 2 {
		t.Fatalf("expected one send to two recipients: %+v", m.sent)
	}
	// The optional attendee's role rides along in the invitation so the client shows it as optional.
	if !strings.Contains(m.sent[0].ICS, "ROLE=OPT-PARTICIPANT") {
		t.Fatalf("optional role missing from invite ICS:\n%s", m.sent[0].ICS)
	}
}

func TestOccurrenceCancelMailsInstance(t *testing.T) {
	m := &fakeMailer{enabled: true}
	s, _, organizer := setup(t, m)
	start := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	ev := &event.Event{UID: "cancel-1", Summary: "Weekly", Start: start, End: start.Add(time.Hour), RRule: "FREQ=WEEKLY;COUNT=5",
		Attendees: []event.Participant{{Email: "ext@gmail.com", Role: "REQ-PARTICIPANT"}}}
	occ := start.Add(14 * 24 * time.Hour)
	if err := s.OnOccurrenceCancel(organizer, ev, occ, false, true); err != nil {
		t.Fatalf("occurrence cancel: %v", err)
	}
	if len(m.sent) != 1 || m.sent[0].Method != "CANCEL" {
		t.Fatalf("expected one CANCEL send: %+v", m.sent)
	}
	if !strings.Contains(m.sent[0].ICS, "RECURRENCE-ID") {
		t.Fatalf("occurrence CANCEL must carry RECURRENCE-ID:\n%s", m.sent[0].ICS)
	}
}

func TestInboundReplyFromMatching(t *testing.T) {
	m := &fakeMailer{enabled: true}
	s, st, organizer := setup(t, m)
	start := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)
	ev := &event.Event{UID: "reply-uid", Summary: "Review", Start: start, End: start.Add(time.Hour),
		Attendees: []event.Participant{{Email: "ext@gmail.com", PartStat: "NEEDS-ACTION"}}}
	if _, err := st.PutEvent(organizer, "personal", ev); err != nil {
		t.Fatalf("seed organizer event: %v", err)
	}
	rcpt := organizer + "@test.local"

	reply := "From: ext@gmail.com\r\nTo: " + rcpt + "\r\nSubject: Re\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: text/calendar; method=REPLY\r\n\r\n" +
		"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REPLY\r\nBEGIN:VEVENT\r\nUID:reply-uid\r\n" +
		"ATTENDEE;PARTSTAT=ACCEPTED:mailto:ext@gmail.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

	if err := s.OnInbound("ext@gmail.com", []string{rcpt}, []byte(reply)); err != nil {
		t.Fatalf("inbound reply: %v", err)
	}
	got, _, err := st.GetEvent(organizer, "personal", "reply-uid")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Attendees) != 1 || got.Attendees[0].PartStat != "ACCEPTED" {
		t.Fatalf("partstat not updated: %+v", got.Attendees)
	}

	// A REPLY whose From is not an attendee is rejected (plan M4).
	spoof := reply // same body, but delivered claiming a different sender
	if err := s.OnInbound("stranger@evil.com", []string{rcpt}, []byte(spoof)); err != ErrNotAttendee {
		t.Fatalf("expected ErrNotAttendee for spoofed reply, got %v", err)
	}
}

func TestInboundRequestCreatesTentative(t *testing.T) {
	m := &fakeMailer{enabled: true}
	s, st, recipient := setup(t, m)
	rcpt := recipient + "@test.local"
	req := "From: boss@partner.com\r\nTo: " + rcpt + "\r\nSubject: Invite\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: text/calendar; method=REQUEST\r\n\r\n" +
		"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:inbound-req\r\n" +
		"DTSTART:20260903T090000Z\r\nSUMMARY:Partner sync\r\nORGANIZER:mailto:boss@partner.com\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	if err := s.OnInbound("boss@partner.com", []string{rcpt}, []byte(req)); err != nil {
		t.Fatalf("inbound request: %v", err)
	}
	got, _, err := st.GetEvent(recipient, "personal", "inbound-req")
	if err != nil {
		t.Fatalf("tentative event not created: %v", err)
	}
	if got.Status != "TENTATIVE" || got.Summary != "Partner sync" {
		t.Fatalf("unexpected tentative event: %+v", got)
	}
}
