// Package notify is icaly's thin client for the holistic notification service (notifyd). It POSTs
// to notify's machine-to-machine ingest endpoint (POST internal/emit) with the shared secret, so
// icaly can raise an in-app / desktop notification on a user's behalf — used by the reminder
// scheduler to fire VALARMs. A disabled client (no URL or secret) makes Emit a silent no-op, so a
// deploy without notify still runs (reminders are simply not delivered).
package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client posts notifications to notifyd's internal endpoint.
type Client struct {
	endpoint string
	secret   string
	http     *http.Client
}

// New builds a client. baseURL is e.g. http://127.0.0.1:8778; secret is the shared notify emit
// secret. An empty base URL or secret leaves the client disabled.
func New(baseURL, secret string) *Client {
	c := &Client{secret: strings.TrimSpace(secret), http: &http.Client{Timeout: 10 * time.Second}}
	if b := strings.TrimRight(strings.TrimSpace(baseURL), "/"); b != "" {
		c.endpoint = b + "/api/services/notify/internal/emit"
	}
	return c
}

// Enabled reports whether the client is configured to send.
func (c *Client) Enabled() bool { return c.endpoint != "" && c.secret != "" }

// EmitInput is one notification to raise for a user.
type EmitInput struct {
	User    string
	Service string
	Title   string
	Body    string
	URL     string
	Icon    string
	Level   string
	Dedupe  string
}

// Emit posts one notification. It is a no-op when the client is disabled.
func (c *Client) Emit(in EmitInput) error {
	if !c.Enabled() {
		return nil
	}
	payload, _ := json.Marshal(map[string]string{
		"user":    in.User,
		"service": in.Service,
		"title":   in.Title,
		"body":    in.Body,
		"url":     in.URL,
		"icon":    in.Icon,
		"level":   in.Level,
		"dedupe":  in.Dedupe,
	})
	req, err := http.NewRequest(http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Notify-Internal-Secret", c.secret)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("notify emit: status %d", resp.StatusCode)
	}
	return nil
}
