package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"oikos/internal/config"
)

// P0-1 — SILENT-GREEN key acceptance. The /setup/model POST that saves an
// OpenRouter key MUST do ONE live validation call (GET {upstream}/v1/models with
// the key) BEFORE persisting. A bad key → a clear, actionable error and NOTHING
// persisted (no key in the store, no provider=openrouter in the config). A good
// key → persisted + success. The validation goes ONLY to the user's chosen
// upstream (no new phone-home).

// fakeUpstream is a stand-in OpenRouter that authorizes a single good key.
func fakeUpstream(t *testing.T, goodKey string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+goodKey {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"No auth credentials found"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// setupTestServerAt builds a server whose key-validation targets a fixed upstream
// base (the fake), so no real network is touched and the call goes ONLY there.
func setupTestServerAt(t *testing.T, upstreamBase string) (*Server, *fakeSecretStore) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("OIKOS_CONFIG", filepath.Join(dir, "config.json"))
	s := New("127.0.0.1:4141")
	fs := &fakeSecretStore{m: map[string]string{}}
	s.SetSecretStore(fs)
	s.SetUpstreamBase(upstreamBase) // validation + relay both target the fake
	return s, fs
}

func TestSetupModelBadKeyRejectedNothingPersisted(t *testing.T) {
	up := fakeUpstream(t, "sk-or-good")
	s, fs := setupTestServerAt(t, up.URL)

	rec := httptest.NewRecorder()
	req := loopbackReq("POST", "/setup/model", `{"provider":"openrouter","key":"sk-or-BAD"}`)
	s.Handler().ServeHTTP(rec, req)

	// A rejected key is NOT a 200 (no silent green).
	if rec.Code == http.StatusOK {
		t.Fatalf("a 401-returning upstream must NOT yield a 200 saved-ok; body=%s", rec.Body.String())
	}
	body := rec.Body.String()
	// The message must be human + actionable (mention the key was rejected).
	low := strings.ToLower(body)
	if !strings.Contains(low, "reject") {
		t.Fatalf("error must tell the user the key was rejected, got: %s", body)
	}
	// NOTHING persisted: no key in the store.
	if got, _ := fs.Get("openrouter-key"); got != "" {
		t.Fatalf("a rejected key must NOT be persisted to the store, got %q", got)
	}
	// NOTHING persisted: provider not flipped to openrouter (else the next serve 401s).
	c, _ := config.Load()
	if c.Provider == "openrouter" {
		t.Fatalf("provider must NOT be persisted openrouter when the key was rejected")
	}
	// The key must never leak to the response.
	if strings.Contains(body, "sk-or-BAD") {
		t.Fatalf("the rejected key leaked into the response body: %s", body)
	}
}

func TestSetupModelGoodKeyValidatedThenPersisted(t *testing.T) {
	up := fakeUpstream(t, "sk-or-good")
	s, fs := setupTestServerAt(t, up.URL)

	rec := httptest.NewRecorder()
	req := loopbackReq("POST", "/setup/model", `{"provider":"openrouter","key":"sk-or-good"}`)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("a 200-returning upstream must yield a 200 saved-ok, got %d body=%s", rec.Code, rec.Body.String())
	}
	if got, _ := fs.Get("openrouter-key"); got != "sk-or-good" {
		t.Fatalf("a validated key must be persisted to the store, got %q", got)
	}
	c, _ := config.Load()
	if c.Provider != "openrouter" {
		t.Fatalf("provider must be persisted openrouter after a validated key, got %q", c.Provider)
	}
	// The key must never land in the config file.
	p, _ := config.Path()
	raw, _ := os.ReadFile(p)
	if strings.Contains(string(raw), "sk-or-good") {
		t.Fatalf("SECRET LEAKED into config file:\n%s", raw)
	}
}

// A LOCAL-model setup (no key) skips the validation call entirely — the local
// path never touches the cloud upstream.
func TestSetupModelLocalSkipsKeyValidation(t *testing.T) {
	// An upstream base that would FAIL any GET (so if validation ran for local, it
	// would error). The local path must not call it at all.
	called := false
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	t.Cleanup(bad.Close)

	dir := t.TempDir()
	t.Setenv("OIKOS_CONFIG", filepath.Join(dir, "config.json"))
	s := New("127.0.0.1:4141")
	s.SetSecretStore(&fakeSecretStore{m: map[string]string{}})
	s.SetUpstreamBase(bad.URL)

	rec := httptest.NewRecorder()
	req := loopbackReq("POST", "/setup/model", `{"provider":"local"}`)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("local setup must succeed, got %d body=%s", rec.Code, rec.Body.String())
	}
	if called {
		t.Fatalf("local setup must NOT make a key-validation upstream call")
	}
	c, _ := config.Load()
	if c.Provider != "local" {
		t.Fatalf("local provider must be persisted, got %q", c.Provider)
	}
}

// "Persist nothing on failure" — if config.Save fails AFTER the validated key was
// written to the store, the key is rolled back so no orphaned credential remains.
func TestSetupModelConfigSaveFailureRollsBackKey(t *testing.T) {
	up := fakeUpstream(t, "sk-or-good")
	dir := t.TempDir()
	// Make config.Save fail: OIKOS_CONFIG's parent is a FILE, so MkdirAll errors.
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	t.Setenv("OIKOS_CONFIG", filepath.Join(blocker, "config.json"))

	s := New("127.0.0.1:4141")
	fs := &fakeSecretStore{m: map[string]string{}}
	s.SetSecretStore(fs)
	s.SetUpstreamBase(up.URL)

	rec := httptest.NewRecorder()
	req := loopbackReq("POST", "/setup/model", `{"provider":"openrouter","key":"sk-or-good"}`)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("a config-save failure must NOT report success, got 200")
	}
	// The key must have been rolled back (nothing persisted on failure).
	if got, _ := fs.Get("openrouter-key"); got != "" {
		t.Fatalf("on config-save failure the validated key must be rolled back, still stored: %q", got)
	}
}

// On a config-save failure, a PREVIOUSLY-stored working key must be RESTORED, not
// lost — the rollback reverts to the prior value rather than blindly deleting.
func TestSetupModelConfigSaveFailureRestoresPriorKey(t *testing.T) {
	up := fakeUpstream(t, "sk-or-new") // the new key validates fine
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	t.Setenv("OIKOS_CONFIG", filepath.Join(blocker, "config.json"))

	s := New("127.0.0.1:4141")
	fs := &fakeSecretStore{m: map[string]string{"openrouter-key": "sk-or-OLD-working"}}
	s.SetSecretStore(fs)
	s.SetUpstreamBase(up.URL)

	rec := httptest.NewRecorder()
	req := loopbackReq("POST", "/setup/model", `{"provider":"openrouter","key":"sk-or-new"}`)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("a config-save failure must NOT report success, got 200")
	}
	// The user's PRIOR working key must be intact (not lost to a blind delete).
	if got, _ := fs.Get("openrouter-key"); got != "sk-or-OLD-working" {
		t.Fatalf("on config-save failure the PRIOR working key must be restored, got %q", got)
	}
}

// The validation call goes ONLY to the user's chosen upstream — confirm it is not
// a new phone-home: with an upstream-base override set, the ONLY host contacted is
// that override (the fake). We assert by observing the fake recorded the request.
func TestSetupModelValidationHitsOnlyChosenUpstream(t *testing.T) {
	var gotPath, gotAuth string
	hits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(up.Close)

	s, _ := setupTestServerAt(t, up.URL)
	rec := httptest.NewRecorder()
	req := loopbackReq("POST", "/setup/model", `{"provider":"openrouter","key":"sk-or-x"}`)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("good key on a 200 upstream must succeed, got %d", rec.Code)
	}
	if hits != 1 {
		t.Fatalf("validation must make exactly ONE upstream call, made %d", hits)
	}
	if gotPath != "/v1/models" {
		t.Fatalf("validation must GET /v1/models, got %q", gotPath)
	}
	if gotAuth != "Bearer sk-or-x" {
		t.Fatalf("validation must carry the pasted key, got auth %q", gotAuth)
	}
}
