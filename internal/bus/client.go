package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is oikos's minimal aibus client for a joined endpoint. It publishes and
// (later) subscribes over the endpoint from the join, presenting the zone key in
// the X-Aibus-Key header — the server derives and enforces the zone from that
// key; oikos never asserts a zone.
type Client struct {
	ep   Endpoint
	http *http.Client
}

// New builds a Client for a resolved endpoint.
func New(ep Endpoint) *Client {
	return &Client{ep: ep, http: &http.Client{Timeout: 10 * time.Second}}
}

// Publish posts one event {project_id, kind, payload} to the bus with the zone
// key in the header, and returns the server-assigned event id. A non-2xx (e.g. a
// 403 zone-guard denial) is returned as an error — never swallowed into a
// success (a foreign/guest key that is denied must not look like it published).
func (c *Client) Publish(ctx context.Context, project, kind string, payload map[string]any) (int64, error) {
	body, err := json.Marshal(map[string]any{
		"project_id": project,
		"kind":       kind,
		"payload":    payload,
	})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ep.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.ep.Key != "" {
		req.Header.Set("X-Aibus-Key", c.ep.Key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("oikos bus publish: %s: %s", resp.Status, bytes.TrimSpace(rb))
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return 0, fmt.Errorf("oikos bus publish: bad response: %w", err)
	}
	return out.ID, nil
}
