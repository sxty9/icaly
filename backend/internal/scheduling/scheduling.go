// Package scheduling is icaly's transport-neutral iTIP (RFC 5546) state machine plus RFC 6638
// delivery. Internal attendees (a local Holistic user at this instance's mail domain) are served
// in-process — a NEEDS-ACTION copy is written straight into their calendar, no mail involved.
// External attendees go out as iMIP (RFC 6047) through the mailer (internal/imip → maild). The
// same machine handles inbound REPLY (match by UID, verify the From is a known attendee, update
// PARTSTAT) and REQUEST/CANCEL (a tentative event for a local invitee of an external organizer).
package scheduling

import (
	"os/user"
	"strings"
	"time"

	"icaly/internal/event"
	"icaly/internal/ical"
	"icaly/internal/imip"
	"icaly/internal/instance"
	"icaly/internal/store"
)

// Mailer is the outbound iMIP transport (implemented by *imip.Client; faked in tests).
type Mailer interface {
	Enabled() bool
	Send(imip.SendInput) error
}

// Scheduler coordinates iTIP delivery over the store.
type Scheduler struct {
	st     *store.Store
	inst   *instance.Resolver
	mailer Mailer
}

// New builds a scheduler.
func New(st *store.Store, inst *instance.Resolver, mailer Mailer) *Scheduler {
	return &Scheduler{st: st, inst: inst, mailer: mailer}
}

// Outcome summarises what an organizer save dispatched.
type Outcome struct {
	InternalDelivered int `json:"internalDelivered"`
	ExternalSent      int `json:"externalSent"`
	ExternalSkipped   int `json:"externalSkipped"` // external attendees not mailed (no right / no transport)
}

// ErrNotAttendee rejects an inbound REPLY whose From is not a known attendee (plan M4).
var ErrNotAttendee = errStr("reply sender is not an attendee of the event")

type errStr string

func (e errStr) Error() string { return string(e) }

// OnOrganizerSave dispatches invitations after the organizer saved ev (which must already have a
// UID). Internal attendees get an in-process copy; external ones are mailed iff canInviteExternal
// and a transport is configured. The attendee slice is annotated with resolved internal identity.
func (s *Scheduler) OnOrganizerSave(organizer string, ev *event.Event, canInviteExternal bool) (Outcome, error) {
	var out Outcome
	if len(ev.Attendees) == 0 {
		return out, nil
	}
	if ev.Organizer == nil || ev.Organizer.Email == "" {
		ev.Organizer = &event.Participant{Email: s.inst.Address(organizer), Username: organizer, IsInternal: true}
	}
	var external []string
	for i := range ev.Attendees {
		a := &ev.Attendees[i]
		if u, internal := s.classify(a.Email); internal {
			a.IsInternal, a.Username = true, u
			if u != organizer {
				if err := s.deliverInternal(u, ev, a.Email); err == nil {
					out.InternalDelivered++
				}
			}
		} else {
			a.IsInternal = false
			external = append(external, a.Email)
		}
	}
	if len(external) == 0 {
		return out, nil
	}
	if !canInviteExternal || !s.mailer.Enabled() {
		out.ExternalSkipped = len(external)
		return out, nil
	}
	ics, err := ical.EncodeWithMethod(ev, "REQUEST")
	if err != nil {
		out.ExternalSkipped = len(external)
		return out, err
	}
	if err := s.mailer.Send(imip.SendInput{
		FromUser: organizer, To: external,
		Subject: "Invitation: " + titleOf(ev), Body: invitationBody(ev), ICS: string(ics), Method: "REQUEST",
	}); err != nil {
		out.ExternalSkipped = len(external)
		return out, err
	}
	out.ExternalSent = len(external)
	return out, nil
}

// OnOrganizerCancel notifies attendees that an event was cancelled: internal copies are removed,
// external attendees receive an iTIP CANCEL.
func (s *Scheduler) OnOrganizerCancel(organizer string, ev *event.Event, canInviteExternal bool) error {
	var external []string
	for _, a := range ev.Attendees {
		if u, internal := s.classify(a.Email); internal {
			if u != organizer {
				if _, calID, _, err := s.st.FindEventByUID(u, ev.UID); err == nil {
					_ = s.st.DeleteEvent(u, calID, ev.UID)
				}
			}
		} else {
			external = append(external, a.Email)
		}
	}
	if len(external) > 0 && canInviteExternal && s.mailer.Enabled() {
		cancel := ev.Clone()
		cancel.Status = "CANCELLED"
		if ics, err := ical.EncodeWithMethod(cancel, "CANCEL"); err == nil {
			_ = s.mailer.Send(imip.SendInput{FromUser: organizer, To: external,
				Subject: "Cancelled: " + titleOf(ev), Body: "This event has been cancelled.", ICS: string(ics), Method: "CANCEL"})
		}
	}
	return nil
}

// OnAttendeeReply records a local user's RSVP on their own copy and propagates it to the
// organizer — in-process if the organizer is internal, else as an iTIP REPLY by mail.
func (s *Scheduler) OnAttendeeReply(replier, calID, uid, partStat string) error {
	ev, _, err := s.st.GetEvent(replier, calID, uid)
	if err != nil {
		return err
	}
	myEmail := s.inst.Address(replier)
	setPartStat(ev, myEmail, partStat)
	if _, err := s.st.PutEvent(replier, calID, ev); err != nil {
		return err
	}
	org := ev.Organizer
	if org == nil || org.Email == "" {
		return nil
	}
	if orgUser, internal := s.classify(org.Email); internal {
		if orgUser == replier {
			return nil
		}
		oev, oCal, _, err := s.st.FindEventByUID(orgUser, uid)
		if err != nil {
			return nil // organizer no longer has it; nothing to update
		}
		setPartStat(oev, myEmail, partStat)
		_, _ = s.st.PutEvent(orgUser, oCal, oev)
		return nil
	}
	if !s.mailer.Enabled() {
		return nil
	}
	reply := ev.Clone()
	reply.Attendees = []event.Participant{{Email: myEmail, PartStat: partStat}}
	ics, err := ical.EncodeWithMethod(reply, "REPLY")
	if err != nil {
		return err
	}
	return s.mailer.Send(imip.SendInput{FromUser: replier, To: []string{org.Email},
		Subject: "Re: " + titleOf(ev), Body: "Response: " + partStat, ICS: string(ics), Method: "REPLY"})
}

// OnInbound processes a forwarded inbound iMIP message (raw RFC 822 from maild). fromAddr is the
// envelope/header From; rcpts are the recipients. Returns ErrNotAttendee for a spoofed REPLY.
func (s *Scheduler) OnInbound(fromAddr string, rcpts []string, raw []byte) error {
	ics := imip.ExtractCalendar(raw)
	if ics == "" {
		return nil
	}
	ev, err := ical.Decode([]byte(ics))
	if err != nil || ev.UID == "" {
		return nil
	}
	recipient := s.firstLocalRecipient(rcpts)
	switch ical.Method([]byte(ics)) {
	case "REPLY":
		if recipient == "" {
			return nil
		}
		oev, calID, _, err := s.st.FindEventByUID(recipient, ev.UID)
		if err != nil {
			return nil
		}
		if !attendeeMatches(oev, fromAddr) { // plan M4: only a known attendee may reply
			return ErrNotAttendee
		}
		ps := partStatFor(ev, fromAddr)
		if ps == "" {
			ps = "ACCEPTED"
		}
		setPartStat(oev, fromAddr, ps)
		_, err = s.st.PutEvent(recipient, calID, oev)
		return err
	case "REQUEST":
		if recipient == "" {
			return nil
		}
		invite := ev.Clone()
		invite.CalendarID = "personal"
		if invite.Status == "" {
			invite.Status = "TENTATIVE"
		}
		_, err := s.st.PutEvent(recipient, "personal", invite)
		return err
	case "CANCEL":
		if recipient == "" {
			return nil
		}
		if _, calID, _, err := s.st.FindEventByUID(recipient, ev.UID); err == nil {
			return s.st.DeleteEvent(recipient, calID, ev.UID)
		}
	}
	return nil
}

// ── helpers ─────────────────────────────────────────────────────────────────────────

func (s *Scheduler) deliverInternal(recipient string, ev *event.Event, recipientEmail string) error {
	copy := ev.Clone()
	copy.CalendarID = "personal"
	// A per-occurrence (RECURRENCE-ID) update must be merged into the recipient's existing series
	// as an override — a plain PutEvent by UID would overwrite their whole series copy with this
	// single instance (attendee-side data loss).
	if copy.RecurrenceID != nil {
		if _, _, err := s.st.GetEvent(recipient, "personal", ev.UID); err == nil {
			return s.st.OverrideOccurrence(recipient, "personal", ev.UID, copy)
		}
		// Recipient was invited only to this occurrence (they don't hold the series): deliver it as
		// a standalone event rather than silently dropping the invitation.
		copy.RecurrenceID, copy.RRule, copy.ExDates = nil, "", nil
		_, err := s.st.PutEvent(recipient, "personal", copy)
		return err
	}
	// Preserve the recipient's prior RSVP if they already hold this event (re-invite on edit).
	if prev, _, err := s.st.GetEvent(recipient, "personal", ev.UID); err == nil {
		if ps := partStatFor(prev, recipientEmail); ps != "" {
			setPartStat(copy, recipientEmail, ps)
		}
	}
	_, err := s.st.PutEvent(recipient, "personal", copy)
	return err
}

// OnOccurrenceCancel notifies attendees when the organizer deletes part of a recurring series:
// "this occurrence" (following=false) or "this and following" (following=true). Internal
// attendees' copies are updated in place (EXDATE / truncate); external attendees get an iTIP
// CANCEL for the affected instance. recurrenceID is the occurrence's original start.
func (s *Scheduler) OnOccurrenceCancel(organizer string, ev *event.Event, recurrenceID time.Time, following, canInviteExternal bool) error {
	var external []string
	for _, a := range ev.Attendees {
		if u, internal := s.classify(a.Email); internal {
			if u == organizer {
				continue
			}
			if _, calID, _, err := s.st.FindEventByUID(u, ev.UID); err == nil {
				if following {
					_ = s.st.TruncateSeries(u, calID, ev.UID, recurrenceID)
				} else {
					_ = s.st.ExcludeOccurrence(u, calID, ev.UID, recurrenceID)
				}
			}
		} else {
			external = append(external, a.Email)
		}
	}
	if len(external) == 0 || !canInviteExternal || !s.mailer.Enabled() {
		return nil
	}
	// A single-instance CANCEL. (A "following" delete ideally carries RECURRENCE-ID;RANGE=
	// THISANDFUTURE; we send a per-instance CANCEL as a pragmatic notification for now.)
	cancel := ev.Clone()
	cancel.Status = "CANCELLED"
	cancel.RRule, cancel.ExDates = "", nil
	rid := recurrenceID.UTC()
	cancel.RecurrenceID = &rid
	ics, err := ical.EncodeWithMethod(cancel, "CANCEL")
	if err != nil {
		return err
	}
	return s.mailer.Send(imip.SendInput{FromUser: organizer, To: external,
		Subject: "Cancelled: " + titleOf(ev), Body: "An occurrence of this event has been cancelled.", ICS: string(ics), Method: "CANCEL"})
}

// classify reports whether an address is an internal Holistic user at this instance's mail
// domain (and resolves the local account name). Empty mail domain ⇒ everyone is external.
func (s *Scheduler) classify(email string) (string, bool) {
	email = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(email, "mailto:")))
	at := strings.LastIndex(email, "@")
	if at <= 0 {
		return "", false
	}
	local, domain := email[:at], email[at+1:]
	if md := strings.ToLower(s.inst.MailDomain()); md == "" || domain != md {
		return "", false
	}
	if _, err := user.Lookup(local); err != nil {
		return "", false
	}
	return local, true
}

func (s *Scheduler) firstLocalRecipient(rcpts []string) string {
	for _, r := range rcpts {
		if u, internal := s.classify(r); internal {
			return u
		}
	}
	return ""
}

func setPartStat(ev *event.Event, email, partStat string) {
	for i := range ev.Attendees {
		if sameAddr(ev.Attendees[i].Email, email) {
			ev.Attendees[i].PartStat = partStat
			return
		}
	}
}

func partStatFor(ev *event.Event, email string) string {
	for _, a := range ev.Attendees {
		if sameAddr(a.Email, email) {
			return a.PartStat
		}
	}
	return ""
}

func attendeeMatches(ev *event.Event, addr string) bool {
	for _, a := range ev.Attendees {
		if sameAddr(a.Email, addr) {
			return true
		}
	}
	return false
}

func sameAddr(a, b string) bool {
	norm := func(s string) string {
		return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "mailto:")))
	}
	return norm(a) != "" && norm(a) == norm(b)
}

func titleOf(ev *event.Event) string {
	if ev.Summary == "" {
		return "(no title)"
	}
	return ev.Summary
}

func invitationBody(ev *event.Event) string {
	var b strings.Builder
	b.WriteString("You have been invited to: ")
	b.WriteString(titleOf(ev))
	if ev.Location != "" {
		b.WriteString("\nLocation: " + ev.Location)
	}
	b.WriteString("\nWhen: " + ev.Start.UTC().Format("2006-01-02 15:04 UTC"))
	return b.String()
}
