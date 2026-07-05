package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"oikos/internal/upstream"
)

// Fix #6 [MED latent]: New() must default the provider so a server that never had
// SetProvider called does not nil-deref. A request to /v1/chat/completions on a
// fresh New()'d server (no key, no local LLM) must return a clean 401 (the
// no-backend path), NOT panic on a nil provider.
func TestNewDefaultsProviderNoPanicReturns401(t *testing.T) {
	s := New("127.0.0.1:4141")

	// The provider must be defaulted by New() (this is the fix under test): a
	// non-nil *upstream.SingleUpstream, so the handler never nil-derefs.
	prov, ok := s.provider.(*upstream.SingleUpstream)
	if !ok || prov == nil {
		t.Fatalf("New() must default provider to a non-nil *upstream.SingleUpstream, got %T", s.provider)
	}
	// Force local-LLM detection off on the DEFAULTED provider (no key + no local
	// LLM → ErrNoBackend → 401), keeping the test hermetic (no real TCP dial).
	prov.Detect = func(string) bool { return false }

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("fresh New() server must not panic on a request; recovered: %v", r)
		}
	}()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-backend on a fresh server must be a clean 401, got %d", rec.Code)
	}
}
