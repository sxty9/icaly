package store

import (
	"strings"

	"icaly/internal/event"
)

// Access is a principal's resolved per-calendar access level: "" (none), "view", or "edit".
type Access string

const (
	AccessNone Access = ""
	AccessView Access = "view"
	AccessEdit Access = "edit"
)

// GroupChecker resolves live contax personal-group membership. The store holds it as an interface
// so it never imports the contax client (layering); a nil checker makes contax grants inert (the
// Phase-A default, before the contax internal endpoint exists).
type GroupChecker interface {
	// ContaxMember reports whether username belongs to contax personal group grpID.
	ContaxMember(grpID, username string) bool
}

// SharedCalendar is a calendar as seen by some accessor: the calendar plus the accessor's resolved
// access. For a calendar the accessor owns, Level is edit, ReadOnly false and SharedBy "". For a
// shared calendar, ReadOnly (the embedded Calendar field) reflects a view-only grant and SharedBy
// names the real owner. The feed token is blanked for shared calendars (owner-only capability).
type SharedCalendar struct {
	event.Calendar
	Level    Access `json:"-"`
	SharedBy string `json:"sharedBy,omitempty"`
}

// AccessLevel resolves what access (username, groups, isAdmin) has to the calendar (owner, calID).
// The owner — and any admin — always gets edit. Otherwise the highest matching grant wins: a
// holistic grant matches an OS group in `groups`, an ad-hoc grant matches a stored member row, a
// contax grant matches live membership via gc (nil gc ⇒ contax grants never match).
func (s *Store) AccessLevel(username string, groups []string, isAdmin bool, owner, calID string, gc GroupChecker) Access {
	if owner == username {
		// Own calendars: always edit. The store provisions "personal" on first use and returns
		// ErrNotFound for a genuinely unknown id, so gating on calOwned here would 404 a first write.
		return AccessEdit
	}
	if isAdmin {
		if s.calOwned(owner, calID) {
			return AccessEdit
		}
		return AccessNone
	}
	// Materialise grant rows before any nested lookup: the index runs on a single connection, so
	// memberExists/ContaxMember must not fire while this iterator is open.
	type gr struct{ id, kind, ref, level string }
	var grants []gr
	rows, err := s.db.Query(`SELECT id,kind,ref,level FROM cal_grants WHERE owner=? AND cal_id=?`, owner, calID)
	if err != nil {
		return AccessNone
	}
	for rows.Next() {
		var g gr
		if rows.Scan(&g.id, &g.kind, &g.ref, &g.level) == nil {
			grants = append(grants, g)
		}
	}
	rows.Close()

	groupSet := sliceSet(groups)
	best := AccessNone
	for _, g := range grants {
		granted := false
		switch g.kind {
		case "holistic":
			granted = groupSet[g.ref]
		case "adhoc":
			granted = s.memberExists(g.id, username)
		case "contax":
			granted = gc != nil && gc.ContaxMember(g.ref, username)
		}
		if granted {
			best = maxAccess(best, Access(g.level))
		}
	}
	return best
}

// CalendarsFor returns the accessor's own calendars (level edit) unioned with every calendar shared
// to them (real owner, computed level, SharedBy set, view-only marked ReadOnly). It scans the grant
// table by the accessor's identity (groups / ad-hoc membership / live contax) rather than probing
// every calendar, so cost is bounded by the number of grants that could match this user.
func (s *Store) CalendarsFor(username string, groups []string, gc GroupChecker) ([]SharedCalendar, error) {
	own, err := s.Calendars(username)
	if err != nil {
		return nil, err
	}
	out := make([]SharedCalendar, 0, len(own))
	seen := make(map[string]bool)
	for _, c := range own {
		out = append(out, SharedCalendar{Calendar: c, Level: AccessEdit})
		seen[CalKey(c.Owner, c.ID)] = true
	}

	best := map[string]Access{}
	add := func(owner, calID, level string) {
		if owner == username {
			return
		}
		k := CalKey(owner, calID)
		best[k] = maxAccess(best[k], Access(level))
	}

	// holistic grants whose group the accessor is in.
	if len(groups) > 0 {
		q := `SELECT owner,cal_id,level FROM cal_grants WHERE kind='holistic' AND ref IN (` + placeholders(len(groups)) + `)`
		args := make([]any, len(groups))
		for i, g := range groups {
			args[i] = g
		}
		if err := s.scanGrantRows(q, args, add); err != nil {
			return nil, err
		}
	}
	// ad-hoc grants the accessor is a member of.
	if err := s.scanGrantRows(
		`SELECT g.owner,g.cal_id,g.level FROM cal_grants g JOIN cal_grant_members m ON m.grant_id=g.id WHERE g.kind='adhoc' AND m.username=?`,
		[]any{username}, add); err != nil {
		return nil, err
	}
	// contax grants: materialise all, then filter by live membership (bounded by #contax grants).
	{
		type cg struct{ owner, calID, level, ref string }
		var cgs []cg
		rows, err := s.db.Query(`SELECT owner,cal_id,level,ref FROM cal_grants WHERE kind='contax'`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var c cg
			if rows.Scan(&c.owner, &c.calID, &c.level, &c.ref) == nil {
				cgs = append(cgs, c)
			}
		}
		rows.Close()
		for _, c := range cgs {
			if gc != nil && gc.ContaxMember(c.ref, username) {
				add(c.owner, c.calID, c.level)
			}
		}
	}

	for k, level := range best {
		if seen[k] {
			continue
		}
		owner, calID := SplitCalKey(k)
		c, ok := s.calendarRow(owner, calID)
		if !ok {
			continue
		}
		c.FeedToken = "" // capability token stays owner-only
		c.ReadOnly = level == AccessView
		out = append(out, SharedCalendar{Calendar: c, Level: level, SharedBy: owner})
		seen[k] = true
	}
	return out, nil
}

func (s *Store) memberExists(grantID, username string) bool {
	var n int
	_ = s.db.QueryRow(`SELECT 1 FROM cal_grant_members WHERE grant_id=? AND username=? LIMIT 1`, grantID, username).Scan(&n)
	return n == 1
}

func (s *Store) calendarRow(owner, calID string) (event.Calendar, bool) {
	var c event.Calendar
	var ro, pub int
	err := s.db.QueryRow(`SELECT id,owner,kind,name,description,color,timezone,ord,readonly,public,ctag,feed_token
		FROM calendars WHERE owner=? AND id=?`, owner, calID).
		Scan(&c.ID, &c.Owner, &c.Kind, &c.Name, &c.Description, &c.Color, &c.TimeZone, &c.Order, &ro, &pub, &c.CTag, &c.FeedToken)
	if err != nil {
		return event.Calendar{}, false
	}
	c.ReadOnly, c.Public = ro == 1, pub == 1
	return c, true
}

func (s *Store) scanGrantRows(q string, args []any, add func(owner, calID, level string)) error {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var owner, calID, level string
		if err := rows.Scan(&owner, &calID, &level); err != nil {
			return err
		}
		add(owner, calID, level)
	}
	return rows.Err()
}

func maxAccess(a, b Access) Access {
	if a == AccessEdit || b == AccessEdit {
		return AccessEdit
	}
	if a == AccessView || b == AccessView {
		return AccessView
	}
	return AccessNone
}

func sliceSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// CalKey is the canonical composite key identifying one calendar across owners: the
// (owner, calID) pair, NUL-joined. It is the single source of truth for this format —
// any package that keys a calendar by (owner, calID) (e.g. the live push hub's access
// set) reuses it rather than re-deriving the separator, so the two can never drift.
func CalKey(owner, calID string) string { return owner + "\x00" + calID }

// SplitCalKey is the inverse of CalKey: it recovers (owner, calID) from a composite key.
// A key without the separator yields (k, "").
func SplitCalKey(k string) (owner, calID string) {
	if i := strings.IndexByte(k, 0); i >= 0 {
		return k[:i], k[i+1:]
	}
	return k, ""
}
