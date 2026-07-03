// Package apppass manages per-user app passwords: long, high-entropy tokens a native mail
// client (JMAP, or a future IMAP layer) uses to authenticate to maild over HTTP Basic auth —
// the holistic session cookie only exists in the browser. Tokens are shown once and stored
// only as SHA-256 hashes (256-bit random tokens make a salt/bcrypt unnecessary), one JSON
// file per user under the maild data root, written atomically (the holistic file convention).
package apppass

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Meta is the non-secret record of one app password.
type Meta struct {
	ID      string    `json:"id"`
	Label   string    `json:"label"`
	Created time.Time `json:"created"`
	hash    string    // hex(sha256(token)); never serialized to clients
}

// metaWire is the on-disk shape (includes the hash).
type metaWire struct {
	ID      string    `json:"id"`
	Label   string    `json:"label"`
	Created time.Time `json:"created"`
	Hash    string    `json:"hash"`
}

// Store is a set of per-user app-password files rooted at one directory.
type Store struct {
	dir string
	mu  sync.Mutex
}

// New returns a store keeping files under dir (e.g. /var/lib/mail/apppasswords).
func New(dir string) *Store {
	_ = os.MkdirAll(dir, 0o700)
	return &Store{dir: dir}
}

func (s *Store) path(user string) string { return filepath.Join(s.dir, user+".json") }

func (s *Store) load(user string) ([]metaWire, error) {
	b, err := os.ReadFile(s.path(user))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []metaWire
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) save(user string, list []metaWire) error {
	b, _ := json.MarshalIndent(list, "", "  ")
	tmp := s.path(user) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(user))
}

// Create mints a new app password, returning the clear-text token (shown once) and its meta.
func (s *Store) Create(user, label string) (string, Meta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.load(user)
	if err != nil {
		return "", Meta{}, err
	}
	var tb [24]byte
	if _, err := rand.Read(tb[:]); err != nil {
		return "", Meta{}, err
	}
	token := hex.EncodeToString(tb[:]) // 48 hex chars
	var ib [6]byte
	_, _ = rand.Read(ib[:])
	sum := sha256.Sum256([]byte(token))
	mw := metaWire{ID: hex.EncodeToString(ib[:]), Label: label, Created: time.Now().UTC(), Hash: hex.EncodeToString(sum[:])}
	list = append(list, mw)
	if err := s.save(user, list); err != nil {
		return "", Meta{}, err
	}
	return token, Meta{ID: mw.ID, Label: mw.Label, Created: mw.Created}, nil
}

// Verify reports whether token is a valid app password for user (constant-time compare).
func (s *Store) Verify(user, token string) bool {
	list, err := s.load(user)
	if err != nil || len(list) == 0 {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	want := hex.EncodeToString(sum[:])
	ok := false
	for _, m := range list {
		if subtle.ConstantTimeCompare([]byte(m.Hash), []byte(want)) == 1 {
			ok = true
		}
	}
	return ok
}

// List returns the non-secret metadata of a user's app passwords.
func (s *Store) List(user string) ([]Meta, error) {
	list, err := s.load(user)
	if err != nil {
		return nil, err
	}
	out := make([]Meta, 0, len(list))
	for _, m := range list {
		out = append(out, Meta{ID: m.ID, Label: m.Label, Created: m.Created})
	}
	return out, nil
}

// Delete removes an app password by id.
func (s *Store) Delete(user, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.load(user)
	if err != nil {
		return err
	}
	out := list[:0]
	for _, m := range list {
		if m.ID != id {
			out = append(out, m)
		}
	}
	return s.save(user, out)
}
