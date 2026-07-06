package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"essaim/internal/upstream"
)

// okUpstream stands up a fake upstream that returns 200 on /v1/models so that a
// 401 in these tests can ONLY come from the middleware gate, never from the
// handler's zero-key path.
func okUpstream(t *testing.T) (*httptest.Server, *upstream.SingleUpstream) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(up.Close)
	return up, &upstream.SingleUpstream{Key: "k", Detect: func(string) bool { return false }}
}

// With --require-token engaged (token set), /v1/* is gated but /health stays open.
func TestRequireTokenGatesV1NotHealth(t *testing.T) {
	up, prov := okUpstream(t)
	s := New("127.0.0.1:4141")
	s.SetToken("secret")
	s.SetProvider(prov)
	s.upstreamBaseOverride = up.URL

	// /health: open
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
	if rec.Code != 200 {
		t.Fatalf("/health should be open, got %d", rec.Code)
	}

	// /v1/models without token: 401 (from the gate)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 401 {
		t.Fatalf("/v1 must require token when gate engaged, got %d", rec.Code)
	}

	// /v1/models with correct token: gate passes through to a 200 handler.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("correct token must pass the gate to a 200 handler, got %d", rec.Code)
	}
}

// Fix #2 [HIGH]: a raw `Authorization: <token>` (no "Bearer " scheme) must be
// REJECTED. strings.TrimPrefix is a no-op when the prefix is absent, so the old
// code accepted a bare token. The okUpstream returns 200, so a 401 here can ONLY
// come from the gate. (Constant-time compare is exercised on the equal path.)
func TestRequireTokenRejectsNonBearerScheme(t *testing.T) {
	up, prov := okUpstream(t)
	s := New("127.0.0.1:4141")
	s.SetToken("secret")
	s.SetProvider(prov)
	s.upstreamBaseOverride = up.URL

	cases := []struct {
		name   string
		header string // "" means no Authorization header at all
		set    bool
		want   int
	}{
		{"raw token without Bearer scheme", "secret", true, http.StatusUnauthorized},
		{"correct Bearer token", "Bearer secret", true, http.StatusOK},
		{"missing header", "", false, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/v1/models", nil)
			if tc.set {
				req.Header.Set("Authorization", tc.header)
			}
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("auth gate: %s → want %d, got %d", tc.name, tc.want, rec.Code)
			}
		})
	}
}

// Amendment 1: with NO token set (default single-user-host trust), /v1/* is
// open — the gate is opt-in.
func TestNoTokenLeavesV1Open(t *testing.T) {
	up, prov := okUpstream(t)
	s := New("127.0.0.1:4141")
	s.SetProvider(prov)
	s.upstreamBaseOverride = up.URL

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("default trust: /v1 must be open without --require-token, got %d", rec.Code)
	}
}
