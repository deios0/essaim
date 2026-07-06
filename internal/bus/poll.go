package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Event is a minimal decoded bus event — enough for oikos to react to its zone's
// coordination/rule events. The full payload is kept raw for the caller.
type Event struct {
	ID      int64           `json:"id"`
	Kind    string          `json:"kind"`
	From    string          `json:"from,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Poll GETs events with id > since for the joined zone and returns them plus the
// new max id (the next cursor). The zone key is in the header; the SERVER filters
// to the caller's zone. A non-2xx (e.g. 403) surfaces as an error — a denied read
// must never masquerade as an empty poll.
func (c *Client) Poll(ctx context.Context, since int64) ([]Event, int64, error) {
	url := c.ep.URL
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	url = url + sep + "since=" + strconv.FormatInt(since, 10) + "&limit=200"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, since, err
	}
	if c.ep.Key != "" {
		req.Header.Set("X-Aibus-Key", c.ep.Key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, since, friendlyNetErr(err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, since, fmt.Errorf("oikos bus poll: %s: %s", resp.Status, strings.TrimSpace(string(rb)))
	}
	var out struct {
		Events []Event `json:"events"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return nil, since, fmt.Errorf("oikos bus poll: bad response: %w", err)
	}
	maxID := since
	for _, e := range out.Events {
		if e.ID > maxID {
			maxID = e.ID
		}
	}
	return out.Events, maxID, nil
}
