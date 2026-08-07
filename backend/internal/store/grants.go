package store

import (
	"database/sql"
	"errors"
	"time"
)

// Grant is one sharing ACL entry on a calendar: it gives a grantee — a Holistic/OS group
// ("holistic"), a contax personal group ("contax"), or an ad-hoc member set ("adhoc") scoped to
// this one calendar — view or edit access to the owner's calendar. Ad-hoc grants carry their
// internal member usernames in cal_grant_members. PublicExt records that the owner consented to
// expose the read-only feed link so external (non-account) contacts in the grant can subscribe.
type Grant struct {
	ID        string    `json:"id"`
	Owner     string    `json:"-"`
	CalID     string    `json:"-"`
	Kind      string    `json:"kind"`  // holistic | contax | adhoc
	Ref       string    `json:"ref"`   // holistic: OS group name; contax: grp-id; adhoc: ""
	Label     string    `json:"label"` // display label
	Level     string    `json:"level"` // view | edit
	PublicExt bool      `json:"publicExt"`
	Members   []string  `json:"members,omitempty"` // ad-hoc: internal usernames
	Created   time.Time `json:"created"`
}

var errBadGrant = errors.New("invalid grant")

func validKind(k string) bool  { return k == "holistic" || k == "contax" || k == "adhoc" }
func validLevel(l string) bool { return l == "view" || l == "edit" }

// AddGrant records a new sharing grant on the owner's calendar. For an ad-hoc grant, members are the
// internal usernames the set contains (ignored for holistic/contax). Returns ErrNotFound for a
// calendar the user does not own and errBadGrant for a malformed kind/level.
func (s *Store) AddGrant(owner, calID, kind, ref, label, level string, publicExt bool, members []string) (Grant, error) {
	if !validKind(kind) || !validLevel(level) {
		return Grant{}, errBadGrant
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.calOwned(owner, calID) {
		return Grant{}, ErrNotFound
	}
	g := Grant{
		ID: genID(8), Owner: owner, CalID: calID, Kind: kind, Ref: ref,
		Label: label, Level: level, PublicExt: publicExt, Created: time.Now().UTC(),
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Grant{}, err
	}
	if _, err := tx.Exec(`INSERT INTO cal_grants(id,owner,cal_id,kind,ref,label,level,public_ext,created)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		g.ID, owner, calID, kind, ref, label, level, b2i(publicExt), g.Created.Unix()); err != nil {
		_ = tx.Rollback()
		return Grant{}, err
	}
	if kind == "adhoc" {
		if err := insertMembers(tx, g.ID, members); err != nil {
			_ = tx.Rollback()
			return Grant{}, err
		}
		g.Members = dedupStrings(members)
	}
	if err := refreshPublicTx(tx, owner, calID); err != nil {
		_ = tx.Rollback()
		return Grant{}, err
	}
	if err := tx.Commit(); err != nil {
		return Grant{}, err
	}
	return g, nil
}

// SetGrant updates a grant's level, external-consent flag and (for ad-hoc grants) member set. A nil
// members slice leaves the member set untouched; a non-nil (possibly empty) slice replaces it.
func (s *Store) SetGrant(owner, calID, grantID, level string, publicExt bool, members []string) error {
	if !validLevel(level) {
		return errBadGrant
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE cal_grants SET level=?, public_ext=? WHERE id=? AND owner=? AND cal_id=?`,
		level, b2i(publicExt), grantID, owner, calID)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_ = tx.Rollback()
		return ErrNotFound
	}
	if members != nil {
		if _, err := tx.Exec(`DELETE FROM cal_grant_members WHERE grant_id=?`, grantID); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := insertMembers(tx, grantID, members); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := refreshPublicTx(tx, owner, calID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// RemoveGrant deletes a grant (and its ad-hoc members) from the owner's calendar.
func (s *Store) RemoveGrant(owner, calID, grantID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM cal_grant_members WHERE grant_id=?`, grantID); err != nil {
		_ = tx.Rollback()
		return err
	}
	res, err := tx.Exec(`DELETE FROM cal_grants WHERE id=? AND owner=? AND cal_id=?`, grantID, owner, calID)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_ = tx.Rollback()
		return ErrNotFound
	}
	if err := refreshPublicTx(tx, owner, calID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// ListGrants returns every grant on the owner's calendar, ad-hoc grants with their member sets.
func (s *Store) ListGrants(owner, calID string) ([]Grant, error) {
	rows, err := s.db.Query(`SELECT id,kind,ref,label,level,public_ext,created FROM cal_grants
		WHERE owner=? AND cal_id=? ORDER BY created`, owner, calID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		var pub int
		var created int64
		if err := rows.Scan(&g.ID, &g.Kind, &g.Ref, &g.Label, &g.Level, &pub, &created); err != nil {
			return nil, err
		}
		g.Owner, g.CalID, g.PublicExt, g.Created = owner, calID, pub == 1, time.Unix(created, 0).UTC()
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Kind == "adhoc" {
			m, err := s.grantMembers(out[i].ID)
			if err != nil {
				return nil, err
			}
			out[i].Members = m
		}
	}
	return out, nil
}

func (s *Store) grantMembers(grantID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT username FROM cal_grant_members WHERE grant_id=? ORDER BY username`, grantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// refreshPublicTx keeps calendars.public in step with external-consent within the caller's
// transaction: public is on iff any grant on the calendar has public_ext set. Folding it into the
// grant mutation's own transaction makes the ACL change and its derived public flag one atomic unit
// — there is no committed state in which a grant exists but the flag disagrees, and no crash window
// between the two writes. The COUNT sees the just-applied grant change because it runs inside tx.
func refreshPublicTx(tx *sql.Tx, owner, calID string) error {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM cal_grants WHERE owner=? AND cal_id=? AND public_ext=1`,
		owner, calID).Scan(&n); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE calendars SET public=? WHERE owner=? AND id=?`, b2i(n > 0), owner, calID)
	return err
}

func insertMembers(tx *sql.Tx, grantID string, members []string) error {
	for _, m := range dedupStrings(members) {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO cal_grant_members(grant_id,username) VALUES(?,?)`, grantID, m); err != nil {
			return err
		}
	}
	return nil
}

func dedupStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
