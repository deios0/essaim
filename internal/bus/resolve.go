// Package bus is oikos's opt-in aibus client (Phase 3). It is OFF by default: a
// binary that never runs `oikos join` opens no socket and stays white/local.
// When joined, oikos speaks aibus to the endpoint from the join, scoped to the
// zone the SERVER derives from the key — the client is never trusted to assert a
// zone. For a trusted user the key is an existing zone key (your key-mint step);
// the guest invite-redeem path plugs in later on top of the same client.
package bus

import "strings"

// Endpoint is where oikos talks to a bus and with which key. Zone is
// informational only — the server enforces the real zone from the key.
type Endpoint struct {
	URL  string `json:"url,omitempty"`
	Key  string `json:"key,omitempty"`
	Zone string `json:"zone,omitempty"`
}

// Resolve returns the bus endpoint oikos should use, applying precedence:
// the AIBUS_URL / AIBUS_KEY environment variables WIN over the stored join
// config (the ADR gotcha — an off-tailnet override or wrong-zone fix travels via
// env and must never be defeated by a stored/hardcoded endpoint). Returns
// ok=false when neither source supplies a URL (default-off: no bus).
func Resolve(getenv func(string) string, stored Endpoint) (Endpoint, bool) {
	out := stored
	if u := strings.TrimSpace(getenv("AIBUS_URL")); u != "" {
		out.URL = u
	}
	if k := strings.TrimSpace(getenv("AIBUS_KEY")); k != "" {
		out.Key = k
	}
	if strings.TrimSpace(out.URL) == "" {
		return Endpoint{}, false
	}
	return out, true
}
