// Package imip is icaly's only touch-point with the mail service (maild). It is a thin RFC 6047
// adapter: outbound, it POSTs an iTIP message to maild's trusted internal-send endpoint to be
// delivered as text/calendar on the organizer's behalf; inbound, it extracts the text/calendar
// part from a raw RFC 822 message that maild forwards. All iTIP semantics live in
// internal/scheduling — this package only moves bytes.
package imip

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/mail"
	"net/textproto"
	"strings"
	"time"
)

// ErrDisabled is returned when outbound iMIP is attempted but not configured.
var ErrDisabled = errors.New("imip: mail transport not configured")

// SendError is a non-2xx response from maild's internal-send endpoint.
type SendError struct {
	Status int
	Body   string
}

func (e *SendError) Error() string {
	return fmt.Sprintf("imip: maild send failed (%d): %s", e.Status, e.Body)
}

// Client sends iMIP messages through maild's internal-send endpoint.
type Client struct {
	endpoint string // full URL of maild's internal-send
	secret   string
	http     *http.Client
}

// New builds a client. maildBaseURL is e.g. http://127.0.0.1:8775; secret is the shared
// icaly↔maild secret. An empty base URL or secret leaves the client disabled.
func New(maildBaseURL, secret string) *Client {
	c := &Client{secret: secret, http: &http.Client{Timeout: 15 * time.Second}}
	if base := strings.TrimRight(strings.TrimSpace(maildBaseURL), "/"); base != "" {
		c.endpoint = base + "/api/services/mail/internal/send"
	}
	return c
}

// Enabled reports whether outbound iMIP is configured.
func (c *Client) Enabled() bool { return c.endpoint != "" && c.secret != "" }

// SendInput is one outbound iMIP message.
type SendInput struct {
	FromUser string // local Holistic user the mail is sent on behalf of (the organizer)
	To       []string
	Subject  string
	Body     string
	ICS      string
	Method   string // REQUEST | REPLY | CANCEL
}

// Send delivers an iMIP message via maild. Returns an error if disabled or the call fails.
func (c *Client) Send(in SendInput) error {
	if !c.Enabled() {
		return ErrDisabled
	}
	payload, _ := json.Marshal(map[string]any{
		"from":           in.FromUser,
		"to":             in.To,
		"subject":        in.Subject,
		"body":           in.Body,
		"calendarIcs":    in.ICS,
		"calendarMethod": in.Method,
	})
	req, err := http.NewRequest(http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Mail-Internal-Secret", c.secret)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &SendError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	return nil
}

// ExtractCalendar walks a raw RFC 822 message and returns the first text/calendar part decoded
// to a string (handling base64/quoted-printable and nested multipart). Returns "" if none.
func ExtractCalendar(raw []byte) string {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	body, _ := io.ReadAll(io.LimitReader(m.Body, 25<<20))
	return walkForCalendar(textproto.MIMEHeader(m.Header), body, 0)
}

func walkForCalendar(hdr textproto.MIMEHeader, data []byte, depth int) string {
	if depth > 12 {
		return ""
	}
	mediatype, params, err := mime.ParseMediaType(hdr.Get("Content-Type"))
	if err != nil || mediatype == "" {
		mediatype = "text/plain"
	}
	if strings.HasPrefix(mediatype, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return ""
		}
		mr := multipart.NewReader(bytes.NewReader(data), boundary)
		for {
			part, err := mr.NextPart()
			if err != nil {
				return ""
			}
			pdata, _ := io.ReadAll(io.LimitReader(part, 25<<20))
			if found := walkForCalendar(part.Header, pdata, depth+1); found != "" {
				return found
			}
		}
	}
	if strings.HasPrefix(mediatype, "text/calendar") || mediatype == "application/ics" {
		return string(decodeCTE(hdr.Get("Content-Transfer-Encoding"), data))
	}
	return ""
}

func decodeCTE(cte string, data []byte) []byte {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "base64":
		if out, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(data)), "")); err == nil {
			return out
		}
	case "quoted-printable":
		// quoted-printable is rare for calendars; fall through to raw on any oddity.
	}
	return data
}
