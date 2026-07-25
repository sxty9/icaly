// Package contax is icaly's thin client for the contax contacts service. It resolves a personal
// ("contax-level") contact group's internal member usernames via contax's machine-to-machine
// endpoint (GET internal/groups/{id}/members, shared-secret auth), so calendar sharing can grant
// access to a contax group and keep it live. Membership is cached per group id with a short TTL —
// one fetch answers the question for every user — so an access check never costs a round trip on
// the hot path. A disabled client (no URL or secret) resolves no members, leaving contax-group
// grants inert (holistic/ad-hoc sharing still works). It satisfies store.GroupChecker.
package contax

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ttl bounds how stale a cached membership set may be: a user added to / removed from a contax group
// gains / loses access within this window on the next access check.
const ttl = 90 * time.Second

// Client resolves contax personal-group membership, cached per group id.
type Client struct {
	base   string // contax base, e.g. http://127.0.0.1:8777
	secret string
	http   *http.Client

	mu     sync.Mutex
	cache  map[string]entry
	nowFn  func() time.Time // injectable clock for tests
}

type entry struct {
	members map[string]bool
	at      time.Time
}

// New builds a client. baseURL is e.g. http://127.0.0.1:8777; secret is the shared contax internal
// secret. An empty base URL or secret leaves the client disabled (resolves no members).
func New(baseURL, secret string) *Client {
	return &Client{
		base:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		secret: strings.TrimSpace(secret),
		http:   &http.Client{Timeout: 8 * time.Second},
		cache:  map[string]entry{},
		nowFn:  time.Now,
	}
}

// Enabled reports whether the client is configured to resolve membership.
func (c *Client) Enabled() bool { return c.base != "" && c.secret != "" }

// ContaxMember reports whether username belongs to contax personal group grpID (owner counts as a
// member). Served from a per-group cache; on a miss or a stale entry it fetches from contax. On a
// fetch error a stale entry is reused if present, else membership is denied (fail closed). Satisfies
// store.GroupChecker.
func (c *Client) ContaxMember(grpID, username string) bool {
	if username == "" {
		return false
	}
	set, ok := c.membersOf(grpID)
	return ok && set[username]
}

// Members returns the usernames in contax personal group grpID (owner + internal members), served
// from the same cache as ContaxMember. ok is false when the group cannot be resolved (disabled
// client, unknown group, or a transient error with no cached value). Used to notify internal
// grantees when a calendar is shared with a contax group.
func (c *Client) Members(grpID string) ([]string, bool) {
	set, ok := c.membersOf(grpID)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(set))
	for u := range set {
		out = append(out, u)
	}
	return out, true
}

// membersOf returns the cached (or freshly fetched) membership set for a group. On a fetch error a
// stale set is reused if present, else (nil, false).
func (c *Client) membersOf(grpID string) (map[string]bool, bool) {
	if !c.Enabled() || grpID == "" {
		return nil, false
	}
	c.mu.Lock()
	e, ok := c.cache[grpID]
	fresh := ok && c.nowFn().Sub(e.at) < ttl
	c.mu.Unlock()
	if fresh {
		return e.members, true
	}
	members, err := c.fetch(grpID)
	if err != nil {
		if ok {
			return e.members, true // serve stale rather than drop on a transient error
		}
		return nil, false
	}
	c.mu.Lock()
	c.cache[grpID] = entry{members: members, at: c.nowFn()}
	c.mu.Unlock()
	return members, true
}

func (c *Client) fetch(grpID string) (map[string]bool, error) {
	endpoint := c.base + "/api/services/contax/internal/groups/" + url.PathEscape(grpID) + "/members"
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Contax-Internal-Secret", c.secret)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return nil, &httpError{resp.StatusCode}
	}
	var body struct {
		Usernames []string `json:"usernames"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	m := make(map[string]bool, len(body.Usernames))
	for _, u := range body.Usernames {
		m[u] = true
	}
	return m, nil
}

type httpError struct{ code int }

func (e *httpError) Error() string { return "contax: unexpected status" }
