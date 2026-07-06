package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"essaim/internal/config"
	"essaim/internal/secret"
)

// setupTestServer returns a fresh server with the setup handler wired and an
// in-memory secret store, plus a temp config path set via ESSAIM_CONFIG.
func setupTestServer(t *testing.T) (*Server, *fakeSecretStore) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))
	s := New("127.0.0.1:4141")
	fs := &fakeSecretStore{m: map[string]string{}}
	s.SetSecretStore(fs)
	return s, fs
}

// loopbackReq builds a request whose RemoteAddr is loopback (httptest's default
// is the public TEST-NET 192.0.2.1, which the loopback-only guard rejects).
func loopbackReq(method, target string, body string) *http.Request {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "127.0.0.1:4141" // a real loopback client sends a loopback Host
	return req
}

type fakeSecretStore struct{ m map[string]string }

func (f *fakeSecretStore) Get(k string) (string, error) { return f.m[k], nil }
func (f *fakeSecretStore) Set(k, v string) error        { f.m[k] = v; return nil }
func (f *fakeSecretStore) Delete(k string) error        { delete(f.m, k); return nil }

var _ secret.Store = (*fakeSecretStore)(nil)

func TestSetupGETServesSelfContainedHTML(t *testing.T) {
	s, _ := setupTestServer(t)
	rec := httptest.NewRecorder()
	req := loopbackReq("GET", "/setup", "")
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /setup = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET /setup content-type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	// One self-contained page: no external <script src> or <link href> RESOURCE
	// loads (a user-clickable <a href> deep link to openrouter is fine — it's not
	// a resource the page fetches).
	if strings.Contains(body, "src=\"http") || strings.Contains(body, "<link") {
		t.Fatalf("/setup HTML must be self-contained (no external <script src>/<link>):\n%s", body)
	}
	if !strings.Contains(body, "essaim") {
		t.Fatalf("/setup HTML should mention essaim")
	}
	// The three first-run surfaces must be present.
	for _, want := range []string{"/setup/model", "/setup/vault", "/setup/wire"} {
		if !strings.Contains(body, want) {
			t.Fatalf("/setup HTML should POST to %s; not found", want)
		}
	}
	// The page must carry the one-line "rules fire on shared vocabulary" hint that
	// points users at the writing-rules guide — so a first rule they write actually
	// injects (the e2e relevance-gating finding, surfaced at first run).
	for _, want := range []string{"database", "docs/writing-rules.md"} {
		if !strings.Contains(body, want) {
			t.Fatalf("/setup HTML should surface the writing-rules vocabulary hint containing %q; not found", want)
		}
	}
}

func TestSetupStateReportsLocalLLMAndConfig(t *testing.T) {
	s, _ := setupTestServer(t)
	// A provider that reports a local LLM present.
	s.SetSetupDetect(func() (string, bool) { return "lmstudio", true })

	rec := httptest.NewRecorder()
	req := loopbackReq("GET", "/setup/state", "")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /setup/state = %d, want 200", rec.Code)
	}
	var st map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("state not JSON: %v", err)
	}
	if st["local_llm"] != "lmstudio" {
		t.Fatalf("state.local_llm = %v, want lmstudio", st["local_llm"])
	}
	if st["local_llm_present"] != true {
		t.Fatalf("state.local_llm_present = %v, want true", st["local_llm_present"])
	}
}

func TestSetupPostModelLocalPersistsConfig(t *testing.T) {
	s, _ := setupTestServer(t)
	rec := httptest.NewRecorder()
	req := loopbackReq("POST", "/setup/model", `{"provider":"local"}`)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /setup/model local = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	c, _ := config.Load()
	if c.Provider != "local" {
		t.Fatalf("persisted provider = %q, want local", c.Provider)
	}
}

func TestSetupPostModelOpenRouterStoresKeyInKeychainNotConfig(t *testing.T) {
	s, fs := setupTestServer(t)
	// Hermetic: a passing validator so this test asserts keychain isolation, not
	// live validation, and never touches the network (P0-1 has its own tests).
	s.SetKeyValidator(func(context.Context, string) error { return nil })
	rec := httptest.NewRecorder()
	req := loopbackReq("POST", "/setup/model", `{"provider":"openrouter","key":"sk-or-secret-123"}`)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /setup/model openrouter = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	// Key goes to the secret store, NEVER the config file.
	if got, _ := fs.Get("openrouter-key"); got != "sk-or-secret-123" {
		t.Fatalf("key not stored in keychain; got %q", got)
	}
	c, _ := config.Load()
	if c.Provider != "openrouter" {
		t.Fatalf("persisted provider = %q, want openrouter", c.Provider)
	}
	// The config file must not contain the secret.
	p, _ := config.Path()
	raw, _ := os.ReadFile(p)
	if strings.Contains(string(raw), "sk-or-secret-123") {
		t.Fatalf("SECRET LEAKED into config file:\n%s", raw)
	}
}

// fakeFailingStore simulates a headless box where the OS keyring is unavailable
// (the go-keyring "failed to unlock correct collection" case).
type fakeFailingStore struct{}

func (fakeFailingStore) Get(string) (string, error) { return "", nil }
func (fakeFailingStore) Set(string, string) error {
	return errors.New("failed to unlock correct collection '/org/freedesktop/secrets/aliases/default'")
}

func TestSetupPostModelKeyringFailureGivesActionableMessageNotRaw500(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))
	s := New("127.0.0.1:4141")
	s.SetSecretStore(fakeFailingStore{})
	// Hermetic: the key VALIDATES (so we reach the keychain-store step) but the
	// store fails — the case under test. No network.
	s.SetKeyValidator(func(context.Context, string) error { return nil })

	rec := httptest.NewRecorder()
	req := loopbackReq("POST", "/setup/model", `{"provider":"openrouter","key":"sk-or-x"}`)
	s.Handler().ServeHTTP(rec, req)

	// Not a raw 500 — a clean, actionable status.
	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("keyring failure should NOT surface as a raw 500: body %s", rec.Body.String())
	}
	body := rec.Body.String()
	// The message must point the user at the env-var fallback (the P1-6b pattern).
	if !strings.Contains(body, "ESSAIM_OPENROUTER_KEY") {
		t.Fatalf("keyring failure message must mention the ESSAIM_OPENROUTER_KEY env fallback, got: %s", body)
	}
	// The raw go-keyring jargon must not leak to the user.
	if strings.Contains(body, "freedesktop") {
		t.Fatalf("raw go-keyring jargon leaked to the user: %s", body)
	}
	// provider must NOT be persisted as openrouter when the key couldn't be stored
	// (otherwise the next serve resolves to no key and 401s).
	c, _ := config.Load()
	if c.Provider == "openrouter" {
		t.Fatalf("provider must not be persisted openrouter when the key failed to store")
	}
}

func TestSetupPostModelOpenRouterRejectsEmptyKey(t *testing.T) {
	s, _ := setupTestServer(t)
	rec := httptest.NewRecorder()
	req := loopbackReq("POST", "/setup/model", `{"provider":"openrouter","key":""}`)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty openrouter key should be a 400, got %d", rec.Code)
	}
}

func TestSetupPostVaultPersists(t *testing.T) {
	s, _ := setupTestServer(t)
	vdir := t.TempDir()
	rec := httptest.NewRecorder()
	req := loopbackReq("POST", "/setup/vault", `{"vault_dir":`+jsonQuote(vdir)+`}`)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /setup/vault = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	c, _ := config.Load()
	if c.VaultDir != vdir {
		t.Fatalf("persisted vault_dir = %q, want %q", c.VaultDir, vdir)
	}
}

func TestSetupPostWirePersistsTool(t *testing.T) {
	s, _ := setupTestServer(t)
	rec := httptest.NewRecorder()
	req := loopbackReq("POST", "/setup/wire", `{"tool":"cursor"}`)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /setup/wire = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	c, _ := config.Load()
	found := false
	for _, w := range c.WiredTools {
		if w.Name == "cursor" {
			found = true
		}
	}
	if !found {
		t.Fatalf("cursor not persisted as wired: %+v", c.WiredTools)
	}
}

func TestSetupRejectsNonLoopbackRemote(t *testing.T) {
	s, _ := setupTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/setup", nil)
	req.RemoteAddr = "203.0.113.7:55555" // a public IP
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("/setup from a non-loopback remote must be 403, got %d", rec.Code)
	}
}

func TestSetupGETIsOpenWithoutToken(t *testing.T) {
	// Even with --require-token engaged, /setup is reachable (it's how you GET the
	// token-bearing wiring). It is loopback-only and non-/v1.
	s, _ := setupTestServer(t)
	s.SetToken("sometoken")
	rec := httptest.NewRecorder()
	req := loopbackReq("GET", "/setup", "")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/setup must be open even with a token set, got %d", rec.Code)
	}
}

// jsonQuote returns a minimal JSON-quoted string (test helper for paths with
// no special chars; tmpdirs are safe).
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
