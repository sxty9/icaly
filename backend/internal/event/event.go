// Package event is icaly's pragmatic, canonical in-app representation of a calendar event
// and a calendar collection. It is the working shape the UI and JSON API speak; the
// internal/ical package maps it to/from RFC 5545 (+ 7986) VCALENDAR bytes, which are the
// on-disk single source of truth. A full JSCalendar (RFC 8984) model arrives with JMAP in
// Phase 3; until then this covers the Google/Outlook attribute set the requirements call for.
package event

import (
	"strings"
	"time"
)

// NormAddr canonicalizes a calendar-user address for identity comparison: it strips a leading
// "mailto:" scheme, trims surrounding whitespace and lower-cases. It is the single source of
// truth for address identity across the service (attendee matching, dedup, organizer authority);
// callers compare NormAddr(x) values rather than re-implementing the normalization inline.
func NormAddr(s string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "mailto:")))
}

// SameAddr reports whether two calendar-user addresses denote the same participant, ignoring
// case, surrounding whitespace and a "mailto:" scheme. The empty address matches nothing.
func SameAddr(a, b string) bool {
	na := NormAddr(a)
	return na != "" && na == NormAddr(b)
}

// Participant is an ORGANIZER or ATTENDEE. Email is the calendar-user address (without the
// "mailto:" scheme). IsInternal/Username are resolved at write time (a Holistic Linux user)
// and are index-only hints — never the source of truth.
type Participant struct {
	Email      string `json:"email"`
	Name       string `json:"name,omitempty"`     // CN
	Role       string `json:"role,omitempty"`     // CHAIR | REQ-PARTICIPANT | OPT-PARTICIPANT | NON-PARTICIPANT
	PartStat   string `json:"partStat,omitempty"` // NEEDS-ACTION | ACCEPTED | DECLINED | TENTATIVE
	RSVP       bool   `json:"rsvp,omitempty"`
	CUType     string `json:"cuType,omitempty"` // INDIVIDUAL | GROUP | ROOM | RESOURCE
	IsInternal bool   `json:"isInternal,omitempty"`
	Username   string `json:"username,omitempty"`
}

// Alarm is a reminder (VALARM). Trigger is an iCalendar duration relative to start, e.g. "-PT15M".
type Alarm struct {
	Action      string `json:"action"`  // DISPLAY | EMAIL
	Trigger     string `json:"trigger"` // e.g. -PT15M
	Description string `json:"description,omitempty"`
}

// GeoPoint is a geographic position for the event's location, mapped to the iCalendar GEO
// property (RFC 5545 §3.8.1.6: "GEO:lat;lon"). It is set when the location was chosen from the
// geocoding picker, alongside the free-text Location, and lets the UI offer a map link.
type GeoPoint struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// Event is one calendar object. Times are stored as absolute UTC instants (for timed events)
// or as date-anchored UTC midnights (for all-day); TimeZone keeps the originating IANA zone
// for display and a future VTIMEZONE upgrade (plan B2). End is exclusive.
type Event struct {
	UID          string        `json:"uid"`
	CalendarID   string        `json:"calendarId,omitempty"`
	Summary      string        `json:"summary"`
	Description  string        `json:"description,omitempty"`
	Location     string        `json:"location,omitempty"`
	Geo          *GeoPoint     `json:"geo,omitempty"`
	Start        time.Time     `json:"start"`
	End          time.Time     `json:"end"`
	AllDay       bool          `json:"allDay,omitempty"`
	TimeZone     string        `json:"timeZone,omitempty"`
	Status       string        `json:"status,omitempty"`       // CONFIRMED | TENTATIVE | CANCELLED
	Class        string        `json:"class,omitempty"`        // PUBLIC | PRIVATE | CONFIDENTIAL
	Transparency string        `json:"transparency,omitempty"` // OPAQUE | TRANSPARENT
	Priority     int           `json:"priority,omitempty"`
	Color        string        `json:"color,omitempty"`
	Categories   []string      `json:"categories,omitempty"`
	RRule        string        `json:"rrule,omitempty"`
	ExDates      []time.Time   `json:"exDates,omitempty"`
	RecurrenceID *time.Time    `json:"recurrenceId,omitempty"`
	Organizer    *Participant  `json:"organizer,omitempty"`
	Attendees    []Participant `json:"attendees,omitempty"`
	Alarms       []Alarm       `json:"alarms,omitempty"`
	Conference   string        `json:"conference,omitempty"`
	URL          string        `json:"url,omitempty"`
	Sequence     int           `json:"sequence,omitempty"`
	Created      time.Time     `json:"created,omitempty"`
	Updated      time.Time     `json:"updated,omitempty"`
}

// Duration is the event's length, with sensible fallbacks when End is unset.
func (e *Event) Duration() time.Duration {
	if !e.End.IsZero() && e.End.After(e.Start) {
		return e.End.Sub(e.Start)
	}
	if e.AllDay {
		return 24 * time.Hour
	}
	return time.Hour
}

// Clone returns a deep copy (used to materialise recurrence instances from a master).
func (e *Event) Clone() *Event {
	c := *e
	if e.Categories != nil {
		c.Categories = append([]string(nil), e.Categories...)
	}
	if e.ExDates != nil {
		c.ExDates = append([]time.Time(nil), e.ExDates...)
	}
	if e.Attendees != nil {
		c.Attendees = append([]Participant(nil), e.Attendees...)
	}
	if e.Alarms != nil {
		c.Alarms = append([]Alarm(nil), e.Alarms...)
	}
	if e.Organizer != nil {
		o := *e.Organizer
		c.Organizer = &o
	}
	if e.RecurrenceID != nil {
		t := *e.RecurrenceID
		c.RecurrenceID = &t
	}
	if e.Geo != nil {
		g := *e.Geo
		c.Geo = &g
	}
	return &c
}

// Calendar is a collection of events owned by one Holistic account.
type Calendar struct {
	ID          string `json:"id"`
	Owner       string `json:"owner"`
	Kind        string `json:"kind"` // personal | subscription | scheduling-inbox | scheduling-outbox
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Color       string `json:"color,omitempty"`
	TimeZone    string `json:"timeZone,omitempty"`
	Order       int    `json:"order"`
	ReadOnly    bool   `json:"readOnly"`
	Public      bool   `json:"public"`
	CTag        string `json:"ctag"`
	// FeedToken is the capability token for the read-only webcal/ICS feed of this calendar.
	// It is the credential for feeds/{token}.ics, so it is only ever returned to the owner.
	FeedToken string `json:"feedToken,omitempty"`
}
