package bus

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// Verify does a lightweight live GET against the endpoint with the zone key to
// confirm the key is accepted for this zone before `essaim join` persists it. A
// 2xx means accepted; a 401/403 (or any non-2xx) surfaces as an error so we
// never persist an unconfirmed/rejected credential. Network failure also errors
// (do not persist a join we could not confirm).
func (c *Client) Verify(ctx context.Context) error {
	url := c.ep.URL
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+sep+"limit=1", nil)
	if err != nil {
		return err
	}
	if c.ep.Key != "" {
		req.Header.Set("X-Aibus-Key", c.ep.Key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("essaim join: could not reach the bus to verify the key — check the endpoint and your connection: %w", friendlyNetErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("essaim join: the bus rejected that key for this zone (%s) — check the key and zone", resp.Status)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("essaim join: unexpected bus response %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// LoadKey reads a raw key from a key file, trimming surrounding whitespace.
// essaim stores only the key-file PATH in config; the raw key stays in its file
// (e.g. ~/.config/essaim/keys/...) and is loaded at use time.
func LoadKey(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("essaim: could not read key file %q: %w", path, err)
	}
	k := strings.TrimSpace(string(b))
	if k == "" {
		return "", fmt.Errorf("essaim: key file %q is empty", path)
	}
	return k, nil
}
