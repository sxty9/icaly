// Package store is icaly's hybrid persistence layer: per-calendar .ics files on disk are
// the single source of truth (filesystem-first, like maild's Maildir), and an embedded
// pure-Go SQLite database is a DERIVED, rebuildable index + monotonic change-log. The
// change-log's autoincrement seq is the one spine that drives ctag, sync-token, JMAP state
// and live push. Writes are atomic (tmp→fsync→rename) followed by one index transaction.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"icaly/internal/event"
	"icaly/internal/ical"
)

// ErrNotFound is returned for a missing calendar or event.
var ErrNotFound = errors.New("not found")

// Change describes one committed mutation. It is broadcast to live subscribers (the push
// hub) after the index transaction commits, and is the same shape replayed to reconnecting
// SSE clients via the change-log. Seq is the monotonic change-log id.
type Change struct {
	Seq        int64     `json:"seq"`
	Owner      string    `json:"-"`
	CalendarID string    `json:"calendar"`
	UID        string    `json:"uid"`
	Type       string    `json:"type"` // put | delete
	ETag       string    `json:"etag,omitempty"`
	At         time.Time `json:"at"`
}

const schema = `
CREATE TABLE IF NOT EXISTS calendars(
  id          TEXT NOT NULL,
  owner       TEXT NOT NULL,
  kind        TEXT NOT NULL DEFAULT 'personal',
  name        TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  color       TEXT NOT NULL DEFAULT '',
  timezone    TEXT NOT NULL DEFAULT '',
  ord         INTEGER NOT NULL DEFAULT 0,
  readonly    INTEGER NOT NULL DEFAULT 0,
  feed_token  TEXT NOT NULL DEFAULT '',
  public      INTEGER NOT NULL DEFAULT 0,
  ctag        TEXT NOT NULL DEFAULT '0',
  min_valid_seq INTEGER NOT NULL DEFAULT 0,
  created     INTEGER NOT NULL,
  updated     INTEGER NOT NULL,
  PRIMARY KEY(owner, id)
);
CREATE TABLE IF NOT EXISTS events(
  owner       TEXT NOT NULL,
  calendar_id TEXT NOT NULL,
  uid         TEXT NOT NULL,
  etag        TEXT NOT NULL,
  dtstart     INTEGER NOT NULL,
  dtend       INTEGER NOT NULL,
  all_day     INTEGER NOT NULL DEFAULT 0,
  recurring   INTEGER NOT NULL DEFAULT 0,
  summary     TEXT NOT NULL DEFAULT '',
  json        TEXT NOT NULL,
  PRIMARY KEY(owner, calendar_id, uid)
);
CREATE INDEX IF NOT EXISTS events_range ON events(owner, calendar_id, dtstart, dtend);
CREATE TABLE IF NOT EXISTS changelog(
  seq         INTEGER PRIMARY KEY AUTOINCREMENT,
  owner       TEXT NOT NULL,
  calendar_id TEXT NOT NULL,
  uid         TEXT NOT NULL,
  change_type TEXT NOT NULL,
  etag        TEXT NOT NULL DEFAULT '',
  at          INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS changelog_cal ON changelog(owner, calendar_id, seq);
`

// Store owns the data root and the derived SQLite index.
type Store struct {
	root string
	db   *sql.DB
	mu   sync.Mutex // serialises mutations (one writer; SQLite max-open-conns is 1)

	subMu sync.RWMutex
	subs  []func(Change) // live-change observers (push hub); notified after commit

	// removeFile deletes an event's source-of-truth .ics file. It is a field only so a test can
	// force a removal failure and assert the delete aborts atomically; production always uses os.Remove.
	removeFile func(path string) error
}

// OnChange registers an observer invoked (synchronously, after commit) for every mutation.
// The push hub uses this to fan changes out to SSE clients; observers must not block.
func (s *Store) OnChange(fn func(Change)) {
	s.subMu.Lock()
	s.subs = append(s.subs, fn)
	s.subMu.Unlock()
}

func (s *Store) emit(c Change) {
	s.subMu.RLock()
	subs := s.subs
	s.subMu.RUnlock()
	for _, fn := range subs {
		fn(c)
	}
}

// Open initialises the data root and the index, running migrations.
func Open(root string) (*Store, error) {
	if root == "" {
		root = "/var/lib/icaly"
	}
	for _, d := range []string{root, filepath.Join(root, "calendars"), filepath.Join(root, "index")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "index", "icaly.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	migrate(db)
	st := &Store{root: root, db: db, removeFile: os.Remove}
	if err := st.reconcile(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return st, nil
}

// migrate applies additive, idempotent schema upgrades for databases created by an earlier
// version. Each ALTER is allowed to fail (the column already exists on a fresh schema).
func migrate(db *sql.DB) {
	_, _ = db.Exec(`ALTER TABLE calendars ADD COLUMN min_valid_seq INTEGER NOT NULL DEFAULT 0`)
}

// reconcile makes the derived index converge to the .ics files, which are the single source of
// truth (plan §3). Run once at startup, it heals a crash between an .ics write and its index
// commit, picks up out-of-band edits, and lets the index be wiped and rebuilt from the caldir:
// for each known calendar, any .ics whose bytes hash to a different ETag than its index row (or
// has no row) is re-indexed and a "put" logged; any indexed event whose file has vanished is
// tombstoned. It is a no-op when index and disk already agree, and runs before any subscriber is
// attached, so its change emissions are silently dropped (only the change-log rows persist).
func (s *Store) reconcile() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT owner, id FROM calendars`)
	if err != nil {
		return err
	}
	type cal struct{ owner, id string }
	var cals []cal
	for rows.Next() {
		var c cal
		if err := rows.Scan(&c.owner, &c.id); err != nil {
			rows.Close()
			return err
		}
		cals = append(cals, c)
	}
	rows.Close()
	for _, c := range cals {
		dir := s.calDir(c.owner, c.id)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // calendar row with no directory yet (never written to)
		}
		onDisk := make(map[string]bool)
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".ics" {
				continue
			}
			// Recover the exact UID from the filename (reverse of encodeName), so an event whose
			// UID contains rewritten bytes (e.g. an imported localpart@domain UID) keeps its real
			// index key instead of being re-keyed to a sanitised string.
			uid := decodeName(strings.TrimSuffix(e.Name(), ".ics"))
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			onDisk[uid] = true
			etag := etagOf(b)
			var cur string
			switch qerr := s.db.QueryRow(`SELECT etag FROM events WHERE owner=? AND calendar_id=? AND uid=?`,
				c.owner, c.id, uid).Scan(&cur); {
			case qerr == nil && cur == etag:
				continue // index already matches the file
			case qerr != nil && !errors.Is(qerr, sql.ErrNoRows):
				return qerr
			}
			ev, derr := ical.Decode(b)
			if derr != nil {
				continue // undecodable: keep the bytes, skip indexing
			}
			ev.UID, ev.CalendarID = uid, c.id
			if err := s.indexUpsert(c.owner, c.id, uid, ev, etag); err != nil {
				return err
			}
		}
		// Index rows whose .ics file disappeared (removed out-of-band) → tombstone for convergence.
		erows, err := s.db.Query(`SELECT uid FROM events WHERE owner=? AND calendar_id=?`, c.owner, c.id)
		if err != nil {
			return err
		}
		var orphans []string
		for erows.Next() {
			var uid string
			if err := erows.Scan(&uid); err != nil {
				erows.Close()
				return err
			}
			if !onDisk[uid] {
				orphans = append(orphans, uid)
			}
		}
		erows.Close()
		for _, uid := range orphans {
			if err := s.indexDelete(c.owner, c.id, uid); err != nil {
				return err
			}
		}
	}
	return nil
}

// Close releases the index handle.
func (s *Store) Close() error { return s.db.Close() }

// ── calendars ───────────────────────────────────────────────────────────────────────

// Calendars returns the user's calendars, auto-provisioning a default "personal" one on
// first access (parallels maild creating a Maildir on demand).
func (s *Store) Calendars(user string) ([]event.Calendar, error) {
	if err := s.ensureHome(user); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT id,owner,kind,name,description,color,timezone,ord,readonly,public,ctag,feed_token
		FROM calendars WHERE owner=? ORDER BY ord, name`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []event.Calendar
	for rows.Next() {
		var c event.Calendar
		var ro, pub int
		if err := rows.Scan(&c.ID, &c.Owner, &c.Kind, &c.Name, &c.Description, &c.Color, &c.TimeZone, &c.Order, &ro, &pub, &c.CTag, &c.FeedToken); err != nil {
			return nil, err
		}
		c.ReadOnly, c.Public = ro == 1, pub == 1
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateCalendar adds a new personal calendar for the user.
func (s *Store) CreateCalendar(user, name, color, tz string) (event.Calendar, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureHome(user); err != nil {
		return event.Calendar{}, err
	}
	return s.createCalendar(user, genID(8), name, color, tz)
}

// CreateCalendarID adds a calendar with a caller-chosen id (CalDAV MKCALENDAR, where the URL
// fixes the collection name). The id must already be path-safe and must not exist.
func (s *Store) CreateCalendarID(user, id, name, color, tz string) (event.Calendar, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" || safeName(id) != id {
		return event.Calendar{}, errors.New("invalid calendar id")
	}
	if err := s.ensureHome(user); err != nil {
		return event.Calendar{}, err
	}
	if s.calOwned(user, id) {
		return event.Calendar{}, ErrPreconditionFailed
	}
	return s.createCalendar(user, id, name, color, tz)
}

// UpdateCalendar applies CalDAV PROPPATCH changes: a non-nil name/color is written, nil is left
// untouched. Returns ErrNotFound for an unknown calendar.
func (s *Store) UpdateCalendar(user, id string, name, color *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.calOwned(user, id) {
		return ErrNotFound
	}
	if name != nil {
		if _, err := s.db.Exec(`UPDATE calendars SET name=? WHERE owner=? AND id=?`, *name, user, id); err != nil {
			return err
		}
	}
	if color != nil {
		if _, err := s.db.Exec(`UPDATE calendars SET color=? WHERE owner=? AND id=?`, *color, user, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ensureHome(user string) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM calendars WHERE owner=?`, user).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := s.createCalendar(user, "personal", "Personal", "#3b82f6", "")
	return err
}

func (s *Store) createCalendar(user, id, name, color, tz string) (event.Calendar, error) {
	if name == "" {
		name = id
	}
	now := time.Now().Unix()
	if err := os.MkdirAll(filepath.Join(s.calDir(user, id), ".tmp"), 0o755); err != nil {
		return event.Calendar{}, err
	}
	// ON CONFLICT DO NOTHING makes auto-provisioning idempotent: ensureHome runs from unlocked
	// read paths, so two concurrent first-access reads can both reach here; the loser is a no-op
	// rather than a unique-constraint error. CreateCalendar/CreateCalendarID never collide (random
	// id, or an explicit pre-check), so this does not mask a real duplicate for them.
	_, err := s.db.Exec(`INSERT INTO calendars(id,owner,kind,name,description,color,timezone,ord,readonly,feed_token,public,ctag,created,updated)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(owner,id) DO NOTHING`,
		id, user, "personal", name, "", color, tz, 0, 0, genID(24), 0, "0", now, now)
	if err != nil {
		return event.Calendar{}, err
	}
	return event.Calendar{ID: id, Owner: user, Kind: "personal", Name: name, Color: color, TimeZone: tz, CTag: "0"}, nil
}

func (s *Store) calOwned(user, calID string) bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM calendars WHERE owner=? AND id=?`, user, calID).Scan(&n)
	return n > 0
}

// CTag returns the collection tag (latest change seq) for a calendar.
func (s *Store) CTag(user, calID string) (string, error) {
	var ct string
	err := s.db.QueryRow(`SELECT ctag FROM calendars WHERE owner=? AND id=?`, user, calID).Scan(&ct)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return ct, err
}

// ── events ──────────────────────────────────────────────────────────────────────────

// PutEvent creates or replaces an event: it assigns a UID when absent, preserves Created and
// bumps Sequence on update, writes the canonical .ics atomically, then updates the index and
// appends a change-log row. Returns the new ETag.
func (s *Store) PutEvent(user, calID string, ev *event.Event) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureHome(user); err != nil {
		return "", err
	}
	if !s.calOwned(user, calID) {
		return "", ErrNotFound
	}
	now := time.Now().UTC()
	if ev.UID == "" {
		ev.UID = genID(16)
	}
	var prevJSON string
	switch err := s.db.QueryRow(`SELECT json FROM events WHERE owner=? AND calendar_id=? AND uid=?`,
		user, calID, ev.UID).Scan(&prevJSON); {
	case err == nil:
		var old event.Event
		if json.Unmarshal([]byte(prevJSON), &old) == nil {
			if !old.Created.IsZero() {
				ev.Created = old.Created
			}
			ev.Sequence = old.Sequence + 1
		}
	case errors.Is(err, sql.ErrNoRows):
		// new event
	default:
		return "", err
	}
	if ev.Created.IsZero() {
		ev.Created = now
	}
	ev.Updated = now

	b, err := ical.Encode(ev)
	if err != nil {
		return "", err
	}
	etag := etagOf(b)
	if err := writeFileAtomic(s.icsPath(user, calID, ev.UID), s.calDir(user, calID), b); err != nil {
		return "", err
	}
	if err := s.indexUpsert(user, calID, ev.UID, ev, etag); err != nil {
		return "", err
	}
	return etag, nil
}

// indexUpsert refreshes the derived index for one event and appends a "put" change-log row, in a
// single transaction, then broadcasts the change. The .ics file is the source of truth and must
// already be written by the caller; etag is the hash of those bytes. Shared by PutEvent, PutRaw
// and the startup reconciler so all write paths log and emit identically.
func (s *Store) indexUpsert(user, calID, uid string, ev *event.Event, etag string) error {
	evJSON, _ := json.Marshal(ev)
	dtstart, dtend := indexRange(ev)
	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO events(owner,calendar_id,uid,etag,dtstart,dtend,all_day,recurring,summary,json)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(owner,calendar_id,uid) DO UPDATE SET
		  etag=excluded.etag, dtstart=excluded.dtstart, dtend=excluded.dtend,
		  all_day=excluded.all_day, recurring=excluded.recurring, summary=excluded.summary, json=excluded.json`,
		user, calID, uid, etag, dtstart, dtend, b2i(ev.AllDay), b2i(ev.RRule != ""), ev.Summary, string(evJSON)); err != nil {
		_ = tx.Rollback()
		return err
	}
	seq, err := s.logChange(tx, user, calID, uid, "put", etag, now)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.emit(Change{Seq: seq, Owner: user, CalendarID: calID, UID: uid, Type: "put", ETag: etag, At: now})
	return nil
}

// indexDelete removes one event from the index and appends a "delete" tombstone, in a single
// transaction (symmetry with indexUpsert), then broadcasts. It does NOT touch the .ics file;
// callers remove the file only after this returns nil. Returns ErrNotFound when no row matched.
func (s *Store) indexDelete(user, calID, uid string) error {
	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM events WHERE owner=? AND calendar_id=? AND uid=?`, user, calID, uid)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_ = tx.Rollback()
		return ErrNotFound
	}
	seq, err := s.logChange(tx, user, calID, uid, "delete", "", now)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.emit(Change{Seq: seq, Owner: user, CalendarID: calID, UID: uid, Type: "delete", At: now})
	return nil
}

// indexRange maps an event to its queryable [dtstart, dtend] window. An event whose DTSTART
// could not be parsed (e.g. a non-IANA TZID we cannot resolve) is stored verbatim but indexed
// with a maximally-wide span, so it still surfaces in every time-range query instead of being
// silently filed at year 1 and hidden (plan: all events stay discoverable).
func indexRange(ev *event.Event) (int64, int64) {
	if ev.Start.IsZero() {
		return math.MinInt64 / 2, math.MaxInt64 / 2
	}
	end := ev.End
	if end.IsZero() {
		end = ev.Start.Add(ev.Duration())
	}
	return ev.Start.Unix(), end.Unix()
}

// GetEvent returns the stored event (decoded from the index JSON) and its ETag.
func (s *Store) GetEvent(user, calID, uid string) (*event.Event, string, error) {
	var etag, j string
	err := s.db.QueryRow(`SELECT etag,json FROM events WHERE owner=? AND calendar_id=? AND uid=?`,
		user, calID, uid).Scan(&etag, &j)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	var ev event.Event
	if err := json.Unmarshal([]byte(j), &ev); err != nil {
		return nil, "", err
	}
	return &ev, etag, nil
}

// FindEventByUID locates an event by UID across all of the user's calendars, returning the
// event, its calendar id and ETag. Used by scheduling to match inbound iTIP REPLY/CANCEL.
func (s *Store) FindEventByUID(user, uid string) (*event.Event, string, string, error) {
	var calID, etag, j string
	err := s.db.QueryRow(`SELECT calendar_id, etag, json FROM events WHERE owner=? AND uid=? LIMIT 1`, user, uid).
		Scan(&calID, &etag, &j)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", "", ErrNotFound
	}
	if err != nil {
		return nil, "", "", err
	}
	var ev event.Event
	if err := json.Unmarshal([]byte(j), &ev); err != nil {
		return nil, "", "", err
	}
	return &ev, calID, etag, nil
}

// ListEvents returns the event instances overlapping [from, to): non-recurring events whose
// span intersects the window, plus expanded occurrences of recurring masters (each a clone
// with Start/End and RecurrenceID set to the occurrence).
func (s *Store) ListEvents(user, calID string, from, to time.Time) ([]*event.Event, error) {
	if err := s.ensureHome(user); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT json, recurring FROM events WHERE owner=? AND calendar_id=?`, user, calID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var masters []*event.Event
	var recurring []*event.Event
	for rows.Next() {
		var j string
		var rec int
		if err := rows.Scan(&j, &rec); err != nil {
			return nil, err
		}
		var ev event.Event
		if json.Unmarshal([]byte(j), &ev) != nil {
			continue
		}
		if rec == 1 {
			recurring = append(recurring, &ev)
		} else {
			masters = append(masters, &ev)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []*event.Event
	for _, ev := range masters {
		end := ev.End
		if end.IsZero() {
			end = ev.Start.Add(ev.Duration())
		}
		if ev.Start.Before(to) && end.After(from) {
			out = append(out, ev)
		}
	}
	for _, ev := range recurring {
		b, err := os.ReadFile(s.icsPath(user, calID, ev.UID))
		if err != nil {
			continue
		}
		// Expand from the .ics so RECURRENCE-ID overrides (per-occurrence edits), EXDATE deletions
		// and an UNTIL-truncated tail are all reflected — not just the bare RRULE.
		insts, err := ical.ExpandSeries(b, from, to)
		if err != nil {
			continue
		}
		out = append(out, insts...)
	}
	return out, nil
}

// DeleteEvent removes an event, recording a tombstone in the change-log. The source-of-truth
// .ics file is removed BEFORE the index+tombstone transaction, so a crash (or a failed removal)
// between the two steps leaves the reconciler converging toward "deleted" — an index row with no
// file is tombstoned — instead of resurrecting the event, which is what a leftover file with no
// index row would do (reconcile re-indexes such a file as a "put"). This mirrors the write path,
// which writes the file before the index. A genuine removal failure aborts the delete with the
// event still fully intact, rather than silently committing a tombstone that cannot be undone.
func (s *Store) DeleteEvent(user, calID, uid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.calOwned(user, calID) {
		return ErrNotFound
	}
	if err := s.removeFile(s.icsPath(user, calID, uid)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return s.indexDelete(user, calID, uid)
}

// ── recurring-series scoped edits (Google-Calendar-style: this / this+following / all) ──
// Each reads the verbatim .ics, transforms the VEVENT set via the ical package, and writes it
// back through PutRaw (verbatim + reindex + change-log + push). RawEvent/PutRaw do their own
// locking, so these wrappers must NOT hold s.mu (it is a non-reentrant mutex).

// editSeries is the read-modify-write spine for the scoped recurrence edits. It transforms the
// verbatim .ics via fn and writes it back with If-Match against the bytes it read, retrying a few
// times on a concurrent write so two simultaneous scoped edits cannot silently lose one another.
func (s *Store) editSeries(user, calID, uid string, fn func([]byte) ([]byte, error)) error {
	for attempt := 0; attempt < 4; attempt++ {
		b, etag, err := s.RawEvent(user, calID, uid)
		if err != nil {
			return err
		}
		nb, err := fn(b)
		if err != nil {
			return err
		}
		if _, _, err = s.PutRaw(user, calID, uid, nb, etag, ""); errors.Is(err, ErrPreconditionFailed) {
			continue // someone else wrote between our read and write — reload and retry
		} else {
			return err
		}
	}
	return ErrPreconditionFailed
}

// OverrideOccurrence applies a "this event only" edit: a RECURRENCE-ID exception carrying
// edited's single-instance data, on top of the unchanged master rule. edited.RecurrenceID must be
// the occurrence's original start.
func (s *Store) OverrideOccurrence(user, calID, uid string, edited *event.Event) error {
	return s.editSeries(user, calID, uid, func(b []byte) ([]byte, error) { return ical.Override(b, edited) })
}

// ExcludeOccurrence applies a "this event only" delete: EXDATEs the occurrence out of the series.
func (s *Store) ExcludeOccurrence(user, calID, uid string, occ time.Time) error {
	return s.editSeries(user, calID, uid, func(b []byte) ([]byte, error) { return ical.Exclude(b, occ) })
}

// TruncateSeries ends the series just before occ (the "this and following" delete, and the master
// half of a this-and-following edit which is paired with a new series from occ onward).
func (s *Store) TruncateSeries(user, calID, uid string, occ time.Time) error {
	return s.editSeries(user, calID, uid, func(b []byte) ([]byte, error) { return ical.Truncate(b, occ.Add(-time.Second)) })
}

// ReplaceMaster rewrites the series master from `master` (which must carry the preserved RRULE and
// EXDATEs) while keeping every RECURRENCE-ID override — the "all events" edit, byte-faithful to
// the rest of the series rather than re-encoding only the master model (which would drop them).
func (s *Store) ReplaceMaster(user, calID, uid string, master *event.Event) error {
	return s.editSeries(user, calID, uid, func(b []byte) ([]byte, error) { return ical.ReplaceMaster(b, master) })
}

// SeriesTailRule returns the RRULE a this-and-following split should give the new series.
func (s *Store) SeriesTailRule(user, calID, uid string, occ time.Time) (string, error) {
	b, _, err := s.RawEvent(user, calID, uid)
	if err != nil {
		return "", err
	}
	return ical.TailRule(b, occ)
}

// MasterStart returns the series master's DTSTART, so a whole-series ("all") edit made from an
// occurrence can shift the master by the same delta the user applied to that occurrence.
func (s *Store) MasterStart(user, calID, uid string) (time.Time, error) {
	b, _, err := s.RawEvent(user, calID, uid)
	if err != nil {
		return time.Time{}, err
	}
	return ical.MasterStart(b)
}

// logChange appends one change-log row, advances the calendar's ctag to the new seq, and
// returns that seq (the live-push / sync spine).
func (s *Store) logChange(tx *sql.Tx, user, calID, uid, typ, etag string, at time.Time) (int64, error) {
	res, err := tx.Exec(`INSERT INTO changelog(owner,calendar_id,uid,change_type,etag,at) VALUES(?,?,?,?,?,?)`,
		user, calID, uid, typ, etag, at.Unix())
	if err != nil {
		return 0, err
	}
	seq, _ := res.LastInsertId()
	if _, err := tx.Exec(`UPDATE calendars SET ctag=?, updated=? WHERE owner=? AND id=?`,
		strconv.FormatInt(seq, 10), at.Unix(), user, calID); err != nil {
		return 0, err
	}
	return seq, nil
}

// MaxSeq returns the highest change-log seq across all of the user's calendars (0 if none).
// It seeds the SSE "hello" frame so a client knows the live position before subscribing.
func (s *Store) MaxSeq(user string) (int64, error) {
	var seq sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(seq) FROM changelog WHERE owner=?`, user).Scan(&seq); err != nil {
		return 0, err
	}
	return seq.Int64, nil
}

// ChangesSince returns the user's change-log entries with seq > since, oldest first. It backs
// SSE reconnect replay (Last-Event-ID) so a client misses nothing across a dropped stream.
func (s *Store) ChangesSince(user string, since int64) ([]Change, error) {
	rows, err := s.db.Query(`SELECT seq,calendar_id,uid,change_type,etag,at FROM changelog
		WHERE owner=? AND seq>? ORDER BY seq`, user, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		var at int64
		if err := rows.Scan(&c.Seq, &c.CalendarID, &c.UID, &c.Type, &c.ETag, &at); err != nil {
			return nil, err
		}
		c.Owner, c.At = user, time.Unix(at, 0).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

// CalendarByFeedToken resolves a webcal/ICS feed capability token to its calendar (with Owner
// set). The token is the sole credential for the read-only feed, so the lookup is unscoped.
func (s *Store) CalendarByFeedToken(token string) (event.Calendar, error) {
	if token == "" {
		return event.Calendar{}, ErrNotFound
	}
	var c event.Calendar
	var ro, pub int
	err := s.db.QueryRow(`SELECT id,owner,kind,name,description,color,timezone,ord,readonly,public,ctag,feed_token
		FROM calendars WHERE feed_token=?`, token).
		Scan(&c.ID, &c.Owner, &c.Kind, &c.Name, &c.Description, &c.Color, &c.TimeZone, &c.Order, &ro, &pub, &c.CTag, &c.FeedToken)
	if errors.Is(err, sql.ErrNoRows) {
		return event.Calendar{}, ErrNotFound
	}
	if err != nil {
		return event.Calendar{}, err
	}
	c.ReadOnly, c.Public = ro == 1, pub == 1
	return c, nil
}

// RawEvents returns the verbatim stored .ics bytes of every event in a calendar. These are the
// single source of truth (never re-encoded), so the ICS feed and export stay byte-faithful.
func (s *Store) RawEvents(user, calID string) ([][]byte, error) {
	// Hold the write lock so the whole-calendar scan is a consistent snapshot: a concurrent
	// PutRaw/DeleteEvent (which rename/remove files under this same lock) cannot make the feed or
	// export observe a half-applied write — one event's new bytes but not another's, or a file
	// caught mid-delete. Mirrors SyncCollection, which likewise snapshots under s.mu.
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.calOwned(user, calID) {
		return nil, ErrNotFound
	}
	entries, err := os.ReadDir(s.calDir(user, calID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out [][]byte
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".ics" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.calDir(user, calID), e.Name()))
		if err != nil {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// ── CalDAV support (Phase 2): verbatim objects, ETag preconditions, sync-token ──────────

// ObjectMeta lists one stored event resource for CalDAV PROPFIND / calendar-query: the resource
// name is "<UID>.ics", ETag is the strong validator (hash of the stored bytes), and Start/End/
// Recurring back the calendar-query time-range filter without reading the .ics file.
type ObjectMeta struct {
	UID       string
	ETag      string
	Start     time.Time
	End       time.Time
	Recurring bool
}

// SyncChange is one entry of a WebDAV-Sync (RFC 6578) delta: a put carries the current ETag,
// a delete is a tombstone (empty ETag), deduplicated to the latest state per resource.
type SyncChange struct {
	UID     string
	ETag    string
	Deleted bool
}

// ErrPreconditionFailed signals an If-Match / If-None-Match mismatch (caller answers HTTP 412).
var ErrPreconditionFailed = errors.New("precondition failed")

// ErrSyncTokenTooOld signals a sync-token older than the retained change-log floor; the caller
// must answer DAV:valid-sync-token (HTTP 409) and force a full resync (plan M2).
var ErrSyncTokenTooOld = errors.New("sync token too old")

// ErrInvalidCalendar signals that a PutRaw body could not be parsed as iCalendar — distinct from
// infrastructure errors so the CalDAV layer can answer the valid-calendar-data precondition (403)
// only for genuinely bad data, not for disk/transaction failures (which must surface as 5xx).
var ErrInvalidCalendar = errors.New("invalid calendar data")

// RawEvent returns the verbatim stored .ics bytes of one event plus its ETag. The bytes are the
// single source of truth — never re-encoded — so a CalDAV GET is byte-identical to the client's
// PUT and its hash equals the returned ETag (plan B1: no resync loops, no X-prop data loss).
func (s *Store) RawEvent(user, calID, uid string) ([]byte, string, error) {
	if !s.calOwned(user, calID) {
		return nil, "", ErrNotFound
	}
	var etag string
	err := s.db.QueryRow(`SELECT etag FROM events WHERE owner=? AND calendar_id=? AND uid=?`, user, calID, uid).Scan(&etag)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	b, err := os.ReadFile(s.icsPath(user, calID, uid))
	if err != nil {
		return nil, "", ErrNotFound
	}
	// The ETag is the hash of the bytes actually served — not the index's cached value — so the
	// strong validator always matches the response body even if the index lags a concurrent
	// write or a crash left them momentarily out of step (plan B1). _ = etag (index existence only).
	_ = etag
	return b, etagOf(b), nil
}

// EventMetas lists the resource name + ETag of every event in a calendar (PROPFIND depth 1,
// calendar-query without a time filter). Cheap: served from the index, no file reads.
func (s *Store) EventMetas(user, calID string) ([]ObjectMeta, error) {
	if !s.calOwned(user, calID) {
		return nil, ErrNotFound
	}
	rows, err := s.db.Query(`SELECT uid, etag, dtstart, dtend, recurring FROM events WHERE owner=? AND calendar_id=? ORDER BY uid`, user, calID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ObjectMeta
	for rows.Next() {
		var m ObjectMeta
		var ds, de int64
		var rec int
		if err := rows.Scan(&m.UID, &m.ETag, &ds, &de, &rec); err != nil {
			return nil, err
		}
		m.Start, m.End, m.Recurring = time.Unix(ds, 0).UTC(), time.Unix(de, 0).UTC(), rec == 1
		out = append(out, m)
	}
	return out, rows.Err()
}

// PutRaw stores client-supplied iCalendar bytes verbatim (CalDAV PUT). The bytes become the
// source of truth and are hashed for the ETag; the index is refreshed by parsing them for the
// queryable fields only. ifMatch / ifNoneMatch carry the request preconditions: ifNoneMatch "*"
// requires the resource be new, a non-empty ifMatch requires the current ETag to match. The
// returned bool reports whether the resource was newly created (so the caller can answer HTTP
// 201 vs 204).
func (s *Store) PutRaw(user, calID, uid string, raw []byte, ifMatch, ifNoneMatch string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureHome(user); err != nil {
		return "", false, err
	}
	if !s.calOwned(user, calID) {
		return "", false, ErrNotFound
	}
	// Parse for the index (and to reject non-calendar bodies). RECURRENCE-ID overrides are not
	// separately indexed; the verbatim bytes still carry them for a byte-faithful GET. The
	// on-disk filename is a reversible encoding of uid (see encodeName), so any RFC 5545 UID —
	// including the common localpart@domain form — is stored safely without a key-space clash.
	ev, err := ical.Decode(raw)
	if err != nil {
		return "", false, ErrInvalidCalendar
	}
	var curEtag string
	exists := false
	switch err := s.db.QueryRow(`SELECT etag FROM events WHERE owner=? AND calendar_id=? AND uid=?`,
		user, calID, uid).Scan(&curEtag); {
	case err == nil:
		exists = true
	case errors.Is(err, sql.ErrNoRows):
		// new resource
	default:
		return "", false, err
	}
	if ifNoneMatch == "*" && exists {
		return "", false, ErrPreconditionFailed
	}
	if ifMatch != "" && (!exists || !etagMatch(ifMatch, curEtag)) {
		return "", false, ErrPreconditionFailed
	}

	etag := etagOf(raw)
	if err := writeFileAtomic(s.icsPath(user, calID, uid), s.calDir(user, calID), raw); err != nil {
		return "", false, err
	}
	ev.UID, ev.CalendarID = uid, calID
	if err := s.indexUpsert(user, calID, uid, ev, etag); err != nil {
		return "", false, err
	}
	return etag, !exists, nil
}

// DeleteRaw removes an event with an optional If-Match precondition (CalDAV DELETE), recording a
// tombstone in the change-log so sync-collection can report the removal.
func (s *Store) DeleteRaw(user, calID, uid, ifMatch string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.calOwned(user, calID) {
		return ErrNotFound
	}
	var curEtag string
	err := s.db.QueryRow(`SELECT etag FROM events WHERE owner=? AND calendar_id=? AND uid=?`, user, calID, uid).Scan(&curEtag)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if ifMatch != "" && !etagMatch(ifMatch, curEtag) {
		return ErrPreconditionFailed
	}
	// Remove the source file before the index+tombstone transaction (see DeleteEvent): a crash
	// converges toward deleted, and a real removal failure aborts with the event intact rather
	// than committing an un-undoable tombstone. The precondition check above already confirmed the
	// index row exists, so indexDelete (under this same lock) still matches it.
	if err := s.removeFile(s.icsPath(user, calID, uid)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return s.indexDelete(user, calID, uid)
}

// SyncCollection computes the WebDAV-Sync (RFC 6578) delta for a calendar. since is the numeric
// token the client returned (0 = initial sync). It returns the changes, the new token (the
// current ctag) and ErrSyncTokenTooOld when since predates the retained change-log floor —
// forcing the caller to answer 409 + full resync (plan M2). Initial sync enumerates all members.
func (s *Store) SyncCollection(user, calID string, since int64) ([]SyncChange, int64, error) {
	// Hold the write lock so ctag and the change-log rows are read as one snapshot: otherwise a
	// concurrent commit could advance ctag after we read it but before we read its rows, returning
	// a token that claims to cover changes the client never received.
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.calOwned(user, calID) {
		return nil, 0, ErrNotFound
	}
	var minValid int64
	var ctag string
	if err := s.db.QueryRow(`SELECT min_valid_seq, ctag FROM calendars WHERE owner=? AND id=?`, user, calID).
		Scan(&minValid, &ctag); err != nil {
		return nil, 0, err
	}
	newToken, _ := strconv.ParseInt(ctag, 10, 64)
	if since > 0 && since < minValid {
		return nil, 0, ErrSyncTokenTooOld
	}
	if since <= 0 { // initial sync: enumerate current members as puts
		metas, err := s.EventMetas(user, calID)
		if err != nil {
			return nil, 0, err
		}
		out := make([]SyncChange, 0, len(metas))
		for _, m := range metas {
			out = append(out, SyncChange{UID: m.UID, ETag: m.ETag})
		}
		return out, newToken, nil
	}
	rows, err := s.db.Query(`SELECT uid, change_type, etag FROM changelog
		WHERE owner=? AND calendar_id=? AND seq>? ORDER BY seq`, user, calID, since)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	// Collapse multiple log rows per resource to its latest state (a put then delete = delete).
	idx := map[string]int{}
	var out []SyncChange
	for rows.Next() {
		var uid, typ, etag string
		if err := rows.Scan(&uid, &typ, &etag); err != nil {
			return nil, 0, err
		}
		sc := SyncChange{UID: uid, ETag: etag, Deleted: typ == "delete"}
		if i, ok := idx[uid]; ok {
			out[i] = sc
		} else {
			idx[uid] = len(out)
			out = append(out, sc)
		}
	}
	return out, newToken, rows.Err()
}

// Compact trims the change-log to entries at/after `before`, advancing each calendar's
// min_valid_seq so stale sync-tokens are rejected with 409 (plan M2). The floor is the largest
// seq that has been trimmed (and is therefore no longer retrievable) — derived from the seqs
// actually deleted, not from the timestamp ordering — so a backward clock step that trims a
// high seq while lower seqs survive cannot leave a gap that is silently served. min_valid_seq is
// monotonic (max with its previous value). Intended for a periodic maintenance pass.
func (s *Store) Compact(before time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := before.Unix()
	rows, err := s.db.Query(`SELECT owner, id, min_valid_seq FROM calendars`)
	if err != nil {
		return err
	}
	type cal struct {
		owner, id string
		minValid  int64
	}
	var cals []cal
	for rows.Next() {
		var c cal
		if err := rows.Scan(&c.owner, &c.id, &c.minValid); err != nil {
			rows.Close()
			return err
		}
		cals = append(cals, c)
	}
	rows.Close()
	for _, c := range cals {
		var maxDel sql.NullInt64
		if err := s.db.QueryRow(`SELECT MAX(seq) FROM changelog WHERE owner=? AND calendar_id=? AND at<?`,
			c.owner, c.id, cutoff).Scan(&maxDel); err != nil {
			return err
		}
		if !maxDel.Valid {
			continue // nothing to trim for this calendar
		}
		if _, err := s.db.Exec(`DELETE FROM changelog WHERE owner=? AND calendar_id=? AND at<?`,
			c.owner, c.id, cutoff); err != nil {
			return err
		}
		floor := c.minValid
		if maxDel.Int64 > floor {
			floor = maxDel.Int64
		}
		if _, err := s.db.Exec(`UPDATE calendars SET min_valid_seq=? WHERE owner=? AND id=?`, floor, c.owner, c.id); err != nil {
			return err
		}
	}
	return nil
}

// etagMatch reports whether an If-Match / If-None-Match header value satisfies the current strong
// ETag. It accepts "*" (any), a comma-separated list, and tolerates a weak "W/" prefix.
func etagMatch(header, current string) bool {
	for _, tok := range strings.Split(header, ",") {
		tok = strings.TrimSpace(tok)
		tok = strings.TrimPrefix(tok, "W/")
		if tok == "*" || tok == current {
			return true
		}
	}
	return false
}

// ── paths + helpers ───────────────────────────────────────────────────────────────────

func (s *Store) calDir(user, calID string) string {
	return filepath.Join(s.root, "calendars", safeName(user), safeName(calID))
}

func (s *Store) icsPath(user, calID, uid string) string {
	return filepath.Join(s.calDir(user, calID), encodeName(uid)+".ics")
}

// encodeName maps an event UID to its on-disk filename as a single, reversible, collision-free
// path segment: bytes outside the portable set [A-Za-z0-9._-] are percent-encoded (%XX). Unlike
// the lossy safeName, this is a bijection, so the index key (the raw UID, which per RFC 5545 is
// commonly localpart@domain) and the file identity can never diverge — and the reconciler can
// recover the exact UID from a filename. '/' encodes to %2F, so the segment cannot traverse.
func encodeName(uid string) string {
	const hexdig = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(uid); i++ {
		c := uid[i]
		if c == '-' || c == '_' || c == '.' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hexdig[c>>4])
			b.WriteByte(hexdig[c&0x0f])
		}
	}
	return b.String()
}

// decodeName is the inverse of encodeName, recovering the raw UID from a filename stem.
func decodeName(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		if name[i] == '%' && i+2 < len(name) {
			hi, lo := unhex(name[i+1]), unhex(name[i+2])
			if hi >= 0 && lo >= 0 {
				b.WriteByte(byte(hi<<4 | lo))
				i += 2
				continue
			}
		}
		b.WriteByte(name[i])
	}
	return b.String()
}

func unhex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	}
	return -1
}

func writeFileAtomic(path, dir string, b []byte) error {
	tmp := filepath.Join(dir, ".tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(tmp, "ev-*")
	if err != nil {
		return err
	}
	name := f.Name()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func etagOf(b []byte) string {
	h := sha256.Sum256(b)
	return `"` + hex.EncodeToString(h[:])[:32] + `"`
}

func genID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func b2i(v bool) int {
	if v {
		return 1
	}
	return 0
}

// safeName restricts a path segment to [A-Za-z0-9._-]; other bytes become '_'. Our generated
// ids are already safe; this guards imported UIDs / usernames from path traversal.
func safeName(s string) string {
	if s == "" {
		return "_"
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '-' || c == '_' || c == '.' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
