// Package brain is essaim's opt-in client for a zone-scoped rule store (a Brain
// server). It is OFF by default; a binary that never joins a Brain opens no
// socket. When joined, essaim pulls the ZONE's shared rules (the server derives
// and enforces the zone from the key) and writes them into a dedicated fenced
// block in the user's native rules files — kept separate from the user's own
// correction-learned vault. For a trusted user the key is an existing zone Brain
// key; the guest path plugs in later on the same client.
package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Endpoint is where essaim reaches a Brain and with which key. Zone is
// informational only — the server enforces the real zone from the key.
type Endpoint struct {
	URL  string `json:"url,omitempty"`
	Key  string `json:"key,omitempty"`
	Zone string `json:"zone,omitempty"`
}

// Rule is one zone rule (the fields essaim renders).
type Rule struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
	Body  string `json:"body"`
}

// Client talks to a Brain server for a joined endpoint.
type Client struct {
	ep   Endpoint
	http *http.Client
}

// New builds a Client for a resolved endpoint.
func New(ep Endpoint) *Client {
	return &Client{ep: ep, http: &http.Client{Timeout: 15 * time.Second}}
}

// Pull fetches the zone's active rules for a project via GET /api/rules?project=
// with the Brain key in X-Brain-Key. The server derives+enforces the zone from
// the key. A non-2xx (e.g. a 403 zone denial) surfaces as an error — never a
// silent empty pull.
func (c *Client) Pull(ctx context.Context, project string) ([]Rule, error) {
	base := strings.TrimRight(c.ep.URL, "/")
	url := base + "/api/rules?project=" + project + "&limit=500"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if c.ep.Key != "" {
		req.Header.Set("X-Brain-Key", c.ep.Key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("essaim brain pull: %s: %s", resp.Status, strings.TrimSpace(string(rb)))
	}
	var out struct {
		Rules []Rule `json:"rules"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return nil, fmt.Errorf("essaim brain pull: bad response: %w", err)
	}
	return out.Rules, nil
}
