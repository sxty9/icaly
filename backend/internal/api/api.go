// Package api serves icaly's HTTP surface under /api/services/icaly/, behind the shared
// holistic session. Phase 1a exposes the in-app calendar: list calendars, range-query and
// CRUD events, all scoped to the caller's own (and, later, shared) calendars and gated by
// the hp_icaly_* rights. Error bodies follow holistic's contract: {"detail": "..."}.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"icaly/internal/apppass"
	"icaly/internal/auth"
	"icaly/internal/caldav"
	"icaly/internal/event"
	"icaly/internal/geocode"
	"icaly/internal/ical"
	"icaly/internal/instance"
	"icaly/internal/push"
	"icaly/internal/rights"
	"icaly/internal/scheduling"
	"icaly/internal/store"
)

// mailSendRight is the mail service's send right; external invitations require it AND
// hp_icaly_invite (plan M6) so a user stripped of mail-send cannot regain reach via calendar.
const mailSendRight = "hp_mail_send"

const (
	base    = "/api/services/icaly/"
	service = "icaly"
	version = "0.1.0"

	maxBody   = 1 << 20 // 1 MiB JSON payload
	maxImport = 8 << 20 // 8 MiB .ics import payload

	maxAttendees = 500 // cap per event: bounds invite fan-out / mail amplification
)

// Server wires the verifier, store, instance resolver, live hub, scheduler, app-password store
// and CalDAV handler into HTTP handlers.
type Server struct {
	v             *auth.Verifier
	st            *store.Store
	inst          *instance.Resolver
	hub           *push.Hub
	sched         *scheduling.Scheduler
	ap            *apppass.Store   // app passwords for native CalDAV clients (HTTP Basic)
	dav           *caldav.Handler  // CalDAV / WebDAV-Sync surface under dav/
	geo           *geocode.Service // server-side place-search proxy for the location picker
	geoRate       *rateLimiter     // per-user cap on geocode lookups (abuse / cost guard)
	thr           *throttle        // per-IP auth-failure backoff for the DAV brute-force surface (M4)
	inboundSecret string           // shared icaly↔maild secret guarding POST imip/inbound
}

// New builds a server. ap may be nil (CalDAV app-password auth then unavailable); geo may be nil
// (the location picker then degrades to free text). inboundSecret guards the machine-to-machine
// iMIP inbound webhook; "" disables it (so a misconfigured deploy fails closed rather than open).
func New(v *auth.Verifier, st *store.Store, inst *instance.Resolver, hub *push.Hub, sched *scheduling.Scheduler, ap *apppass.Store, geo *geocode.Service, inboundSecret string) *Server {
	return &Server{
		v: v, st: st, inst: inst, hub: hub, sched: sched, ap: ap, geo: geo,
		dav:           caldav.New(st, base+"dav/", inst.Address),
		geoRate:       newRateLimiter(10*time.Second, 30), // 30 lookups / 10s / user (UI debounces too)
		thr:           newThrottle(),
		inboundSecret: inboundSecret,
	}
}

type handler func(w http.ResponseWriter, r *http.Request, u *auth.User)

// Handler returns the routed http.Handler (Go 1.22 method+path patterns).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+base+"health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET "+base+"info", s.guard("", false, s.info))

	mux.HandleFunc("GET "+base+"calendars", s.guard(rights.GroupView, false, s.calendars))
	mux.HandleFunc("POST "+base+"calendars", s.guard(rights.GroupShare, true, s.createCalendar))

	mux.HandleFunc("GET "+base+"events", s.guard(rights.GroupView, false, s.listEvents))
	mux.HandleFunc("GET "+base+"event", s.guard(rights.GroupView, false, s.getEvent))
	mux.HandleFunc("POST "+base+"events", s.guard(rights.GroupEdit, true, s.putEvent))
	mux.HandleFunc("POST "+base+"events/delete", s.guard(rights.GroupEdit, true, s.deleteEvent))
	mux.HandleFunc("POST "+base+"events/rsvp", s.guard(rights.GroupView, true, s.rsvp))
	mux.HandleFunc("GET "+base+"freebusy", s.guard(rights.GroupView, false, s.freebusy))

	// Inbound iMIP webhook from maild — shared-secret auth, never a user session (plan §4d).
	mux.HandleFunc("POST "+base+"imip/inbound", s.imipInbound)

	// Import/export: authored .ics in, calendar .ics out.
	mux.HandleFunc("POST "+base+"events/import", s.guard(rights.GroupEdit, true, s.importEvents))
	mux.HandleFunc("GET "+base+"events/export", s.guard(rights.GroupView, false, s.exportEvents))

	// Live in-app stream: cookie-auth GET, deliberately NOT CSRF-gated (EventSource cannot set
	// headers) and never behind app-pass Basic (plan m7).
	mux.HandleFunc("GET "+base+"events/stream", s.guard(rights.GroupView, false, s.stream))

	// Read-only webcal/ICS feed: the capability token IS the credential, so no session guard.
	mux.HandleFunc("GET "+base+"feeds/{file}", s.feed)

	// Location picker: server-side geocoding proxy (keeps any API key off the client). GETs, so
	// no CSRF; per-user rate-limited inside the handlers.
	mux.HandleFunc("GET "+base+"geocode", s.guard(rights.GroupView, false, s.geocodeSuggest))
	mux.HandleFunc("GET "+base+"geocode/resolve", s.guard(rights.GroupView, false, s.geocodeResolve))

	// App passwords for native CalDAV clients (managed from the browser session; the share
	// right covers app passwords and public feeds, per the rights manifest).
	mux.HandleFunc("GET "+base+"apppasswords", s.guard(rights.GroupShare, false, s.listAppPasswords))
	mux.HandleFunc("POST "+base+"apppasswords", s.guard(rights.GroupShare, true, s.createAppPassword))
	mux.HandleFunc("POST "+base+"apppasswords/delete", s.guard(rights.GroupShare, true, s.deleteAppPassword))

	// CalDAV + WebDAV-Sync subtree. Registered without a method so the WebDAV verbs (PROPFIND,
	// REPORT, MKCALENDAR, PROPPATCH, …) all route here; guardDav authenticates each request via
	// the session cookie OR an app-password Basic credential, behind the failure throttle (M4).
	mux.HandleFunc(base+"dav/", s.davHandler)
	return mux
}

// guard authenticates, optionally requires a fine-grained right, and optionally enforces CSRF.
func (s *Server) guard(perm string, csrf bool, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.v.User(r)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "Not authenticated")
			return
		}
		if perm != "" && !u.Can(perm) {
			writeErr(w, http.StatusForbidden, "You do not have permission for this action")
			return
		}
		if csrf && !s.v.CheckCSRF(r) {
			writeErr(w, http.StatusForbidden, "CSRF check failed")
			return
		}
		h(w, r, u)
	}
}

func (s *Server) info(w http.ResponseWriter, _ *http.Request, u *auth.User) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service":    service,
		"version":    version,
		"user":       u.Username,
		"isAdmin":    u.IsAdmin,
		"address":    s.inst.Address(u.Username),
		"mailDomain": s.inst.MailDomain(),
	})
}

func (s *Server) calendars(w http.ResponseWriter, _ *http.Request, u *auth.User) {
	cals, err := s.st.Calendars(u.Username)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not open calendars")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"address":    s.inst.Address(u.Username),
		"mailDomain": s.inst.MailDomain(),
		"calendars":  cals,
	})
}

func (s *Server) createCalendar(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var req struct {
		Name     string `json:"name"`
		Color    string `json:"color"`
		TimeZone string `json:"timeZone"`
	}
	if !decodeBody(w, r, &req) || strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "A calendar name is required")
		return
	}
	cal, err := s.st.CreateCalendar(u.Username, strings.TrimSpace(req.Name), req.Color, req.TimeZone)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not create calendar")
		return
	}
	writeJSON(w, http.StatusOK, cal)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request, u *auth.User) {
	calID := calendarParam(r)
	from := parseTime(r.URL.Query().Get("start"), time.Now().AddDate(0, -1, 0))
	to := parseTime(r.URL.Query().Get("end"), time.Now().AddDate(0, 3, 0))
	evs, err := s.st.ListEvents(u.Username, calID, from, to)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "Unknown calendar")
			return
		}
		writeErr(w, http.StatusInternalServerError, "Could not list events")
		return
	}
	if evs == nil {
		evs = []*event.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"calendar": calID, "events": evs})
}

func (s *Server) getEvent(w http.ResponseWriter, r *http.Request, u *auth.User) {
	calID := calendarParam(r)
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	if uid == "" {
		writeErr(w, http.StatusBadRequest, "An event uid is required")
		return
	}
	ev, etag, err := s.st.GetEvent(u.Username, calID, uid)
	if err != nil {
		writeErr(w, http.StatusNotFound, "Event not found")
		return
	}
	w.Header().Set("ETag", etag)
	writeJSON(w, http.StatusOK, ev)
}

// putEvent creates or updates an event. For a recurring series edited from one occurrence, the
// payload carries editScope: "this" (write a RECURRENCE-ID override), "following" (truncate the
// master before this occurrence + start a new series here), or "all"/"" (update the master,
// shifting the whole series by the same time delta the user applied to the occurrence).
func (s *Server) putEvent(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var req struct {
		event.Event
		EditScope string `json:"editScope"`
	}
	if !decodeBody(w, r, &req) {
		writeErr(w, http.StatusBadRequest, "Invalid event")
		return
	}
	ev := req.Event
	calID := ev.CalendarID
	if calID == "" {
		calID = calendarParam(r)
	}
	ev.CalendarID = calID
	if ev.Start.IsZero() {
		writeErr(w, http.StatusBadRequest, "Event start is required")
		return
	}
	ev.Attendees = dedupAttendees(ev.Attendees) // one invite per address, no duplicates
	if len(ev.Attendees) > maxAttendees {
		writeErr(w, http.StatusBadRequest, "Too many attendees")
		return
	}
	scope := strings.ToLower(strings.TrimSpace(req.EditScope))
	caller := s.inst.Address(u.Username)

	// Organizer authority is taken from the STORED event, never the client payload (which could
	// spoof ORGANIZER). A new event, or one this caller organizes, lets them drive invitations;
	// an event delivered to them as an attendee (stored organizer is someone else) must NOT trigger
	// organizer scheduling that mutates other users' calendars.
	prev, _, _ := s.st.GetEvent(u.Username, calID, ev.UID)
	isOrganizer := prev == nil || prev.Organizer == nil || prev.Organizer.Email == "" || sameAddr(prev.Organizer.Email, caller)
	if isOrganizer {
		ev.Organizer = &event.Participant{Email: caller, Username: u.Username, IsInternal: true} // never trust a client organizer
	} else {
		ev.Organizer = prev.Organizer // keep the real organizer; this caller only edits their own copy
	}
	mergePartStats(&ev, prev) // preserve attendees' collected RSVPs across an organizer edit

	canExternal := u.Can(rights.GroupInvite) && u.Can(mailSendRight)
	dispatch := func(e *event.Event) any {
		if !isOrganizer || s.sched == nil || len(e.Attendees) == 0 {
			return nil
		}
		if out, err := s.sched.OnOrganizerSave(u.Username, e, canExternal); err == nil {
			return out
		}
		return nil
	}

	// Scoped edits of a recurring occurrence (the payload is the clicked instance: it carries the
	// master's UID, its RRULE and the occurrence's recurrenceId).
	if ev.RecurrenceID != nil && (scope == "this" || scope == "following") {
		if scope == "this" {
			if err := s.st.OverrideOccurrence(u.Username, calID, ev.UID, &ev); err != nil {
				writeErr(w, http.StatusInternalServerError, "Could not update this occurrence")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"uid": ev.UID, "calendar": calID, "scope": "this", "scheduling": dispatch(&ev)})
			return
		}
		// following: end the old series here, then create a fresh series from this occurrence,
		// rebasing a COUNT-based rule to the remaining count so it doesn't restart the full count.
		occ := *ev.RecurrenceID
		tail, _ := s.st.SeriesTailRule(u.Username, calID, ev.UID, occ)
		if err := s.st.TruncateSeries(u.Username, calID, ev.UID, occ); err != nil {
			writeErr(w, http.StatusInternalServerError, "Could not split the series")
			return
		}
		// Attendees must also have their copy of the OLD series truncated here, or they end up
		// double-booked (old tail + the new series). Do this before delivering the new series.
		if isOrganizer && s.sched != nil && len(ev.Attendees) > 0 {
			_ = s.sched.OnOccurrenceCancel(u.Username, &ev, occ, true, canExternal)
		}
		ns := ev.Clone()
		ns.UID = ""           // a brand-new series gets its own UID
		ns.RecurrenceID = nil // it is a master, not an override
		if tail != "" {
			ns.RRule = tail
		}
		if _, err := s.st.PutEvent(u.Username, calID, ns); err != nil {
			writeErr(w, http.StatusInternalServerError, "Could not create the new series")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"uid": ns.UID, "calendar": calID, "scope": "following", "scheduling": dispatch(ns)})
		return
	}

	// "all" on a recurring occurrence: rewrite the master (shifted by the same delta the user
	// applied to the occurrence) while preserving the series' EXDATEs and RECURRENCE-ID overrides.
	if ev.RecurrenceID != nil {
		if prev == nil {
			writeErr(w, http.StatusNotFound, "Unknown event")
			return
		}
		delta := ev.Start.Sub(*ev.RecurrenceID)
		dur := ev.End.Sub(ev.Start)
		ev.Start = prev.Start.Add(delta)
		if dur > 0 {
			ev.End = ev.Start.Add(dur)
		}
		ev.RecurrenceID = nil
		ev.ExDates = prev.ExDates // keep the series' existing exclusions
		ev.Created = prev.Created
		ev.Sequence = prev.Sequence + 1
		if err := s.st.ReplaceMaster(u.Username, calID, ev.UID, &ev); err != nil {
			writeErr(w, http.StatusInternalServerError, "Could not update the series")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"uid": ev.UID, "calendar": calID, "scope": "all", "scheduling": dispatch(&ev)})
		return
	}

	// Non-recurring event (or brand-new): a plain create/update.
	etag, err := s.st.PutEvent(u.Username, calID, &ev)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "Unknown calendar")
			return
		}
		writeErr(w, http.StatusInternalServerError, "Could not save event")
		return
	}
	w.Header().Set("ETag", etag)
	writeJSON(w, http.StatusOK, map[string]any{
		"uid": ev.UID, "calendar": calID, "etag": etag, "sequence": ev.Sequence, "scheduling": dispatch(&ev),
	})
}

// deleteEvent removes an event or, for a recurring series, a scoped slice of it: editScope "this"
// (EXDATE this occurrence), "following" (truncate the series here), or "all"/"" (delete the whole
// series). recurrenceId identifies the occurrence for the this/following scopes.
func (s *Server) deleteEvent(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var req struct {
		Calendar     string     `json:"calendar"`
		UID          string     `json:"uid"`
		EditScope    string     `json:"editScope"`
		RecurrenceID *time.Time `json:"recurrenceId"`
	}
	if !decodeBody(w, r, &req) || strings.TrimSpace(req.UID) == "" {
		writeErr(w, http.StatusBadRequest, "An event uid is required")
		return
	}
	calID := req.Calendar
	if calID == "" {
		calID = "personal"
	}
	uid := strings.TrimSpace(req.UID)
	scope := strings.ToLower(strings.TrimSpace(req.EditScope))
	canExternal := u.Can(rights.GroupInvite) && u.Can(mailSendRight)
	caller := s.inst.Address(u.Username)

	// Capture the stored event first — both to notify attendees and to decide authority. Only the
	// organizer may cancel for everyone; an attendee's delete just removes their own copy (it must
	// never reach into other users' calendars). Authority comes from the stored ORGANIZER.
	prev, _, _ := s.st.GetEvent(u.Username, calID, uid)
	isOrganizer := prev == nil || prev.Organizer == nil || prev.Organizer.Email == "" || sameAddr(prev.Organizer.Email, caller)

	if req.RecurrenceID != nil && (scope == "this" || scope == "following") {
		following := scope == "following"
		var err error
		if following {
			err = s.st.TruncateSeries(u.Username, calID, uid, *req.RecurrenceID)
		} else {
			err = s.st.ExcludeOccurrence(u.Username, calID, uid, *req.RecurrenceID)
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "Could not delete the occurrence(s)")
			return
		}
		if isOrganizer && s.sched != nil && prev != nil && len(prev.Attendees) > 0 {
			_ = s.sched.OnOccurrenceCancel(u.Username, prev, *req.RecurrenceID, following, canExternal)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "scope": scope})
		return
	}

	// Whole series (or a non-recurring event).
	if err := s.st.DeleteEvent(u.Username, calID, uid); err != nil {
		writeErr(w, http.StatusNotFound, "Event not found")
		return
	}
	if isOrganizer && s.sched != nil && prev != nil && len(prev.Attendees) > 0 {
		_ = s.sched.OnOrganizerCancel(u.Username, prev, canExternal)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// freebusy returns the busy intervals in [start,end) for a calendar: opaque (non-TRANSPARENT,
// non-CANCELLED) event spans, sorted and merged. Recurring events are expanded by the store.
func (s *Server) freebusy(w http.ResponseWriter, r *http.Request, u *auth.User) {
	calID := calendarParam(r)
	from := parseTime(r.URL.Query().Get("start"), time.Now().AddDate(0, 0, -1))
	to := parseTime(r.URL.Query().Get("end"), time.Now().AddDate(0, 1, 0))
	evs, err := s.st.ListEvents(u.Username, calID, from, to)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "Unknown calendar")
			return
		}
		writeErr(w, http.StatusInternalServerError, "Could not compute free/busy")
		return
	}
	type slot struct {
		Start time.Time `json:"start"`
		End   time.Time `json:"end"`
	}
	var slots []slot
	for _, ev := range evs {
		if strings.EqualFold(ev.Transparency, "TRANSPARENT") || strings.EqualFold(ev.Status, "CANCELLED") {
			continue
		}
		end := ev.End
		if end.IsZero() {
			end = ev.Start.Add(ev.Duration())
		}
		slots = append(slots, slot{Start: ev.Start.UTC(), End: end.UTC()})
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].Start.Before(slots[j].Start) })
	merged := slots[:0]
	for _, sl := range slots {
		if n := len(merged); n > 0 && !sl.Start.After(merged[n-1].End) {
			if sl.End.After(merged[n-1].End) {
				merged[n-1].End = sl.End
			}
			continue
		}
		merged = append(merged, sl)
	}
	if merged == nil {
		merged = []slot{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"calendar": calID, "busy": merged})
}

// importEvents ingests iCalendar data, creating/replacing each master VEVENT. The body is
// either a raw text/calendar payload (native clients) or JSON {"ics": "..."} (the dashboard,
// which posts through the CSRF-aware JSON client).
func (s *Server) importEvents(w http.ResponseWriter, r *http.Request, u *auth.User) {
	calID := calendarParam(r)
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxImport))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "Import payload too large")
		return
	}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		var req struct {
			Ics string `json:"ics"`
		}
		if json.Unmarshal(body, &req) != nil || req.Ics == "" {
			writeErr(w, http.StatusBadRequest, "Invalid import payload")
			return
		}
		body = []byte(req.Ics)
	}
	evs, err := ical.DecodeAll(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid iCalendar data")
		return
	}
	imported, skipped := 0, 0
	for _, ev := range evs {
		if ev.Start.IsZero() {
			skipped++
			continue
		}
		ev.CalendarID = calID
		if _, err := s.st.PutEvent(u.Username, calID, ev); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeErr(w, http.StatusNotFound, "Unknown calendar")
				return
			}
			skipped++
			continue
		}
		imported++
	}
	writeJSON(w, http.StatusOK, map[string]int{"imported": imported, "skipped": skipped})
}

// exportEvents streams the whole calendar as a single text/calendar download (byte-faithful,
// from the stored .ics files).
func (s *Server) exportEvents(w http.ResponseWriter, r *http.Request, u *auth.User) {
	calID := calendarParam(r)
	cal, ok := s.calendarOf(u.Username, calID)
	if !ok {
		writeErr(w, http.StatusNotFound, "Unknown calendar")
		return
	}
	raws, err := s.st.RawEvents(u.Username, calID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not read calendar")
		return
	}
	body, err := ical.Feed(cal.Name, raws)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not build export")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/calendar; charset=utf-8")
	h.Set("Content-Disposition", `attachment; filename="`+cal.ID+`.ics"`)
	_, _ = w.Write(body)
}

// feed serves a calendar's read-only webcal/ICS subscription. The {file} path is "<token>.ics";
// the token is the sole credential. ETag is the ctag, so If-None-Match yields cheap 304s.
func (s *Server) feed(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	if !strings.HasSuffix(file, ".ics") {
		writeErr(w, http.StatusNotFound, "Not found")
		return
	}
	cal, err := s.st.CalendarByFeedToken(strings.TrimSuffix(file, ".ics"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "Not found")
		return
	}
	etag := `"feed-` + cal.CTag + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=300")
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="`+cal.ID+`.ics"`)
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	raws, err := s.st.RawEvents(cal.Owner, cal.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not read feed")
		return
	}
	body, err := ical.Feed(cal.Name, raws)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not build feed")
		return
	}
	_, _ = w.Write(body)
}

// stream is the in-app live channel (Server-Sent Events). It emits a "hello" frame with the
// current change-log position, replays anything missed since Last-Event-ID, then forwards
// every committed change for the user. Heartbeat comments keep proxies from idling it out.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, u *auth.User) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "Streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ch, cancel := s.hub.Subscribe(u.Username)
	defer cancel()

	maxSeq, _ := s.st.MaxSeq(u.Username)
	writeSSE(w, fl, maxSeq, "hello", map[string]int64{"seq": maxSeq})
	if last := lastEventID(r); last > 0 {
		if missed, err := s.st.ChangesSince(u.Username, last); err == nil {
			for _, c := range missed {
				writeSSE(w, fl, c.Seq, "changed", c)
			}
		}
	}

	ctx := r.Context()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case c, ok := <-ch:
			if !ok {
				return
			}
			writeSSE(w, fl, c.Seq, "changed", c)
		case <-ping.C:
			_, _ = io.WriteString(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

// rsvp records the caller's participation status on an event and propagates it to the organizer
// (in-process if internal, by iTIP REPLY mail if external).
func (s *Server) rsvp(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var req struct {
		Calendar string `json:"calendar"`
		UID      string `json:"uid"`
		PartStat string `json:"partStat"`
	}
	if !decodeBody(w, r, &req) || strings.TrimSpace(req.UID) == "" || strings.TrimSpace(req.PartStat) == "" {
		writeErr(w, http.StatusBadRequest, "uid and partStat are required")
		return
	}
	calID := req.Calendar
	if calID == "" {
		calID = "personal"
	}
	if s.sched == nil {
		writeErr(w, http.StatusServiceUnavailable, "Scheduling not available")
		return
	}
	if err := s.sched.OnAttendeeReply(u.Username, calID, strings.TrimSpace(req.UID), strings.ToUpper(strings.TrimSpace(req.PartStat))); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "Event not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "Could not record RSVP")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// imipInbound is the machine-to-machine webhook maild calls for every inbound message carrying a
// text/calendar part. Authenticated solely by the shared secret (constant-time), never a session.
func (s *Server) imipInbound(w http.ResponseWriter, r *http.Request) {
	if s.inboundSecret == "" {
		writeErr(w, http.StatusServiceUnavailable, "Inbound iMIP not configured")
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Cal-Inbound-Secret")), []byte(s.inboundSecret)) != 1 {
		writeErr(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxImport))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "Message too large")
		return
	}
	from := r.Header.Get("X-Mail-From")
	rcpts := splitAddrs(r.Header.Get("X-Mail-Rcpt"))
	if s.sched == nil {
		writeErr(w, http.StatusServiceUnavailable, "Scheduling not available")
		return
	}
	if err := s.sched.OnInbound(from, rcpts, raw); err != nil {
		if errors.Is(err, scheduling.ErrNotAttendee) {
			writeErr(w, http.StatusForbidden, "Reply sender is not an attendee")
			return
		}
		writeErr(w, http.StatusUnprocessableEntity, "Could not process calendar message")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ── CalDAV (Phase 2) ────────────────────────────────────────────────────────────────

// davHandler authenticates the request (session OR app-password Basic, behind the per-IP
// throttle) and hands it to the CalDAV handler. Read needs hp_icaly_view; the mutating verbs
// additionally need hp_icaly_edit, enforced inside caldav.Serve via canEdit.
func (s *Server) davHandler(w http.ResponseWriter, r *http.Request) {
	u := s.authDav(w, r)
	if u == nil {
		return
	}
	if !u.Can(rights.GroupView) {
		writeErr(w, http.StatusForbidden, "You do not have permission to use calendars")
		return
	}
	s.dav.Serve(w, r, u.Username, u.IsAdmin, u.Can(rights.GroupEdit))
}

// authDav resolves the DAV principal from the holistic session cookie or, for native clients, an
// app-password HTTP Basic credential. It writes the 401/429 response itself and returns nil on
// failure. A rejected Basic credential counts toward the per-IP backoff; a bare unauthenticated
// probe (the normal 401-challenge step) does not.
func (s *Server) authDav(w http.ResponseWriter, r *http.Request) *auth.User {
	key := clientIP(r)
	if s.thr.blocked(key) {
		w.Header().Set("Retry-After", "60")
		writeErr(w, http.StatusTooManyRequests, "Too many authentication attempts")
		return nil
	}
	if u, err := s.v.User(r); err == nil { // browser session
		s.thr.ok(key)
		return u
	}
	if _, _, hasBasic := r.BasicAuth(); hasBasic && s.ap != nil {
		if u, err := s.v.AppUser(r, s.ap); err == nil {
			s.thr.ok(key)
			return u
		}
		s.thr.fail(key) // credentials supplied but rejected → brute-force signal
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="icaly calendar"`)
	writeErr(w, http.StatusUnauthorized, "Not authenticated")
	return nil
}

// ── app passwords ─────────────────────────────────────────────────────────────────────

func (s *Server) listAppPasswords(w http.ResponseWriter, _ *http.Request, u *auth.User) {
	if s.ap == nil {
		writeErr(w, http.StatusServiceUnavailable, "App passwords not available")
		return
	}
	list, err := s.ap.List(u.Username)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not read app passwords")
		return
	}
	if list == nil {
		list = []apppass.Meta{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"passwords": list})
}

func (s *Server) createAppPassword(w http.ResponseWriter, r *http.Request, u *auth.User) {
	if s.ap == nil {
		writeErr(w, http.StatusServiceUnavailable, "App passwords not available")
		return
	}
	var req struct {
		Label string `json:"label"`
	}
	_ = decodeBody(w, r, &req)
	token, meta, err := s.ap.Create(u.Username, strings.TrimSpace(req.Label))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not create app password")
		return
	}
	// The clear-text token is returned exactly once; only its hash is stored.
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "password": meta, "username": u.Username})
}

func (s *Server) deleteAppPassword(w http.ResponseWriter, r *http.Request, u *auth.User) {
	if s.ap == nil {
		writeErr(w, http.StatusServiceUnavailable, "App passwords not available")
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if !decodeBody(w, r, &req) || strings.TrimSpace(req.ID) == "" {
		writeErr(w, http.StatusBadRequest, "An app password id is required")
		return
	}
	if err := s.ap.Delete(u.Username, strings.TrimSpace(req.ID)); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not delete app password")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ── location picker (geocode proxy) ─────────────────────────────────────────────────

// geocodeSuggest returns autocomplete results for the location field. The provider runs
// server-side (the UI may not call external APIs); a disabled or too-short query yields an empty
// list so the UI quietly falls back to free-text entry.
func (s *Server) geocodeSuggest(w http.ResponseWriter, r *http.Request, u *auth.User) {
	if s.geo == nil || !s.geo.Enabled() {
		writeJSON(w, http.StatusOK, map[string]any{"provider": "", "suggestions": []geocode.Suggestion{}})
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(q)) < 2 {
		writeJSON(w, http.StatusOK, map[string]any{"provider": s.geo.Provider(), "suggestions": []geocode.Suggestion{}})
		return
	}
	if !s.geoRate.allow(u.Username) {
		writeErr(w, http.StatusTooManyRequests, "Too many location lookups")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	res, err := s.geo.Suggest(ctx, q, strings.TrimSpace(r.URL.Query().Get("session")))
	if errors.Is(err, geocode.ErrBudgetExceeded) {
		// Daily cap hit: degrade quietly to free-text rather than erroring the field.
		writeJSON(w, http.StatusOK, map[string]any{"provider": s.geo.Provider(), "suggestions": []geocode.Suggestion{}})
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Location search is unavailable")
		return
	}
	if res == nil {
		res = []geocode.Suggestion{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": s.geo.Provider(), "suggestions": res})
}

// geocodeResolve turns a suggestion id into a full place (address + coordinates). Only needed for
// providers whose suggestions are not pre-resolved (Google); Photon results arrive resolved.
func (s *Server) geocodeResolve(w http.ResponseWriter, r *http.Request, u *auth.User) {
	if s.geo == nil || !s.geo.Enabled() {
		writeErr(w, http.StatusServiceUnavailable, "Location lookup not available")
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "A place id is required")
		return
	}
	if !s.geoRate.allow(u.Username) {
		writeErr(w, http.StatusTooManyRequests, "Too many location lookups")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	place, err := s.geo.Resolve(ctx, id, strings.TrimSpace(r.URL.Query().Get("session")))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Could not resolve the location")
		return
	}
	writeJSON(w, http.StatusOK, place)
}

// rateLimiter is a simple per-key sliding-window limiter (used to cap geocode lookups per user,
// independent of the UI's debounce — an anti-abuse / cost guard for a shared upstream key).
type rateLimiter struct {
	mu     sync.Mutex
	hits   map[string][]int64
	window time.Duration
	max    int
}

func newRateLimiter(window time.Duration, max int) *rateLimiter {
	return &rateLimiter{hits: map[string][]int64{}, window: window, max: max}
}

func (rl *rateLimiter) allow(key string) bool {
	now := time.Now().UnixNano()
	cutoff := now - int64(rl.window)
	rl.mu.Lock()
	defer rl.mu.Unlock()
	// Bound memory: when the map grows large, drop keys that have gone idle (all timestamps
	// expired), so it tracks active users rather than the all-time set (parity with throttle).
	if len(rl.hits) > 4096 {
		for k, ts := range rl.hits {
			if len(ts) == 0 || ts[len(ts)-1] <= cutoff {
				delete(rl.hits, k)
			}
		}
	}
	kept := rl.hits[key][:0]
	for _, t := range rl.hits[key] {
		if t > cutoff {
			kept = append(kept, t)
		}
	}
	if len(kept) >= rl.max {
		rl.hits[key] = kept
		return false
	}
	rl.hits[key] = append(kept, now)
	return true
}

// throttle is a per-key (client IP) authentication-failure backoff guarding the DAV Basic-auth
// brute-force surface (plan M4). It is intentionally simple: a sustained run of failures earns an
// escalating lockout, and any success clears the key.
type throttle struct {
	mu    sync.Mutex
	fails map[string]*failRec
}

type failRec struct {
	count int
	until time.Time
}

func newThrottle() *throttle { return &throttle{fails: map[string]*failRec{}} }

func (t *throttle) blocked(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.fails[key]
	return r != nil && time.Now().Before(r.until)
}

func (t *throttle) fail(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.fails) > 4096 { // bound memory against spoofed-IP floods: drop expired entries
		now := time.Now()
		for k, r := range t.fails {
			if now.After(r.until) {
				delete(t.fails, k)
			}
		}
	}
	r := t.fails[key]
	if r == nil {
		r = &failRec{}
		t.fails[key] = r
	}
	r.count++
	if r.count >= 5 { // first few failures are free (client setup fumbles); then back off
		backoff := time.Duration(r.count-4) * 10 * time.Second
		if backoff > 5*time.Minute {
			backoff = 5 * time.Minute
		}
		r.until = time.Now().Add(backoff)
	}
}

func (t *throttle) ok(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.fails, key)
}

// clientIP extracts the originating client address for the throttle key. The daemon binds
// loopback and is only reached through the local Caddy proxy, which APPENDS the real peer to
// X-Forwarded-For. We therefore trust the RIGHTMOST entry (the one Caddy added); a client cannot
// influence it. The leftmost entries are client-supplied and must never be used, or an attacker
// could rotate them to evade the per-IP backoff.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
			return last
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// ── helpers ─────────────────────────────────────────────────────────────────────────

func splitAddrs(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func writeSSE(w io.Writer, fl http.Flusher, id int64, eventName string, data any) {
	b, _ := json.Marshal(data)
	if id > 0 {
		fmt.Fprintf(w, "id: %d\n", id)
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, b)
	fl.Flush()
}

// lastEventID reads the SSE resume position from the Last-Event-ID header, falling back to a
// query param (EventSource cannot set headers on the initial connect).
func lastEventID(r *http.Request) int64 {
	v := r.Header.Get("Last-Event-ID")
	if v == "" {
		v = r.URL.Query().Get("lastEventId")
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	return n
}

func (s *Server) calendarOf(user, calID string) (event.Calendar, bool) {
	cals, err := s.st.Calendars(user)
	if err != nil {
		return event.Calendar{}, false
	}
	for _, c := range cals {
		if c.ID == calID {
			return c, true
		}
	}
	return event.Calendar{}, false
}

// sameAddr compares two calendar-user addresses, ignoring case, whitespace and a mailto: scheme.
func sameAddr(a, b string) bool {
	n := func(s string) string {
		return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "mailto:")))
	}
	return n(a) != "" && n(a) == n(b)
}

// dedupAttendees keeps the first occurrence of each distinct address (case/scheme-insensitive),
// so an event never carries — or invites — the same person twice.
func dedupAttendees(in []event.Participant) []event.Participant {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, a := range in {
		k := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(a.Email), "mailto:")))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, a)
	}
	return out
}

// mergePartStats preserves attendees' already-collected participation status across an organizer
// edit: the editor always submits attendees as NEEDS-ACTION, so for any attendee the client did
// not explicitly set, carry over the responded status stored on the prior event.
func mergePartStats(ev, prev *event.Event) {
	if prev == nil {
		return
	}
	for i := range ev.Attendees {
		if ps := ev.Attendees[i].PartStat; ps != "" && ps != "NEEDS-ACTION" {
			continue // the client asserted a status — respect it
		}
		for _, p := range prev.Attendees {
			if sameAddr(p.Email, ev.Attendees[i].Email) && p.PartStat != "" && p.PartStat != "NEEDS-ACTION" {
				ev.Attendees[i].PartStat = p.PartStat
				break
			}
		}
	}
}

func calendarParam(r *http.Request) string {
	if c := strings.TrimSpace(r.URL.Query().Get("calendar")); c != "" {
		return c
	}
	return "personal"
}

func parseTime(s string, def time.Time) time.Time {
	if s == "" {
		return def
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC()
	}
	return def
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(v); err != nil && err != io.EOF {
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}
