// Package ical maps icaly's canonical event model to/from RFC 5545 (+ 7986) VCALENDAR
// bytes — the on-disk single source of truth. It wraps emersion/go-ical (codec) and
// teambition/rrule-go (recurrence expansion, via go-ical's RecurrenceSet).
//
// Times are emitted in UTC ("...Z"): unambiguous for every client and free of the
// VTIMEZONE-generation minefield (plan B2). The originating IANA zone is preserved on the
// model for display; switching to DTSTART;TZID + injected VTIMEZONE is a later upgrade.
package ical

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"time"

	goical "github.com/emersion/go-ical"

	"icaly/internal/event"
)

// ProdID identifies icaly as the generating product in emitted calendars.
const ProdID = "-//Holistic//icaly//EN"

const (
	utcLayout  = "20060102T150405Z"
	dateLayout = "20060102" // RFC 5545 DATE value (all-day)
)

var errNoEvent = errors.New("ical: no VEVENT found")

// Encode serialises one event to a complete VCALENDAR (METHOD-less; the on-disk canonical form).
func Encode(ev *event.Event) ([]byte, error) { return encode(ev, "") }

// EncodeWithMethod serialises one event to a VCALENDAR carrying an iTIP METHOD (RFC 5546:
// REQUEST/REPLY/CANCEL), as used for iMIP invitations.
func EncodeWithMethod(ev *event.Event, method string) ([]byte, error) { return encode(ev, method) }

func encode(ev *event.Event, method string) ([]byte, error) {
	cal := goical.NewCalendar()
	cal.Props.SetText(goical.PropVersion, "2.0")
	cal.Props.SetText(goical.PropProductID, ProdID)
	if method != "" {
		cal.Props.SetText(goical.PropMethod, strings.ToUpper(method))
	}
	cal.Children = append(cal.Children, toComponent(ev))
	var buf bytes.Buffer
	if err := goical.NewEncoder(&buf).Encode(cal); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Method returns the uppercased top-level METHOD of a VCALENDAR, or "" if absent.
func Method(b []byte) string {
	cal, err := goical.NewDecoder(bytes.NewReader(b)).Decode()
	if err != nil {
		return ""
	}
	if p := cal.Props.Get(goical.PropMethod); p != nil {
		return strings.ToUpper(strings.TrimSpace(p.Value))
	}
	return ""
}

// Decode parses the first VEVENT of a VCALENDAR back into the event model.
func Decode(b []byte) (*event.Event, error) {
	cal, err := goical.NewDecoder(bytes.NewReader(b)).Decode()
	if err != nil {
		return nil, err
	}
	for _, c := range cal.Children {
		if c.Name == goical.CompEvent {
			return fromComponent(c), nil
		}
	}
	return nil, errNoEvent
}

// DecodeAll parses every top-level VEVENT of a VCALENDAR into the event model (used by import).
// Recurrence-override components (those carrying RECURRENCE-ID) are skipped here: a single
// master per UID is created, matching the model-based store. Returns the events in file order.
func DecodeAll(b []byte) ([]*event.Event, error) {
	cal, err := goical.NewDecoder(bytes.NewReader(b)).Decode()
	if err != nil {
		return nil, err
	}
	var out []*event.Event
	for _, c := range cal.Children {
		if c.Name != goical.CompEvent {
			continue
		}
		if c.Props.Get(goical.PropRecurrenceID) != nil {
			continue // an override instance; folded into its master elsewhere
		}
		out = append(out, fromComponent(c))
	}
	return out, nil
}

// Feed aggregates per-event stored .ics bytes into one METHOD:PUBLISH VCALENDAR for webcal/ICS
// subscription. The stored VEVENTs are copied verbatim (master + any RECURRENCE-ID overrides);
// name sets X-WR-CALNAME and a 1h refresh hint is advertised to subscribers.
func Feed(name string, raws [][]byte) ([]byte, error) {
	out := goical.NewCalendar()
	out.Props.SetText(goical.PropVersion, "2.0")
	out.Props.SetText(goical.PropProductID, ProdID)
	out.Props.SetText(goical.PropMethod, "PUBLISH")
	if name != "" {
		setRaw(out.Props, "X-WR-CALNAME", name)
	}
	setRaw(out.Props, goical.PropRefreshInterval, "PT1H") // VALUE=DURATION is the registered default
	setRaw(out.Props, "X-PUBLISHED-TTL", "PT1H")
	for _, raw := range raws {
		c, err := goical.NewDecoder(bytes.NewReader(raw)).Decode()
		if err != nil {
			continue
		}
		for _, child := range c.Children {
			if child.Name == goical.CompEvent {
				out.Children = append(out.Children, child)
			}
		}
	}
	var buf bytes.Buffer
	if err := goical.NewEncoder(&buf).Encode(out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// FreeBusy builds a METHOD:PUBLISH VFREEBUSY for the window [start, end) with the given busy
// periods (each {from, to} in any zone, emitted in UTC). It backs the CalDAV free-busy-query
// REPORT; periods are FREEBUSY;FBTYPE=BUSY:<start>/<end> in iCalendar UTC basic form.
func FreeBusy(start, end time.Time, busy [][2]time.Time) ([]byte, error) {
	cal := goical.NewCalendar()
	cal.Props.SetText(goical.PropVersion, "2.0")
	cal.Props.SetText(goical.PropProductID, ProdID)
	cal.Props.SetText(goical.PropMethod, "PUBLISH")
	fb := goical.NewComponent(goical.CompFreeBusy)
	fb.Props.SetDateTime(goical.PropDateTimeStamp, time.Now().UTC())
	fb.Props.SetDateTime(goical.PropDateTimeStart, start.UTC())
	fb.Props.SetDateTime(goical.PropDateTimeEnd, end.UTC())
	for _, b := range busy {
		p := goical.NewProp(goical.PropFreeBusy)
		p.Params.Set("FBTYPE", "BUSY")
		p.Value = b[0].UTC().Format(utcLayout) + "/" + b[1].UTC().Format(utcLayout)
		fb.Props.Add(p)
	}
	cal.Children = append(cal.Children, fb)
	var buf bytes.Buffer
	if err := goical.NewEncoder(&buf).Encode(cal); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Occurrences returns the start instants of the first VEVENT that fall in [from, to)
// (inclusive of from), expanding RRULE/RDATE and honouring EXDATE.
func Occurrences(b []byte, from, to time.Time) ([]time.Time, error) {
	cal, err := goical.NewDecoder(bytes.NewReader(b)).Decode()
	if err != nil {
		return nil, err
	}
	for _, c := range cal.Children {
		if c.Name != goical.CompEvent {
			continue
		}
		set, err := c.RecurrenceSet(time.UTC)
		if err != nil {
			return nil, err
		}
		if set == nil { // non-recurring
			st, err := c.Props.DateTime(goical.PropDateTimeStart, time.UTC)
			if err != nil {
				return nil, err
			}
			st = st.UTC()
			if !st.Before(from) && st.Before(to) {
				return []time.Time{st}, nil
			}
			return nil, nil
		}
		occ := set.Between(from, to, true)
		for i := range occ {
			occ[i] = occ[i].UTC()
		}
		return occ, nil
	}
	return nil, errNoEvent
}

// ── recurring-series editing (scoped this / this-and-following / all) ─────────────────

// splitSeries decodes a VCALENDAR into its master VEVENT (the one without RECURRENCE-ID) and the
// RECURRENCE-ID override components (per-occurrence exceptions sharing the master's UID).
func splitSeries(b []byte) (*goical.Calendar, *goical.Component, []*goical.Component, error) {
	cal, err := goical.NewDecoder(bytes.NewReader(b)).Decode()
	if err != nil {
		return nil, nil, nil, err
	}
	var master *goical.Component
	var overrides []*goical.Component
	for _, c := range cal.Children {
		if c.Name != goical.CompEvent {
			continue
		}
		if c.Props.Get(goical.PropRecurrenceID) != nil {
			overrides = append(overrides, c)
		} else if master == nil {
			master = c
		}
	}
	if master == nil {
		return nil, nil, nil, errNoEvent
	}
	return cal, master, overrides, nil
}

// rebuild re-encodes the VCALENDAR from a master + overrides, preserving any non-event children
// (e.g. VTIMEZONE) and keeping the master first so a single-VEVENT decode still finds it.
func rebuild(cal *goical.Calendar, master *goical.Component, overrides []*goical.Component) ([]byte, error) {
	kids := make([]*goical.Component, 0, len(cal.Children)+1)
	for _, c := range cal.Children {
		if c.Name != goical.CompEvent {
			kids = append(kids, c)
		}
	}
	kids = append(kids, master)
	kids = append(kids, overrides...)
	cal.Children = kids
	var buf bytes.Buffer
	if err := goical.NewEncoder(&buf).Encode(cal); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func ridOf(c *goical.Component) (time.Time, bool) {
	if rp := c.Props.Get(goical.PropRecurrenceID); rp != nil {
		if t, err := rp.DateTime(time.UTC); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// ExpandSeries returns the occurrences of a recurring series in [from, to): RRULE/RDATE expansion
// honouring EXDATE, with per-occurrence RECURRENCE-ID overrides substituted in (an edited single
// instance shows its own data), plus any override moved into the window from outside it.
func ExpandSeries(b []byte, from, to time.Time) ([]*event.Event, error) {
	cal, master, overrides, err := splitSeries(b)
	if err != nil {
		return nil, err
	}
	_ = cal
	masterEv := fromComponent(master)
	dur := masterEv.Duration()

	ovByRID := make(map[int64]*event.Event, len(overrides))
	for _, ov := range overrides {
		if rid, ok := ridOf(ov); ok {
			ovByRID[rid.Unix()] = fromComponent(ov)
		}
	}

	set, err := master.RecurrenceSet(time.UTC)
	if err != nil {
		return nil, err
	}
	var out []*event.Event
	emitted := make(map[int64]bool)
	if set == nil { // not actually recurring
		if !masterEv.Start.Before(from) && masterEv.Start.Before(to) {
			out = append(out, masterEv)
		}
		return out, nil
	}
	for _, t := range set.Between(from, to, true) {
		t = t.UTC()
		emitted[t.Unix()] = true
		if ov, ok := ovByRID[t.Unix()]; ok {
			inst := ov.Clone()
			inst.RRule = masterEv.RRule // expose the series rule so the editor knows this is a series
			rid := t
			inst.RecurrenceID = &rid
			out = append(out, inst) // the override carries its own (possibly moved) Start/End
			continue
		}
		inst := masterEv.Clone()
		inst.Start, inst.End = t, t.Add(dur)
		// Keep RRule (the master's) so the UI recognises this as a series and shows the scope
		// chooser; clear only EXDATEs (an instance carries no exclusions of its own).
		inst.ExDates = nil
		rid := t
		inst.RecurrenceID = &rid
		out = append(out, inst)
	}
	// An override whose RECURRENCE-ID is outside the window but whose moved Start falls inside it
	// must still appear.
	for rid, ov := range ovByRID {
		if emitted[rid] {
			continue
		}
		if !ov.Start.Before(from) && ov.Start.Before(to) {
			inst := ov.Clone()
			inst.RRule = masterEv.RRule
			t := time.Unix(rid, 0).UTC()
			inst.RecurrenceID = &t
			out = append(out, inst)
		}
	}
	return out, nil
}

// Override writes a per-occurrence exception (the "this event only" edit): it replaces (or adds)
// the RECURRENCE-ID override for edited.RecurrenceID, leaving the master rule and other overrides
// intact. edited carries the new single-instance data.
func Override(b []byte, edited *event.Event) ([]byte, error) {
	if edited.RecurrenceID == nil {
		return nil, errors.New("ical: override needs a recurrence id")
	}
	cal, master, overrides, err := splitSeries(b)
	if err != nil {
		return nil, err
	}
	rid := edited.RecurrenceID.UTC()
	single := edited.Clone()
	single.RRule, single.ExDates = "", nil // an override is a single instance, never recurring
	comp := toComponent(single)

	kept := overrides[:0]
	for _, ov := range overrides {
		if t, ok := ridOf(ov); ok && t.Equal(rid) {
			continue // drop the old override for this occurrence
		}
		kept = append(kept, ov)
	}
	kept = append(kept, comp)
	return rebuild(cal, master, kept)
}

// Exclude removes one occurrence (the "this event only" delete): it adds an EXDATE to the master
// and drops any override for that instant.
func Exclude(b []byte, occ time.Time) ([]byte, error) {
	cal, master, overrides, err := splitSeries(b)
	if err != nil {
		return nil, err
	}
	occ = occ.UTC()
	// EXDATE's value type MUST match the master's DTSTART (RFC 5545 §3.8.5.1): a DATE-valued
	// (all-day) master needs a DATE EXDATE, a timed master a UTC DATE-TIME.
	allDay := masterIsDate(master)
	xp := goical.NewProp(goical.PropExceptionDates)
	if allDay {
		xp.SetValueType(goical.ValueDate)
		xp.Value = occ.Format(dateLayout)
	} else {
		xp.SetValueType(goical.ValueDateTime)
		xp.Value = occ.Format(utcLayout)
	}
	master.Props.Add(xp)

	kept := overrides[:0]
	for _, ov := range overrides {
		if t, ok := ridOf(ov); ok && t.Equal(occ) {
			continue
		}
		kept = append(kept, ov)
	}
	return rebuild(cal, master, kept)
}

// Truncate ends the series at `until` (the "this and following" delete, and the first half of a
// this-and-following edit): it sets the master RRULE's UNTIL and drops overrides after it.
func Truncate(b []byte, until time.Time) ([]byte, error) {
	cal, master, overrides, err := splitSeries(b)
	if err != nil {
		return nil, err
	}
	until = until.UTC()
	rp := master.Props.Get(goical.PropRecurrenceRule)
	if rp == nil {
		return nil, errors.New("ical: not a recurring event")
	}
	rp.Value = setRRuleUntil(rp.Value, until, masterIsDate(master))
	kept := overrides[:0]
	for _, ov := range overrides {
		if t, ok := ridOf(ov); ok && t.After(until) {
			continue
		}
		kept = append(kept, ov)
	}
	return rebuild(cal, master, kept)
}

// setRRuleUntil sets UNTIL on an RRULE value string, dropping COUNT (UNTIL and COUNT are mutually
// exclusive per RFC 5545). For an all-day series UNTIL is a DATE value, matching DTSTART.
func setRRuleUntil(rrule string, until time.Time, allDay bool) string {
	untilStr := until.Format(utcLayout)
	if allDay {
		untilStr = until.Format(dateLayout)
	}
	var out []string
	seen := false
	for _, p := range strings.Split(rrule, ";") {
		if p == "" {
			continue
		}
		key := strings.ToUpper(strings.SplitN(p, "=", 2)[0])
		switch key {
		case "COUNT":
			continue
		case "UNTIL":
			out = append(out, "UNTIL="+untilStr)
			seen = true
		default:
			out = append(out, p)
		}
	}
	if !seen {
		out = append(out, "UNTIL="+untilStr)
	}
	return strings.Join(out, ";")
}

// ReplaceMaster rewrites the series master from newMaster (which carries the preserved RRULE and
// EXDATEs) while keeping every RECURRENCE-ID override intact. The "all events" edit uses this so
// changing the series neither resurrects EXDATE-deleted occurrences nor reverts customised ones.
func ReplaceMaster(b []byte, newMaster *event.Event) ([]byte, error) {
	cal, _, overrides, err := splitSeries(b)
	if err != nil {
		return nil, err
	}
	return rebuild(cal, toComponent(newMaster), overrides)
}

// TailRule returns the RRULE a "this and following" split should give the new series so it yields
// exactly the occurrences from occ onward. A COUNT-based rule is rebased to the remaining count
// (otherwise the new series would restart the full count); UNTIL/open-ended rules carry over.
func TailRule(b []byte, occ time.Time) (string, error) {
	_, master, _, err := splitSeries(b)
	if err != nil {
		return "", err
	}
	rp := master.Props.Get(goical.PropRecurrenceRule)
	if rp == nil {
		return "", nil
	}
	rule := rp.Value
	origCount := -1
	for _, p := range strings.Split(rule, ";") {
		if kv := strings.SplitN(p, "=", 2); len(kv) == 2 && strings.EqualFold(kv[0], "COUNT") {
			origCount, _ = strconv.Atoi(strings.TrimSpace(kv[1]))
		}
	}
	if origCount < 0 {
		return rule, nil // no COUNT → UNTIL or open-ended, carries over unchanged
	}
	set, err := master.RecurrenceSet(time.UTC)
	if err != nil || set == nil {
		return rule, nil
	}
	start, err := master.Props.DateTime(goical.PropDateTimeStart, time.UTC)
	if err != nil {
		return rule, nil
	}
	before := len(set.Between(start.UTC(), occ.UTC().Add(-time.Second), true))
	remaining := origCount - before
	if remaining < 1 {
		remaining = 1
	}
	var out []string
	for _, p := range strings.Split(rule, ";") {
		if p == "" {
			continue
		}
		if strings.EqualFold(strings.SplitN(p, "=", 2)[0], "COUNT") {
			out = append(out, "COUNT="+strconv.Itoa(remaining))
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, ";"), nil
}

func masterIsDate(c *goical.Component) bool {
	if p := c.Props.Get(goical.PropDateTimeStart); p != nil {
		return p.ValueType() == goical.ValueDate
	}
	return false
}

// MasterStart returns the DTSTART of the master VEVENT (used to shift a whole series by a delta).
func MasterStart(b []byte) (time.Time, error) {
	_, master, _, err := splitSeries(b)
	if err != nil {
		return time.Time{}, err
	}
	t, err := master.Props.DateTime(goical.PropDateTimeStart, time.UTC)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// ── encode helpers ──────────────────────────────────────────────────────────────────

// setRaw sets a property by raw value, leaving the value type at its registered default so
// the encoder emits no spurious VALUE= param (correct for SEQUENCE, RRULE, URL, …).
func setRaw(props goical.Props, name, value string) {
	p := goical.NewProp(name)
	p.Value = value
	props.Set(p)
}

func toComponent(ev *event.Event) *goical.Component {
	e := goical.NewEvent()
	p := e.Props
	p.SetText(goical.PropUID, ev.UID)
	if ev.Summary != "" {
		p.SetText(goical.PropSummary, ev.Summary)
	}
	if ev.Description != "" {
		p.SetText(goical.PropDescription, ev.Description)
	}
	if ev.Location != "" {
		p.SetText(goical.PropLocation, ev.Location)
	}
	if ev.Geo != nil {
		// RFC 5545 §3.8.1.6: GEO:<lat>;<lon> (semicolon, latitude first). Default value type is
		// FLOAT, so setRaw emits no VALUE= param. FormatFloat(-1) keeps full precision, decimal.
		setRaw(p, goical.PropGeo,
			strconv.FormatFloat(ev.Geo.Lat, 'f', -1, 64)+";"+strconv.FormatFloat(ev.Geo.Lon, 'f', -1, 64))
	}

	if ev.AllDay {
		p.SetDate(goical.PropDateTimeStart, ev.Start.UTC())
		end := ev.End
		if end.IsZero() {
			end = ev.Start.Add(24 * time.Hour)
		}
		p.SetDate(goical.PropDateTimeEnd, end.UTC())
	} else {
		p.SetDateTime(goical.PropDateTimeStart, ev.Start.UTC())
		end := ev.End
		if end.IsZero() {
			end = ev.Start.Add(time.Hour)
		}
		p.SetDateTime(goical.PropDateTimeEnd, end.UTC())
	}

	stamp := ev.Updated
	if stamp.IsZero() {
		stamp = time.Now()
	}
	p.SetDateTime(goical.PropDateTimeStamp, stamp.UTC())
	if !ev.Created.IsZero() {
		p.SetDateTime(goical.PropCreated, ev.Created.UTC())
	}
	if !ev.Updated.IsZero() {
		p.SetDateTime(goical.PropLastModified, ev.Updated.UTC())
	}
	setRaw(p, goical.PropSequence, strconv.Itoa(ev.Sequence))

	if ev.Status != "" {
		p.SetText(goical.PropStatus, ev.Status)
	}
	if ev.Class != "" {
		p.SetText(goical.PropClass, ev.Class)
	}
	if ev.Transparency != "" {
		p.SetText(goical.PropTransparency, ev.Transparency)
	}
	if ev.Priority > 0 {
		setRaw(p, goical.PropPriority, strconv.Itoa(ev.Priority))
	}
	if ev.Color != "" {
		setRaw(p, goical.PropColor, ev.Color)
	}
	if len(ev.Categories) > 0 {
		cp := goical.NewProp(goical.PropCategories)
		cp.SetTextList(ev.Categories)
		p.Set(cp)
	}
	if ev.URL != "" {
		setRaw(p, goical.PropURL, ev.URL)
	}
	if ev.Conference != "" {
		setRaw(p, goical.PropConference, ev.Conference)
	}
	if ev.RRule != "" {
		setRaw(p, goical.PropRecurrenceRule, ev.RRule)
	}
	for _, ex := range ev.ExDates {
		xp := goical.NewProp(goical.PropExceptionDates)
		xp.SetValueType(goical.ValueDateTime)
		xp.Value = ex.UTC().Format(utcLayout)
		p.Add(xp)
	}
	if ev.RecurrenceID != nil {
		setRaw(p, goical.PropRecurrenceID, ev.RecurrenceID.UTC().Format(utcLayout))
	}
	if ev.Organizer != nil {
		addParticipant(p, goical.PropOrganizer, *ev.Organizer)
	}
	for _, a := range ev.Attendees {
		addParticipant(p, goical.PropAttendee, a)
	}

	for _, al := range ev.Alarms {
		va := goical.NewComponent(goical.CompAlarm)
		action := al.Action
		if action == "" {
			action = "DISPLAY"
		}
		va.Props.SetText(goical.PropAction, action)
		setRaw(va.Props, goical.PropTrigger, al.Trigger)
		desc := al.Description
		if desc == "" {
			desc = ev.Summary
		}
		va.Props.SetText(goical.PropDescription, desc)
		e.Children = append(e.Children, va)
	}
	return e.Component
}

func addParticipant(props goical.Props, name string, a event.Participant) {
	if a.Email == "" {
		return
	}
	pr := goical.NewProp(name)
	pr.Value = "mailto:" + a.Email
	if a.Name != "" {
		pr.Params.Set(goical.ParamCommonName, a.Name)
	}
	if a.Role != "" {
		pr.Params.Set(goical.ParamRole, a.Role)
	}
	if a.PartStat != "" {
		pr.Params.Set(goical.ParamParticipationStatus, a.PartStat)
	}
	if a.CUType != "" {
		pr.Params.Set(goical.ParamCalendarUserType, a.CUType)
	}
	if a.RSVP {
		pr.Params.Set(goical.ParamRSVP, "TRUE")
	}
	props.Add(pr)
}

// ── decode helpers ──────────────────────────────────────────────────────────────────

func fromComponent(c *goical.Component) *event.Event {
	ev := &event.Event{
		UID:          text(c, goical.PropUID),
		Summary:      text(c, goical.PropSummary),
		Description:  text(c, goical.PropDescription),
		Location:     text(c, goical.PropLocation),
		Status:       text(c, goical.PropStatus),
		Class:        text(c, goical.PropClass),
		Transparency: text(c, goical.PropTransparency),
		Color:        raw(c, goical.PropColor),
		URL:          raw(c, goical.PropURL),
		Conference:   raw(c, goical.PropConference),
		RRule:        raw(c, goical.PropRecurrenceRule),
	}
	if pr := c.Props.Get(goical.PropPriority); pr != nil {
		ev.Priority, _ = strconv.Atoi(strings.TrimSpace(pr.Value))
	}
	if pr := c.Props.Get(goical.PropSequence); pr != nil {
		ev.Sequence, _ = strconv.Atoi(strings.TrimSpace(pr.Value))
	}
	if pr := c.Props.Get(goical.PropCategories); pr != nil {
		if l, err := pr.TextList(); err == nil {
			ev.Categories = l
		}
	}

	if sp := c.Props.Get(goical.PropDateTimeStart); sp != nil {
		ev.AllDay = sp.ValueType() == goical.ValueDate
		if t, err := sp.DateTime(time.UTC); err == nil {
			ev.Start = t.UTC()
		}
		if tz := sp.Params.Get(goical.ParamTimezoneID); tz != "" {
			ev.TimeZone = tz
		}
	}
	if ep := c.Props.Get(goical.PropDateTimeEnd); ep != nil {
		if t, err := ep.DateTime(time.UTC); err == nil {
			ev.End = t.UTC()
		}
	}
	if t, err := c.Props.DateTime(goical.PropCreated, time.UTC); err == nil {
		ev.Created = t.UTC()
	}
	if t, err := c.Props.DateTime(goical.PropLastModified, time.UTC); err == nil {
		ev.Updated = t.UTC()
	}

	for _, xp := range c.Props[goical.PropExceptionDates] {
		if t, err := xp.DateTime(time.UTC); err == nil {
			ev.ExDates = append(ev.ExDates, t.UTC())
		}
	}
	if rp := c.Props.Get(goical.PropRecurrenceID); rp != nil {
		if t, err := rp.DateTime(time.UTC); err == nil {
			tt := t.UTC()
			ev.RecurrenceID = &tt
		}
	}
	// GEO is two semicolon-separated floats; go-ical's Prop.Float() can't parse that, so split it
	// by hand (a comma here would be a WGS84/geo-URI mistake, not iCalendar).
	if gp := c.Props.Get(goical.PropGeo); gp != nil {
		if parts := strings.SplitN(gp.Value, ";", 2); len(parts) == 2 {
			lat, e1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
			lon, e2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
			if e1 == nil && e2 == nil {
				ev.Geo = &event.GeoPoint{Lat: lat, Lon: lon}
			}
		}
	}
	if op := c.Props.Get(goical.PropOrganizer); op != nil {
		o := participant(op)
		ev.Organizer = &o
	}
	for i := range c.Props[goical.PropAttendee] {
		ev.Attendees = append(ev.Attendees, participant(&c.Props[goical.PropAttendee][i]))
	}
	for _, child := range c.Children {
		if child.Name == goical.CompAlarm {
			ev.Alarms = append(ev.Alarms, event.Alarm{
				Action:      text(child, goical.PropAction),
				Trigger:     raw(child, goical.PropTrigger),
				Description: text(child, goical.PropDescription),
			})
		}
	}
	return ev
}

func participant(pr *goical.Prop) event.Participant {
	v := pr.Value
	if len(v) >= 7 && strings.EqualFold(v[:7], "mailto:") {
		v = v[7:]
	}
	return event.Participant{
		Email:    v,
		Name:     pr.Params.Get(goical.ParamCommonName),
		Role:     pr.Params.Get(goical.ParamRole),
		PartStat: pr.Params.Get(goical.ParamParticipationStatus),
		CUType:   pr.Params.Get(goical.ParamCalendarUserType),
		RSVP:     strings.EqualFold(pr.Params.Get(goical.ParamRSVP), "TRUE"),
	}
}

func text(c *goical.Component, name string) string {
	if pr := c.Props.Get(name); pr != nil {
		if s, err := pr.Text(); err == nil {
			return s
		}
		return pr.Value
	}
	return ""
}

func raw(c *goical.Component, name string) string {
	if pr := c.Props.Get(name); pr != nil {
		return pr.Value
	}
	return ""
}
