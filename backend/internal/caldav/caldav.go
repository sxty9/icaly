// Package caldav is icaly's hand-rolled CalDAV (RFC 4791) + WebDAV-Sync (RFC 6578) surface,
// mounted under /api/services/icaly/dav/. It is deliberately NOT built on go-webdav: the plan's
// byte-fidelity invariant (B1) requires that a GET return the exact bytes a client PUT (so the
// served hash equals the stored ETag and strict clients never resync-loop or lose X-props),
// which go-webdav's parsed-*ical.Calendar Backend cannot promise. Hand-rolling the verb +
// XML layer over the hybrid store gives that guarantee and matches how mail hand-rolled JMAP.
//
// The store is the single source of truth; this package only translates WebDAV verbs/XML to
// store calls and never re-encodes event bytes. Authentication, rights and the per-IP failure
// throttle live in package api (guardDav); Serve receives an already-authenticated principal.
package caldav

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"icaly/internal/event"
	"icaly/internal/ical"
	"icaly/internal/store"
)

// WebDAV / CalDAV / Calendar-Server / Apple-iCal namespaces. encoding/xml resolves request
// elements to these URLs on decode; responses are written with the fixed prefixes below.
const (
	nsDAV    = "DAV:"
	nsCalDAV = "urn:ietf:params:xml:ns:caldav"
	nsCalSrv = "http://calendarserver.org/ns/"
	nsApple  = "http://apple.com/ns/ical/"

	syncPrefix = "urn:icaly:sync:" // opaque, origin-free WebDAV-Sync token scheme
	maxDav     = 8 << 20           // 8 MiB cap on PUT bodies / REPORT requests
)

// Handler serves the DAV subtree for one store. base is the absolute path the subtree is
// mounted at (e.g. "/api/services/icaly/dav/"); addressOf maps a user to their calendar-user
// address for calendar-user-address-set (may be nil).
type Handler struct {
	st        *store.Store
	base      string
	addressOf func(user string) string
}

// New builds a Handler. base must end in a slash.
func New(st *store.Store, base string, addressOf func(string) string) *Handler {
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return &Handler{st: st, base: base, addressOf: addressOf}
}

// resource kinds in the DAV tree.
const (
	resBad = iota
	resRoot
	resPrincipal
	resHome
	resCollection
	resObject
)

type resource struct {
	kind   int
	user   string
	cal    string
	object string // resource name without the .ics suffix (the store key)
}

// Serve dispatches one DAV request for the already-authenticated principal. canEdit gates the
// mutating verbs; isAdmin lets an admin act on another user's tree (Schicht-2 bypass).
func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, authUser string, isAdmin, canEdit bool) {
	rel := strings.TrimPrefix(r.URL.Path, h.base)
	res := parsePath(rel)

	// Path-scoping: a principal may only act within their own tree unless they are admin.
	if res.user != "" && res.user != authUser && !isAdmin {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	switch r.Method {
	case "OPTIONS":
		h.options(w)
	case "PROPFIND":
		h.propfind(w, r, res, authUser)
	case "REPORT":
		h.report(w, r, res)
	case "GET", "HEAD":
		h.get(w, r, res)
	case "PUT":
		if !canEdit {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		h.put(w, r, res)
	case "DELETE":
		if !canEdit {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		h.del(w, r, res)
	case "MKCALENDAR":
		if !canEdit {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		h.mkcalendar(w, r, res)
	case "PROPPATCH":
		if !canEdit {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		h.proppatch(w, r, res)
	default:
		w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, PROPPATCH, REPORT, MKCALENDAR")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func parsePath(rel string) resource {
	rel = strings.Trim(rel, "/")
	if rel == "" {
		return resource{kind: resRoot}
	}
	parts := strings.Split(rel, "/")
	switch parts[0] {
	case "principals":
		if len(parts) >= 2 && parts[1] != "" {
			return resource{kind: resPrincipal, user: parts[1]}
		}
	case "calendars":
		switch len(parts) {
		case 2:
			return resource{kind: resHome, user: parts[1]}
		case 3:
			return resource{kind: resCollection, user: parts[1], cal: parts[2]}
		case 4:
			return resource{kind: resObject, user: parts[1], cal: parts[2], object: strings.TrimSuffix(parts[3], ".ics")}
		}
	}
	return resource{kind: resBad}
}

// ── OPTIONS ─────────────────────────────────────────────────────────────────────────

func (h *Handler) options(w http.ResponseWriter) {
	// Advertise classes 1 and 3 (core + RFC 4918 revisions) and calendar-access. We deliberately
	// do NOT claim class 2 (LOCK/UNLOCK): CalDAV clients use ETag/If-Match for concurrency, and
	// advertising a compliance class whose methods 405 would be a false capability.
	w.Header().Set("DAV", "1, 3, calendar-access")
	w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, PROPPATCH, REPORT, MKCALENDAR")
	w.WriteHeader(http.StatusOK)
}

// ── PROPFIND ────────────────────────────────────────────────────────────────────────

func (h *Handler) propfind(w http.ResponseWriter, r *http.Request, res resource, authUser string) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxDav))
	names, allprop := parsePropfind(body)
	d := depth(r)

	var responses []string
	switch res.kind {
	case resRoot:
		responses = append(responses, h.rootResponse(authUser, names, allprop))
	case resPrincipal:
		responses = append(responses, h.principalResponse(res.user, names, allprop))
	case resHome:
		responses = append(responses, h.homeResponse(res.user, names, allprop))
		if d >= 1 {
			cals, _ := h.st.Calendars(res.user)
			for _, c := range cals {
				responses = append(responses, h.collectionResponse(res.user, c, names, allprop))
			}
		}
	case resCollection:
		cal, ok := h.calendar(res.user, res.cal)
		if !ok {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		responses = append(responses, h.collectionResponse(res.user, cal, names, allprop))
		if d >= 1 {
			metas, _ := h.st.EventMetas(res.user, res.cal)
			for _, m := range metas {
				responses = append(responses, h.objectResponse(res.user, res.cal, m, names, allprop))
			}
		}
	case resObject:
		b, etag, err := h.st.RawEvent(res.user, res.cal, res.object)
		if err != nil {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		m := store.ObjectMeta{UID: res.object, ETag: etag}
		responses = append(responses, h.objectResponseData(res.user, res.cal, m, b, names, allprop))
	default:
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	writeMS(w, responses, "")
}

func (h *Handler) rootResponse(authUser string, names []xml.Name, allprop bool) string {
	supported := []kv{
		{dav("resourcetype"), "<D:collection/>"},
		{dav("current-user-principal"), h.hrefElem(h.principalPath(authUser))},
		{dav("principal-collection-set"), h.hrefElem(h.base + "principals/")},
		{cdav("calendar-home-set"), h.hrefElem(h.calendarHome(authUser))},
	}
	ok, missing := buildPropstats(supported, names, allprop)
	return renderResponse(h.base, ok, missing, "")
}

func (h *Handler) principalResponse(user string, names []xml.Name, allprop bool) string {
	supported := []kv{
		{dav("resourcetype"), "<D:principal/>"},
		{dav("displayname"), esc(user)},
		{dav("current-user-principal"), h.hrefElem(h.principalPath(user))},
		{dav("principal-URL"), h.hrefElem(h.principalPath(user))},
		{cdav("calendar-home-set"), h.hrefElem(h.calendarHome(user))},
	}
	if h.addressOf != nil {
		if addr := h.addressOf(user); addr != "" {
			supported = append(supported, kv{cdav("calendar-user-address-set"), h.hrefElem("mailto:" + addr)})
		}
	}
	ok, missing := buildPropstats(supported, names, allprop)
	return renderResponse(h.principalPath(user), ok, missing, "")
}

func (h *Handler) homeResponse(user string, names []xml.Name, allprop bool) string {
	supported := []kv{
		{dav("resourcetype"), "<D:collection/>"},
		{dav("displayname"), "Calendars"},
		{dav("current-user-principal"), h.hrefElem(h.principalPath(user))},
		{dav("owner"), h.hrefElem(h.principalPath(user))},
	}
	ok, missing := buildPropstats(supported, names, allprop)
	return renderResponse(h.calendarHome(user), ok, missing, "")
}

func (h *Handler) collectionResponse(user string, c event.Calendar, names []xml.Name, allprop bool) string {
	ctagNum, _ := strconv.ParseInt(c.CTag, 10, 64)
	priv := "<D:privilege><D:read/></D:privilege>"
	if !c.ReadOnly {
		priv += "<D:privilege><D:write/></D:privilege><D:privilege><D:write-content/></D:privilege>" +
			"<D:privilege><D:bind/></D:privilege><D:privilege><D:unbind/></D:privilege>"
	}
	reports := report("C", "calendar-query") + report("C", "calendar-multiget") +
		report("D", "sync-collection") + report("C", "free-busy-query")
	supported := []kv{
		{dav("resourcetype"), "<D:collection/><C:calendar/>"},
		{dav("displayname"), esc(c.Name)},
		{cdav("calendar-description"), esc(c.Description)},
		{cdav("supported-calendar-component-set"), `<C:comp name="VEVENT"/>`},
		{csrv("getctag"), esc(c.CTag)},
		{dav("sync-token"), esc(h.syncToken(ctagNum))},
		{dav("owner"), h.hrefElem(h.principalPath(c.Owner))},
		{dav("current-user-principal"), h.hrefElem(h.principalPath(user))},
		{dav("supported-report-set"), reports},
		{dav("current-user-privilege-set"), priv},
	}
	if c.Color != "" {
		supported = append(supported, kv{apple("calendar-color"), esc(c.Color)})
	}
	ok, missing := buildPropstats(supported, names, allprop)
	return renderResponse(h.collectionPath(user, c.ID), ok, missing, "")
}

// objectResponse renders an object's props, reading the verbatim bytes only if calendar-data or
// getcontentlength was explicitly requested (cheap depth-1 listings stay file-read-free).
func (h *Handler) objectResponse(user, cal string, m store.ObjectMeta, names []xml.Name, allprop bool) string {
	return h.objectResponseFn(user, cal, m, names, allprop, func() ([]byte, bool) {
		b, _, err := h.st.RawEvent(user, cal, m.UID)
		return b, err == nil
	})
}

func (h *Handler) objectResponseData(user, cal string, m store.ObjectMeta, data []byte, names []xml.Name, allprop bool) string {
	return h.objectResponseFn(user, cal, m, names, allprop, func() ([]byte, bool) { return data, true })
}

func (h *Handler) objectResponseFn(user, cal string, m store.ObjectMeta, names []xml.Name, allprop bool, data func() ([]byte, bool)) string {
	supported := []kv{
		{dav("getetag"), esc(m.ETag)},
		{dav("getcontenttype"), "text/calendar; charset=utf-8; component=VEVENT"},
		{dav("resourcetype"), ""}, // an object is not a collection
		{dav("current-user-principal"), h.hrefElem(h.principalPath(user))},
		{dav("owner"), h.hrefElem(h.principalPath(user))},
	}
	// calendar-data / getcontentlength are returned only on explicit request, never on allprop
	// (RFC 4791 §9.6: calendar-data is not part of allprop, and the body read is not free).
	if hasName(names, cdav("calendar-data")) || hasName(names, dav("getcontentlength")) {
		if b, ok := data(); ok {
			supported = append(supported,
				kv{dav("getcontentlength"), strconv.Itoa(len(b))},
				kv{cdav("calendar-data"), escCalData(string(b))},
			)
		}
	}
	ok, missing := buildPropstats(supported, names, allprop)
	return renderResponse(h.objectPath(user, cal, m.UID), ok, missing, "")
}

// ── GET / HEAD ──────────────────────────────────────────────────────────────────────

func (h *Handler) get(w http.ResponseWriter, r *http.Request, res resource) {
	if res.kind != resObject {
		w.Header().Set("Allow", "OPTIONS, PROPFIND, REPORT")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	b, etag, err := h.st.RawEvent(res.user, res.cal, res.object)
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	if r.Method == "HEAD" {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(b)
}

// ── PUT ─────────────────────────────────────────────────────────────────────────────

func (h *Handler) put(w http.ResponseWriter, r *http.Request, res resource) {
	if res.kind != resObject {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	// MaxBytesReader (not LimitReader) errors on overflow instead of silently truncating, so an
	// oversized PUT is rejected rather than stored as a corrupt prefix.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDav))
	if err != nil {
		http.Error(w, "Request entity too large", http.StatusRequestEntityTooLarge)
		return
	}
	etag, created, err := h.st.PutRaw(res.user, res.cal, res.object, body,
		r.Header.Get("If-Match"), r.Header.Get("If-None-Match"))
	switch {
	case err == nil:
		w.Header().Set("ETag", etag)
		if created {
			w.WriteHeader(http.StatusCreated)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
	case errors.Is(err, store.ErrPreconditionFailed):
		http.Error(w, "Precondition Failed", http.StatusPreconditionFailed)
	case errors.Is(err, store.ErrNotFound):
		// The calendar collection does not exist.
		http.Error(w, "Conflict: calendar does not exist", http.StatusConflict)
	case errors.Is(err, store.ErrInvalidCalendar):
		// Body was not parseable iCalendar → the CalDAV valid-calendar-data precondition.
		writeDavError(w, http.StatusForbidden, "<C:valid-calendar-data/>")
	default:
		// Disk / transaction failure — a server fault, not the client's bad data.
		http.Error(w, "Server Error", http.StatusInternalServerError)
	}
}

// ── DELETE ──────────────────────────────────────────────────────────────────────────

func (h *Handler) del(w http.ResponseWriter, r *http.Request, res resource) {
	switch res.kind {
	case resObject:
		err := h.st.DeleteRaw(res.user, res.cal, res.object, r.Header.Get("If-Match"))
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, store.ErrPreconditionFailed):
			http.Error(w, "Precondition Failed", http.StatusPreconditionFailed)
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "Not Found", http.StatusNotFound)
		default:
			http.Error(w, "Server Error", http.StatusInternalServerError)
		}
	case resCollection:
		// Deleting an entire calendar over DAV is a data-loss footgun; the dashboard owns
		// calendar lifecycle. Refuse rather than silently drop every event.
		http.Error(w, "Forbidden", http.StatusForbidden)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// ── MKCALENDAR ──────────────────────────────────────────────────────────────────────

func (h *Handler) mkcalendar(w http.ResponseWriter, r *http.Request, res resource) {
	if res.kind != resCollection {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxDav))
	var req mkcalReq
	_ = xml.Unmarshal(body, &req)
	name, _ := findSetValue(req.Set, dav("displayname"))
	color, _ := findSetValue(req.Set, apple("calendar-color"))
	if name == "" {
		name = res.cal
	}
	_, err := h.st.CreateCalendarID(res.user, res.cal, name, color, "")
	switch {
	case err == nil:
		w.WriteHeader(http.StatusCreated)
	case errors.Is(err, store.ErrPreconditionFailed):
		http.Error(w, "Calendar already exists", http.StatusMethodNotAllowed)
	default:
		http.Error(w, "Could not create calendar", http.StatusForbidden)
	}
}

// ── PROPPATCH ───────────────────────────────────────────────────────────────────────

func (h *Handler) proppatch(w http.ResponseWriter, r *http.Request, res resource) {
	if res.kind != resCollection {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxDav))
	var req proppatchReq
	_ = xml.Unmarshal(body, &req)

	// Collect the intended writes and the forbidden props first; PROPPATCH is atomic (RFC 4918
	// §9.2) so nothing is applied unless every named property can be set. A <D:remove> of a
	// writable prop clears it. Every named property is reported in a propstat.
	var name, color *string
	empty := ""
	var writable, forbiddenNames []xml.Name
	plan := func(p propValue, remove bool) {
		switch p.XMLName {
		case dav("displayname"):
			if remove {
				name = &empty
			} else {
				v := strings.TrimSpace(p.CharData)
				name = &v
			}
			writable = append(writable, p.XMLName)
		case apple("calendar-color"):
			if remove {
				color = &empty
			} else {
				v := strings.TrimSpace(p.CharData)
				color = &v
			}
			writable = append(writable, p.XMLName)
		default:
			forbiddenNames = append(forbiddenNames, p.XMLName)
		}
	}
	for _, set := range req.Set {
		for _, p := range set.Prop.Props {
			plan(p, false)
		}
	}
	for _, rem := range req.Remove {
		for _, p := range rem.Prop.Props {
			plan(p, true)
		}
	}

	var okProps, failed, forbidden string
	if len(forbiddenNames) == 0 {
		// All writable: apply, then report 200 for each (or an empty 200 for an empty request).
		if name != nil || color != nil {
			if err := h.st.UpdateCalendar(res.user, res.cal, name, color); err != nil {
				http.Error(w, "Not Found", http.StatusNotFound)
				return
			}
		}
		for _, n := range writable {
			okProps += renderProp(n, "")
		}
	} else {
		// Atomic failure: apply nothing. Forbidden props get 403; the would-have-succeeded props
		// get 424 Failed Dependency.
		for _, n := range forbiddenNames {
			forbidden += renderMissing(n)
		}
		for _, n := range writable {
			failed += renderProp(n, "")
		}
	}

	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(msOpen)
	b.WriteString("<D:response><D:href>" + esc(h.collectionPath(res.user, res.cal)) + "</D:href>")
	if okProps != "" {
		b.WriteString("<D:propstat><D:prop>" + okProps + "</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>")
	}
	if forbidden != "" {
		b.WriteString("<D:propstat><D:prop>" + forbidden + "</D:prop><D:status>HTTP/1.1 403 Forbidden</D:status></D:propstat>")
	}
	if failed != "" {
		b.WriteString("<D:propstat><D:prop>" + failed + "</D:prop><D:status>HTTP/1.1 424 Failed Dependency</D:status></D:propstat>")
	}
	// A DAV:response must carry at least one propstat; an empty <D:prop/> request yields none above.
	if okProps == "" && forbidden == "" && failed == "" {
		b.WriteString("<D:propstat><D:prop/><D:status>HTTP/1.1 200 OK</D:status></D:propstat>")
	}
	b.WriteString("</D:response></D:multistatus>")
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = io.WriteString(w, b.String())
}

// ── REPORT ──────────────────────────────────────────────────────────────────────────

func (h *Handler) report(w http.ResponseWriter, r *http.Request, res resource) {
	if res.kind != resCollection {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxDav))
	var probe struct{ XMLName xml.Name }
	if xml.Unmarshal(body, &probe) != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	switch {
	case probe.XMLName == cdav("calendar-multiget"):
		h.reportMultiget(w, res, body)
	case probe.XMLName == cdav("calendar-query"):
		h.reportQuery(w, res, body)
	case probe.XMLName == dav("sync-collection"):
		h.reportSync(w, res, body)
	case probe.XMLName == cdav("free-busy-query"):
		h.reportFreeBusy(w, res, body)
	default:
		http.Error(w, "Unsupported REPORT", http.StatusBadRequest)
	}
}

func (h *Handler) reportMultiget(w http.ResponseWriter, res resource, body []byte) {
	var req multigetReq
	_ = xml.Unmarshal(body, &req)
	names := propNamesOrDefault(req.Prop, dav("getetag"))
	var responses []string
	for _, href := range req.Hrefs {
		uid := uidFromHref(href)
		b, etag, err := h.st.RawEvent(res.user, res.cal, uid)
		if err != nil {
			responses = append(responses, renderResponse(h.objectPath(res.user, res.cal, uid), "", "", status404))
			continue
		}
		m := store.ObjectMeta{UID: uid, ETag: etag}
		responses = append(responses, h.objectResponseData(res.user, res.cal, m, b, names, false))
	}
	writeMS(w, responses, "")
}

func (h *Handler) reportQuery(w http.ResponseWriter, res resource, body []byte) {
	var req calQueryReq
	_ = xml.Unmarshal(body, &req)
	names := propNamesOrDefault(req.Prop, dav("getetag"))
	tr := findTimeRange(&req.Filter.CompFilter)
	var from, to time.Time
	if tr != nil {
		from, to = parseCalTime(tr.Start), parseCalTime(tr.End)
	}
	useRange := tr != nil && !from.IsZero() && !to.IsZero()

	metas, err := h.st.EventMetas(res.user, res.cal)
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	var responses []string
	for _, m := range metas {
		// Recurring masters are always included (a true expansion check is deferred — the
		// client re-filters); non-recurring objects are filtered by [start,end) overlap.
		if useRange && !m.Recurring && !overlaps(m.Start, m.End, from, to) {
			continue
		}
		responses = append(responses, h.objectResponse(res.user, res.cal, m, names, false))
	}
	writeMS(w, responses, "")
}

func (h *Handler) reportSync(w http.ResponseWriter, res resource, body []byte) {
	var req syncReq
	_ = xml.Unmarshal(body, &req)
	since, ok := parseSyncToken(req.SyncToken)
	if !ok {
		writeDavError(w, http.StatusConflict, "<D:valid-sync-token/>")
		return
	}
	changes, newTok, err := h.st.SyncCollection(res.user, res.cal, since)
	if errors.Is(err, store.ErrSyncTokenTooOld) {
		writeDavError(w, http.StatusConflict, "<D:valid-sync-token/>")
		return
	}
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	names := propNamesOrDefault(req.Prop, dav("getetag"))
	var responses []string
	for _, c := range changes {
		if c.Deleted {
			responses = append(responses, renderResponse(h.objectPath(res.user, res.cal, c.UID), "", "", status404))
			continue
		}
		m := store.ObjectMeta{UID: c.UID, ETag: c.ETag}
		responses = append(responses, h.objectResponse(res.user, res.cal, m, names, false))
	}
	writeMS(w, responses, h.syncToken(newTok))
}

func (h *Handler) reportFreeBusy(w http.ResponseWriter, res resource, body []byte) {
	var req freeBusyReq
	_ = xml.Unmarshal(body, &req)
	from, to := parseCalTime(req.TimeRange.Start), parseCalTime(req.TimeRange.End)
	if from.IsZero() || to.IsZero() {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	evs, err := h.st.ListEvents(res.user, res.cal, from, to)
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	out, err := ical.FreeBusy(from, to, mergeBusy(evs, from, to))
	if err != nil {
		http.Error(w, "Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	_, _ = w.Write(out)
}

// ── request parsing ───────────────────────────────────────────────────────────────────

type davProp struct{ XMLName xml.Name }

type propContainer struct {
	Props []davProp `xml:",any"`
}

func (c *propContainer) names() []xml.Name {
	out := make([]xml.Name, 0, len(c.Props))
	for _, p := range c.Props {
		out = append(out, p.XMLName)
	}
	return out
}

type propfindReq struct {
	XMLName  xml.Name       `xml:"DAV: propfind"`
	Prop     *propContainer `xml:"DAV: prop"`
	AllProp  *struct{}      `xml:"DAV: allprop"`
	PropName *struct{}      `xml:"DAV: propname"`
}

type multigetReq struct {
	XMLName xml.Name       `xml:"urn:ietf:params:xml:ns:caldav calendar-multiget"`
	Prop    *propContainer `xml:"DAV: prop"`
	Hrefs   []string       `xml:"DAV: href"`
}

type calQueryReq struct {
	XMLName xml.Name       `xml:"urn:ietf:params:xml:ns:caldav calendar-query"`
	Prop    *propContainer `xml:"DAV: prop"`
	Filter  filterXML      `xml:"urn:ietf:params:xml:ns:caldav filter"`
}

type filterXML struct {
	CompFilter compFilterXML `xml:"urn:ietf:params:xml:ns:caldav comp-filter"`
}

type compFilterXML struct {
	Name       string         `xml:"name,attr"`
	TimeRange  *timeRangeXML  `xml:"urn:ietf:params:xml:ns:caldav time-range"`
	CompFilter *compFilterXML `xml:"urn:ietf:params:xml:ns:caldav comp-filter"`
}

type timeRangeXML struct {
	Start string `xml:"start,attr"`
	End   string `xml:"end,attr"`
}

type syncReq struct {
	XMLName   xml.Name       `xml:"DAV: sync-collection"`
	SyncToken string         `xml:"DAV: sync-token"`
	SyncLevel string         `xml:"DAV: sync-level"`
	Prop      *propContainer `xml:"DAV: prop"`
}

type freeBusyReq struct {
	XMLName   xml.Name     `xml:"urn:ietf:params:xml:ns:caldav free-busy-query"`
	TimeRange timeRangeXML `xml:"urn:ietf:params:xml:ns:caldav time-range"`
}

// propValue carries a PROPPATCH/MKCALENDAR property name plus its text value.
type propValue struct {
	XMLName  xml.Name
	CharData string `xml:",chardata"`
}

type propSet struct {
	Prop struct {
		Props []propValue `xml:",any"`
	} `xml:"DAV: prop"`
}

type proppatchReq struct {
	XMLName xml.Name  `xml:"DAV: propertyupdate"`
	Set     []propSet `xml:"DAV: set"`
	Remove  []propSet `xml:"DAV: remove"`
}

type mkcalReq struct {
	XMLName xml.Name  `xml:"urn:ietf:params:xml:ns:caldav mkcalendar"`
	Set     []propSet `xml:"DAV: set"`
}

func parsePropfind(body []byte) (names []xml.Name, allprop bool) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, true
	}
	var req propfindReq
	if xml.Unmarshal(body, &req) != nil {
		return nil, true // be lenient: an unparseable propfind is treated as allprop
	}
	if req.AllProp != nil || req.PropName != nil {
		return nil, true
	}
	if req.Prop != nil {
		return req.Prop.names(), false
	}
	return nil, true
}

func propNamesOrDefault(c *propContainer, def ...xml.Name) []xml.Name {
	if c == nil {
		return def
	}
	if n := c.names(); len(n) > 0 {
		return n
	}
	return def
}

func findSetValue(sets []propSet, want xml.Name) (string, bool) {
	for _, s := range sets {
		for _, p := range s.Prop.Props {
			if p.XMLName == want {
				return strings.TrimSpace(p.CharData), true
			}
		}
	}
	return "", false
}

func findTimeRange(cf *compFilterXML) *timeRangeXML {
	if cf == nil {
		return nil
	}
	if cf.TimeRange != nil {
		return cf.TimeRange
	}
	return findTimeRange(cf.CompFilter)
}

// ── prop resolution + XML rendering ─────────────────────────────────────────────────

type kv struct {
	name  xml.Name
	inner string
}

func find(s []kv, n xml.Name) (string, bool) {
	for _, e := range s {
		if e.name == n {
			return e.inner, true
		}
	}
	return "", false
}

func hasName(names []xml.Name, want xml.Name) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// buildPropstats splits the requested props into a found set (200) and a not-found set (404).
// On allprop (or an empty request) every supported prop is returned in declared order.
func buildPropstats(supported []kv, names []xml.Name, allprop bool) (ok, missing string) {
	if allprop || len(names) == 0 {
		for _, e := range supported {
			ok += renderProp(e.name, e.inner)
		}
		return ok, ""
	}
	for _, n := range names {
		if inner, found := find(supported, n); found {
			ok += renderProp(n, inner)
		} else {
			missing += renderMissing(n)
		}
	}
	return ok, missing
}

func renderResponse(href, ok, missing, status string) string {
	var b strings.Builder
	b.WriteString("<D:response><D:href>")
	b.WriteString(esc(href))
	b.WriteString("</D:href>")
	if status != "" {
		b.WriteString("<D:status>")
		b.WriteString(status)
		b.WriteString("</D:status>")
	} else {
		if ok != "" {
			b.WriteString("<D:propstat><D:prop>" + ok + "</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>")
		}
		if missing != "" {
			b.WriteString("<D:propstat><D:prop>" + missing + "</D:prop><D:status>HTTP/1.1 404 Not Found</D:status></D:propstat>")
		}
	}
	b.WriteString("</D:response>")
	return b.String()
}

const (
	msOpen = `<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav" ` +
		`xmlns:CS="http://calendarserver.org/ns/" xmlns:IC="http://apple.com/ns/ical/">`
	status404 = "HTTP/1.1 404 Not Found"
)

func writeMS(w http.ResponseWriter, responses []string, syncToken string) {
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(msOpen)
	for _, r := range responses {
		b.WriteString(r)
	}
	if syncToken != "" {
		b.WriteString("<D:sync-token>" + esc(syncToken) + "</D:sync-token>")
	}
	b.WriteString("</D:multistatus>")
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = io.WriteString(w, b.String())
}

func writeDavError(w http.ResponseWriter, status int, inner string) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header+`<D:error xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">`+inner+`</D:error>`)
}

func report(prefix, name string) string {
	return "<D:supported-report><D:report><" + prefix + ":" + name + "/></D:report></D:supported-report>"
}

func renderProp(n xml.Name, inner string) string {
	q := qname(n)
	if inner == "" {
		return "<" + q + "/>"
	}
	return "<" + q + ">" + inner + "</" + q + ">"
}

func renderMissing(n xml.Name) string {
	if prefixOf(n.Space) != "" {
		return "<" + qname(n) + "/>"
	}
	return "<" + n.Local + ` xmlns="` + escAttr(n.Space) + `"/>`
}

func qname(n xml.Name) string {
	if p := prefixOf(n.Space); p != "" {
		return p + ":" + n.Local
	}
	return n.Local
}

func prefixOf(space string) string {
	switch space {
	case nsDAV:
		return "D"
	case nsCalDAV:
		return "C"
	case nsCalSrv:
		return "CS"
	case nsApple:
		return "IC"
	}
	return ""
}

func dav(local string) xml.Name   { return xml.Name{Space: nsDAV, Local: local} }
func cdav(local string) xml.Name  { return xml.Name{Space: nsCalDAV, Local: local} }
func csrv(local string) xml.Name  { return xml.Name{Space: nsCalSrv, Local: local} }
func apple(local string) xml.Name { return xml.Name{Space: nsApple, Local: local} }

// ── small helpers ─────────────────────────────────────────────────────────────────────

func (h *Handler) principalPath(user string) string { return h.base + "principals/" + user + "/" }
func (h *Handler) calendarHome(user string) string  { return h.base + "calendars/" + user + "/" }
func (h *Handler) collectionPath(user, c string) string {
	return h.base + "calendars/" + user + "/" + c + "/"
}
func (h *Handler) objectPath(user, c, uid string) string {
	return h.base + "calendars/" + user + "/" + c + "/" + uid + ".ics"
}
func (h *Handler) hrefElem(p string) string { return "<D:href>" + esc(p) + "</D:href>" }

func (h *Handler) calendar(user, calID string) (event.Calendar, bool) {
	cals, err := h.st.Calendars(user)
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

func (h *Handler) syncToken(n int64) string { return syncPrefix + strconv.FormatInt(n, 10) }

func parseSyncToken(tok string) (int64, bool) {
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return 0, true // empty token = initial sync
	}
	if !strings.HasPrefix(tok, syncPrefix) {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(tok, syncPrefix), 10, 64)
	if err != nil || n < 0 {
		return 0, false // malformed or negative → DAV:valid-sync-token (409), not a silent resync
	}
	return n, true
}

func uidFromHref(href string) string {
	if i := strings.LastIndex(href, "/"); i >= 0 {
		href = href[i+1:]
	}
	return strings.TrimSuffix(href, ".ics")
}

func depth(r *http.Request) int {
	switch strings.TrimSpace(r.Header.Get("Depth")) {
	case "1":
		return 1
	case "infinity":
		return 1 // our trees are shallow; cap infinity at one level
	default:
		return 0
	}
}

func overlaps(s, e, from, to time.Time) bool {
	if e.Before(s) || e.Equal(s) {
		e = s.Add(time.Second) // zero-length: treat as a point so it can still match
	}
	return s.Before(to) && e.After(from)
}

func parseCalTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{"20060102T150405Z", "20060102T150405", "20060102"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func mergeBusy(evs []*event.Event, start, end time.Time) [][2]time.Time {
	var slots [][2]time.Time
	for _, ev := range evs {
		if strings.EqualFold(ev.Transparency, "TRANSPARENT") || strings.EqualFold(ev.Status, "CANCELLED") {
			continue
		}
		s, e := ev.Start, ev.End
		if e.IsZero() {
			e = s.Add(ev.Duration())
		}
		if s.Before(start) {
			s = start
		}
		if e.After(end) {
			e = end
		}
		if !s.Before(e) {
			continue
		}
		slots = append(slots, [2]time.Time{s.UTC(), e.UTC()})
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i][0].Before(slots[j][0]) })
	var merged [][2]time.Time
	for _, sl := range slots {
		if n := len(merged); n > 0 && !sl[0].After(merged[n-1][1]) {
			if sl[1].After(merged[n-1][1]) {
				merged[n-1][1] = sl[1]
			}
			continue
		}
		merged = append(merged, sl)
	}
	return merged
}

var (
	xmlEsc = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	// attrEsc additionally escapes the double quote, for use inside attribute values.
	attrEsc = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	// calDataEsc additionally escapes CR as the numeric reference &#13; so the CRLF line breaks
	// RFC 5545 mandates survive an XML processor's mandatory CRLF→LF text normalisation (XML 1.0
	// §2.11) on the client. Without it, calendar-data fetched via REPORT would arrive LF-only —
	// no longer byte-identical to GET and no longer the preimage of the strong ETag (plan B1).
	calDataEsc = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\r", "&#13;")
)

// esc escapes XML text content. (Quotes are legal in element text, so they are left as-is.)
func esc(s string) string { return xmlEsc.Replace(s) }

// escAttr escapes a value destined for an XML attribute (adds the double quote).
func escAttr(s string) string { return attrEsc.Replace(s) }

// escCalData escapes verbatim iCalendar bytes for an XML text node, preserving CR as &#13;.
func escCalData(s string) string { return calDataEsc.Replace(s) }
