package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// P1 [SETUP CSRF / DNS-REBIND]: the /setup mutation endpoints change durable
// state (provider, vault dir, wired files) and were protected ONLY by
// loopbackOnly (RemoteAddr). A localhost daemon is reachable from any page in the
// victim's browser: a malicious site can POST a simple-content-type body (no CORS
// preflight — decodeJSON ignores Content-Type) or rebind its DNS to 127.0.0.1.
// The guard requires a loopback Host and a same-origin Origin/Sec-Fetch-Site.

func csrfServer(t *testing.T) *Server {
	t.Helper()
	s, _ := setupTestServer(t)
	return s
}

func do(s *Server, req *http.Request) int {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code
}

// A cross-site POST (browser sets Sec-Fetch-Site: cross-site) is refused.
func TestSetupRejectsCrossSiteFetch(t *testing.T) {
	s := csrfServer(t)
	for _, ep := range []string{"/setup/model", "/setup/vault", "/setup/wire"} {
		req := loopbackReq("POST", ep, `{"provider":"local","vault_dir":"/x","tool":"cursor"}`)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		if code := do(s, req); code != http.StatusForbidden {
			t.Fatalf("%s cross-site POST = %d, want 403", ep, code)
		}
	}
}

// A cross-origin POST (Origin is an attacker site) is refused even if it forges
// no Sec-Fetch header.
func TestSetupRejectsForeignOrigin(t *testing.T) {
	s := csrfServer(t)
	req := loopbackReq("POST", "/setup/vault", `{"vault_dir":"/tmp/evil"}`)
	req.Header.Set("Origin", "http://evil.example.com")
	if code := do(s, req); code != http.StatusForbidden {
		t.Fatalf("foreign-Origin POST = %d, want 403", code)
	}
}

// DNS-rebinding: the attacker page's Host is its own domain even though it
// resolves to 127.0.0.1. A non-loopback Host is refused.
func TestSetupRejectsRebindHost(t *testing.T) {
	s := csrfServer(t)
	req := loopbackReq("POST", "/setup/model", `{"provider":"local"}`)
	req.Host = "attacker.example.com:4141"
	if code := do(s, req); code != http.StatusForbidden {
		t.Fatalf("rebind-Host POST = %d, want 403", code)
	}
}

// The legitimate first-run page (same-origin fetch from http://127.0.0.1:4141)
// is allowed through the guard.
func TestSetupAllowsSameOrigin(t *testing.T) {
	s := csrfServer(t)
	req := loopbackReq("POST", "/setup/vault", `{"vault_dir":"/tmp/essaim-vault"}`)
	req.Host = "127.0.0.1:4141"
	req.Header.Set("Origin", "http://127.0.0.1:4141")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if code := do(s, req); code == http.StatusForbidden {
		t.Fatalf("same-origin POST was refused (want it handled), got 403")
	}
}

// A non-browser client (essaim CLI / curl: no Origin, no Sec-Fetch, loopback Host)
// is allowed — the guard targets browsers, not local tooling.
func TestSetupAllowsNonBrowserClient(t *testing.T) {
	s := csrfServer(t)
	req := loopbackReq("POST", "/setup/model", `{"provider":"local"}`)
	req.Host = "localhost:4141"
	if code := do(s, req); code == http.StatusForbidden {
		t.Fatalf("non-browser loopback POST was refused (want it handled), got 403")
	}
}

// Codex review: a DIFFERENT loopback origin (another local port) is still
// cross-origin and must be refused even though its host is loopback.
func TestSetupRejectsCrossPortLoopbackOrigin(t *testing.T) {
	s := csrfServer(t)
	req := loopbackReq("POST", "/setup/vault", `{"vault_dir":"/tmp/x"}`)
	req.Host = "127.0.0.1:4141"
	req.Header.Set("Origin", "http://127.0.0.1:9999") // loopback host, different port
	if code := do(s, req); code != http.StatusForbidden {
		t.Fatalf("cross-port loopback Origin = %d, want 403", code)
	}
}
