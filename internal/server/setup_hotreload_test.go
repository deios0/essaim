package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"essaim/internal/upstream"
)

var errKeyRejectedTest = errors.New("test: key rejected")

// P0-3 — KEY / CONFIG HOT-RELOAD. When the user sets the key via /setup while
// `essaim serve` is running, it must take effect WITHOUT a restart. The proxy must
// re-resolve its provider/key from the store after a successful /setup POST, so
// the NEXT proxied request uses the new upstream — no process restart, no friction.
//
// This test reproduces cmd/essaim's wiring: a store the /setup POST writes through,
// an onProviderUpdate hook that re-reads that store and SetProvider's a keyed
// upstream, and a key-validator stand-in (so no network). It proves a chat request
// that 401s before the key is set succeeds (same process) right after.
func TestKeyHotReloadNoRestart(t *testing.T) {
	// A fake upstream that authorizes a single good key (both the validation GET
	// /v1/models and the chat POST go here via the upstream-base override).
	const goodKey = "sk-or-hot"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+goodKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(up.Close)

	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))

	store := &fakeSecretStore{m: map[string]string{}}

	s := New("127.0.0.1:4141")
	s.SetSecretStore(store)
	s.SetUpstreamBase(up.URL) // validation + relay both target the fake
	// Validator stand-in: accept the good key, reject anything else (no network).
	s.SetKeyValidator(func(_ context.Context, key string) error {
		if key == goodKey {
			return nil
		}
		return errKeyRejectedTest
	})
	// The hot-reload hook — exactly cmd/essaim's shape: re-read the store, and if a
	// key is now present, swap in a keyed provider live. No restart.
	s.SetOnProviderUpdate(func() {
		if key, _ := store.Get("openrouter-key"); key != "" {
			s.SetProvider(&upstream.SingleUpstream{Key: key, Detect: func(string) bool { return false }})
		}
	})

	// START with no key: the provider has no backend, so a chat request 401s.
	s.SetProvider(&upstream.SingleUpstream{Detect: func(string) bool { return false }})
	rec := httptest.NewRecorder()
	req := loopbackReq("POST", "/v1/chat/completions", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("before a key is set, chat must 401 (no backend), got %d body=%s", rec.Code, rec.Body.String())
	}

	// POST the key via /setup — same running process.
	rec = httptest.NewRecorder()
	req = loopbackReq("POST", "/setup/model", `{"provider":"openrouter","key":"`+goodKey+`"}`)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("setting a valid key must succeed, got %d body=%s", rec.Code, rec.Body.String())
	}

	// The NEXT chat request must use the new upstream WITHOUT a restart.
	rec = httptest.NewRecorder()
	req = loopbackReq("POST", "/v1/chat/completions", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("after a hot key set, the next chat must succeed (no restart), got %d body=%s", rec.Code, rec.Body.String())
	}
}
