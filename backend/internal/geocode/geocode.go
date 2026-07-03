// Package geocode is icaly's server-side place-search proxy for the event Location picker. The
// browser may not call external APIs (the service-UI lockdown forbids fetch), so the UI hits
// icaly's own /geocode endpoint and this package talks to the upstream provider — keeping any
// API key server-side. It is provider-agnostic: Photon (OpenStreetMap, free, no key) is the
// default, and Google Places (New) is used automatically when an API key is configured. Results
// are cached so repeated queries cost nothing upstream.
package geocode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrDisabled means no provider is configured (the UI then falls back to free-text location).
var ErrDisabled = errors.New("geocoding disabled")

// ErrBudgetExceeded means the instance-wide daily upstream-call cap is reached; the caller should
// degrade gracefully (free-text). It bounds cost on a shared billed key (and is courteous to the
// free Photon instance) independently of any per-user rate limit.
var ErrBudgetExceeded = errors.New("geocoding daily budget exceeded")

// errResolveUnsupported is returned by providers whose suggestions are already fully resolved
// (Photon), so Resolve should never be called for them.
var errResolveUnsupported = errors.New("provider resolves suggestions inline")

const (
	photonEndpoint = "https://photon.komoot.io/api/"
	googleBase     = "https://places.googleapis.com"
	userAgent      = "icaly/0.1 (holistic calendar; +https://github.com/holistic)"
	cacheTTL       = time.Hour
	cacheCap       = 2048
	httpTimeout    = 6 * time.Second
)

// Suggestion is one autocomplete result. For Photon, Resolved is true and Address/Lat/Lon are
// already filled (no second call needed); for Google, Resolved is false and the caller must
// Resolve(ID) on selection to obtain the address + coordinates.
type Suggestion struct {
	ID        string  `json:"id"`
	Label     string  `json:"label"`     // full text to put into the LOCATION field
	Primary   string  `json:"primary"`   // main line (name / street) for display
	Secondary string  `json:"secondary"` // secondary line (rest of the address) for display
	Resolved  bool    `json:"resolved"`
	Address   string  `json:"address,omitempty"`
	Lat       float64 `json:"lat,omitempty"`
	Lon       float64 `json:"lon,omitempty"`
}

// Place is a fully resolved location.
type Place struct {
	Name    string  `json:"name"`
	Address string  `json:"address"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
}

// Provider is one geocoding backend.
type Provider interface {
	Name() string
	Suggest(ctx context.Context, input, session string) ([]Suggestion, error)
	Resolve(ctx context.Context, id, session string) (Place, error)
}

// Service wraps a Provider with a small TTL cache (suggestions by query, places by id) and an
// instance-wide daily upstream-call budget (a hard cost ceiling on a shared billed key).
type Service struct {
	p        Provider
	mu       sync.Mutex
	sugg     map[string]suggEntry
	place    map[string]placeEntry
	dailyCap int   // <=0 disables the cap
	day      int64 // UTC day number of the current budget window
	spent    int   // upstream calls made in the current window
}

type suggEntry struct {
	at time.Time
	v  []Suggestion
}
type placeEntry struct {
	at time.Time
	v  Place
}

// New selects a provider. provider may be "", "photon", "google" or "none". With "" (auto), a
// non-empty key picks Google, otherwise Photon. "google" without a key falls back to Photon so
// the picker still works until the key is provisioned. "none" disables the feature.
func New(provider, key string, dailyCap int) *Service {
	client := &http.Client{Timeout: httpTimeout}
	ph := &photon{client: client, endpoint: photonEndpoint}
	gg := func() Provider { return &google{key: key, client: client, base: googleBase} }
	var p Provider
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "none":
		p = nil
	case "photon":
		p = ph
	case "google":
		if key != "" {
			p = gg()
		} else {
			p = ph
		}
	default: // auto
		if key != "" {
			p = gg()
		} else {
			p = ph
		}
	}
	s := NewWith(p)
	s.dailyCap = dailyCap
	return s
}

// NewWith wraps a caller-supplied Provider (nil = disabled). It exists mainly so tests can inject
// a fake or an httptest-backed provider without reaching the real upstreams. No budget cap.
func NewWith(p Provider) *Service {
	return &Service{p: p, sugg: map[string]suggEntry{}, place: map[string]placeEntry{}}
}

// charge accounts one upstream call against the daily budget, returning false when the cap is
// reached. cap<=0 means unlimited. The window is a UTC calendar day (matching provider quotas).
func (s *Service) charge() bool {
	if s.dailyCap <= 0 {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	day := time.Now().UTC().Unix() / 86400
	if day != s.day {
		s.day, s.spent = day, 0
	}
	if s.spent >= s.dailyCap {
		return false
	}
	s.spent++
	return true
}

// Enabled reports whether a provider is configured.
func (s *Service) Enabled() bool { return s != nil && s.p != nil }

// Provider returns the active provider name ("" when disabled).
func (s *Service) Provider() string {
	if s.Enabled() {
		return s.p.Name()
	}
	return ""
}

// Suggest returns autocomplete results for input, served from cache when fresh.
func (s *Service) Suggest(ctx context.Context, input, session string) ([]Suggestion, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	key := strings.ToLower(strings.TrimSpace(input))
	s.mu.Lock()
	e, ok := s.sugg[key]
	s.mu.Unlock()
	if ok && time.Since(e.at) < cacheTTL {
		return e.v, nil // cache hits never consume the upstream budget
	}
	if !s.charge() {
		return nil, ErrBudgetExceeded
	}
	v, err := s.p.Suggest(ctx, input, session)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if len(s.sugg) >= cacheCap {
		s.sugg = map[string]suggEntry{}
	}
	s.sugg[key] = suggEntry{at: time.Now(), v: v}
	s.mu.Unlock()
	return v, nil
}

// Resolve turns a suggestion id into a Place (Google only; Photon suggestions arrive resolved).
func (s *Service) Resolve(ctx context.Context, id, session string) (Place, error) {
	if !s.Enabled() {
		return Place{}, ErrDisabled
	}
	s.mu.Lock()
	e, ok := s.place[id]
	s.mu.Unlock()
	if ok && time.Since(e.at) < cacheTTL {
		return e.v, nil // cache hits never consume the upstream budget
	}
	if !s.charge() {
		return Place{}, ErrBudgetExceeded
	}
	v, err := s.p.Resolve(ctx, id, session)
	if err != nil {
		return Place{}, err
	}
	s.mu.Lock()
	if len(s.place) >= cacheCap {
		s.place = map[string]placeEntry{}
	}
	s.place[id] = placeEntry{at: time.Now(), v: v}
	s.mu.Unlock()
	return v, nil
}

// ── Photon (OpenStreetMap) ──────────────────────────────────────────────────────────

type photon struct {
	client   *http.Client
	endpoint string
}

func (p *photon) Name() string { return "photon" }

type photonResp struct {
	Features []struct {
		Geometry struct {
			Coordinates []float64 `json:"coordinates"` // [lon, lat]
		} `json:"geometry"`
		Properties struct {
			Name        string `json:"name"`
			Street      string `json:"street"`
			HouseNumber string `json:"housenumber"`
			Postcode    string `json:"postcode"`
			City        string `json:"city"`
			State       string `json:"state"`
			Country     string `json:"country"`
		} `json:"properties"`
	} `json:"features"`
}

func (p *photon) Suggest(ctx context.Context, input, _ string) ([]Suggestion, error) {
	u := p.endpoint + "?q=" + url.QueryEscape(input) + "&limit=8"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("photon: %s", resp.Status)
	}
	var pr photonResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&pr); err != nil {
		return nil, err
	}
	out := make([]Suggestion, 0, len(pr.Features))
	for _, f := range pr.Features {
		if len(f.Geometry.Coordinates) != 2 {
			continue
		}
		lon, lat := f.Geometry.Coordinates[0], f.Geometry.Coordinates[1]
		pr := f.Properties
		street := strings.TrimSpace(pr.Street + " " + pr.HouseNumber)
		cityLine := strings.TrimSpace(pr.Postcode + " " + pr.City)
		// Primary = the most specific label; the rest forms the secondary/address lines.
		primary := pr.Name
		if primary == "" {
			primary = street
		}
		if primary == "" {
			primary = pr.City
		}
		var rest []string
		for _, part := range []string{street, cityLine, pr.Country} {
			if part != "" && part != primary {
				rest = append(rest, part)
			}
		}
		secondary := strings.Join(rest, ", ")
		full := primary
		if secondary != "" {
			full = primary + ", " + secondary
		}
		out = append(out, Suggestion{
			Label: full, Primary: primary, Secondary: secondary,
			Resolved: true, Address: full, Lat: lat, Lon: lon,
		})
	}
	return out, nil
}

func (p *photon) Resolve(context.Context, string, string) (Place, error) {
	return Place{}, errResolveUnsupported
}

// ── Google Places API (New) ─────────────────────────────────────────────────────────

type google struct {
	key    string
	client *http.Client
	base   string
}

func (g *google) Name() string { return "google" }

func (g *google) Suggest(ctx context.Context, input, session string) ([]Suggestion, error) {
	body := map[string]string{"input": input}
	if session != "" {
		body["sessionToken"] = session
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		g.base+"/v1/places:autocomplete", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-Api-Key", g.key)
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google autocomplete: %s", resp.Status)
	}
	var ar struct {
		Suggestions []struct {
			PlacePrediction struct {
				PlaceID string `json:"placeId"`
				Text    struct {
					Text string `json:"text"`
				} `json:"text"`
				StructuredFormat struct {
					MainText struct {
						Text string `json:"text"`
					} `json:"mainText"`
					SecondaryText struct {
						Text string `json:"text"`
					} `json:"secondaryText"`
				} `json:"structuredFormat"`
			} `json:"placePrediction"`
		} `json:"suggestions"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ar); err != nil {
		return nil, err
	}
	out := make([]Suggestion, 0, len(ar.Suggestions))
	for _, s := range ar.Suggestions {
		pp := s.PlacePrediction
		if pp.PlaceID == "" {
			continue // not a place prediction (e.g. a query prediction) — skip
		}
		out = append(out, Suggestion{
			ID: pp.PlaceID, Label: pp.Text.Text,
			Primary:   pp.StructuredFormat.MainText.Text,
			Secondary: pp.StructuredFormat.SecondaryText.Text,
			Resolved:  false,
		})
	}
	return out, nil
}

func (g *google) Resolve(ctx context.Context, id, session string) (Place, error) {
	u := g.base + "/v1/places/" + url.PathEscape(id)
	if session != "" {
		u += "?sessionToken=" + url.QueryEscape(session)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Place{}, err
	}
	req.Header.Set("X-Goog-Api-Key", g.key)
	// Billing is the highest SKU tier in the field mask. formattedAddress + location are Place
	// Details ESSENTIALS (~$0.005/session); displayName would be PRO (~$0.017). We omit
	// displayName because the autocomplete suggestion already carries the place name (its Label),
	// so this keeps each completed pick at the cheaper Essentials tier.
	req.Header.Set("X-Goog-FieldMask", "id,formattedAddress,location")
	resp, err := g.client.Do(req)
	if err != nil {
		return Place{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Place{}, fmt.Errorf("google details: %s", resp.Status)
	}
	var dr struct {
		FormattedAddress string `json:"formattedAddress"`
		Location         struct {
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
		} `json:"location"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&dr); err != nil {
		return Place{}, err
	}
	return Place{
		Address: dr.FormattedAddress,
		Lat:     dr.Location.Latitude,
		Lon:     dr.Location.Longitude,
	}, nil
}
