package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"oikos/internal/rules"
	"oikos/internal/upstream"
)

// echoUpstream records the request body the upstream received, so tests can
// assert exactly what oikos forwarded.
func echoUpstream(t *testing.T, got *[]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*got = b
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
}

func vaultWith(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func serverWithVault(t *testing.T, up string, dir string) *Server {
	t.Helper()
	s := New("127.0.0.1:4141")
	s.SetProvider(&upstream.SingleUpstream{Key: "k", Detect: func(string) bool { return false }})
	s.upstreamBaseOverride = up
	store, err := rules.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	cfg := rules.GuardConfig{EagerBytes: 4096, MatchFloor: 0.0} // floor 0 so any match injects
	s.SetInjector(newInjectorWithStore(store, cfg))
	return s
}

// End-to-end: a real POST /v1/chat/completions with a DB question injects the
// "Use Postgres" rule into the body the upstream receives (the live-demo path).
func TestEndToEndInjectionReachesUpstream(t *testing.T) {
	var got []byte
	up := echoUpstream(t, &got)
	defer up.Close()
	// Rule body genuinely mentions "database" so the query lexically matches it
	// (F-A: relevance now requires real query-word coverage, not trigram noise).
	dir := vaultWith(t, map[string]string{
		"pg.md": "---\nid: pg\ntitle: Use Postgres\nweight: 0.9\nconfidence: 0.9\nstatus: live\n---\nAlways use the PostgreSQL database, never MySQL.",
	})
	s := serverWithVault(t, up.URL, dir)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"what database should I use?"}]}`))
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status: %d", rec.Code)
	}
	if !strings.Contains(string(got), "<!-- oikos:rules:begin v=1 -->") {
		t.Fatalf("upstream did not receive an injected oikos block:\n%s", got)
	}
	if !strings.Contains(string(got), "Always use the PostgreSQL database, never MySQL.") {
		t.Fatalf("injected block missing the rule body:\n%s", got)
	}
	// The original user message must survive.
	if !strings.Contains(string(got), "what database should I use?") {
		t.Fatalf("user message lost")
	}
}

// Test 29: failopen_deadline_forwards_original — a build that overruns the
// deadline ⇒ the upstream sees the EXACT original bytes (no oikos block), the
// response is 200 (never 500), and /health.degraded == true.
func TestFailOpenDeadlineForwardsOriginal(t *testing.T) {
	var got []byte
	up := echoUpstream(t, &got)
	defer up.Close()
	dir := vaultWith(t, map[string]string{
		"pg.md": "---\nid: pg\ntitle: Use Postgres\nweight: 0.9\nconfidence: 0.9\n---\nUse PostgreSQL.",
	})
	s := serverWithVault(t, up.URL, dir)
	// Force the build to overrun a tiny deadline.
	s.inj.deadline = 5 * time.Millisecond
	s.inj.buildHook = func() { time.Sleep(50 * time.Millisecond) }

	orig := `{"model":"gpt-4o","messages":[{"role":"user","content":"what database?"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(orig))
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("must never 500 on overrun, got %d", rec.Code)
	}
	if strings.Contains(string(got), "oikos:rules:begin") {
		t.Fatalf("overrun must forward original WITHOUT injection:\n%s", got)
	}
	if string(got) != orig {
		t.Fatalf("fail-open must forward byte-EXACT original.\nwant %q\ngot  %q", orig, got)
	}
	if !s.inj.degraded() {
		t.Fatalf("deadline overrun must mark degraded")
	}
}

// Test 30: failopen_panic_recover_requestside — a request-side panic ⇒ forward
// original unmutated, return upstream status (not 500), degraded == true.
func TestFailOpenPanicRecover(t *testing.T) {
	var got []byte
	up := echoUpstream(t, &got)
	defer up.Close()
	dir := vaultWith(t, map[string]string{
		"pg.md": "---\nid: pg\ntitle: Use Postgres\nweight: 0.9\nconfidence: 0.9\n---\nUse PostgreSQL.",
	})
	s := serverWithVault(t, up.URL, dir)
	s.inj.buildHook = func() { panic("boom in the request-side transform") }

	orig := `{"model":"gpt-4o","messages":[{"role":"user","content":"db?"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(orig))

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic must be recovered inside safeBuild, leaked: %v", r)
		}
	}()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("panic fail-open must return upstream status, got %d", rec.Code)
	}
	if string(got) != orig {
		t.Fatalf("panic fail-open must forward byte-exact original.\nwant %q\ngot  %q", orig, got)
	}
	if !s.inj.degraded() {
		t.Fatalf("panic must mark degraded")
	}
}

// Test 31: failopen_forwards_byte_exact_not_remarshalled — on fail-open, even a
// body carrying a stale block is forwarded byte-exact (strip skipped on
// fail-open; no re-serialization that would bust the prompt cache).
func TestFailOpenForwardsByteExactWithStaleBlock(t *testing.T) {
	var got []byte
	up := echoUpstream(t, &got)
	defer up.Close()
	dir := vaultWith(t, map[string]string{
		"pg.md": "---\nid: pg\ntitle: T\nweight: 0.9\nconfidence: 0.9\n---\nB.",
	})
	s := serverWithVault(t, up.URL, dir)
	s.inj.deadline = 1 * time.Millisecond
	s.inj.buildHook = func() { time.Sleep(30 * time.Millisecond) }

	// A request that already carries a stale oikos block.
	orig := `{"messages":[{"role":"system","content":"<!-- oikos:rules:begin v=1 -->\nstale\n<!-- oikos:rules:end -->"},{"role":"user","content":"x"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(orig))
	s.Handler().ServeHTTP(rec, req)

	if string(got) != orig {
		t.Fatalf("fail-open must forward byte-exact (stale block left intact).\nwant %q\ngot  %q", orig, got)
	}
}

// Test 33: index_empty_not_degraded — empty vault ⇒ forward unmutated,
// degraded == false, rules_indexed == 0.
func TestIndexEmptyNotDegraded(t *testing.T) {
	var got []byte
	up := echoUpstream(t, &got)
	defer up.Close()
	s := serverWithVault(t, up.URL, t.TempDir()) // empty vault

	orig := `{"messages":[{"role":"user","content":"db?"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(orig))
	s.Handler().ServeHTTP(rec, req)

	if string(got) != orig {
		t.Fatalf("empty index must forward unmutated.\nwant %q\ngot %q", orig, got)
	}
	if s.inj.degraded() {
		t.Fatalf("empty index is an honest miss, NOT degraded")
	}
	// /health reports rules_indexed:0, degraded:false.
	hr := httptest.NewRecorder()
	s.Handler().ServeHTTP(hr, httptest.NewRequest("GET", "/health", nil))
	var h map[string]any
	_ = json.Unmarshal(hr.Body.Bytes(), &h)
	if h["degraded"] != false {
		t.Fatalf("health.degraded must be false, got %v", h["degraded"])
	}
	if h["rules_indexed"].(float64) != 0 {
		t.Fatalf("health.rules_indexed must be 0, got %v", h["rules_indexed"])
	}
}

// Test 32: health_degraded_sticky_window — fail-open at T ⇒ degraded true within
// the window, false after it. /health needs no key.
func TestHealthDegradedStickyWindow(t *testing.T) {
	dir := vaultWith(t, map[string]string{"r.md": "---\nid: r\ntitle: R\n---\nb"})
	store, _ := rules.NewStore(dir)
	in := newInjectorWithStore(store, rules.GuardConfig{})
	base := time.Now()
	cur := base
	in.now = func() time.Time { return cur }

	in.markDegraded()
	cur = base.Add(59 * time.Second)
	if !in.degraded() {
		t.Fatalf("degraded must be true at T+59s")
	}
	cur = base.Add(61 * time.Second)
	if in.degraded() {
		t.Fatalf("degraded must be false at T+61s")
	}
}

// A no-vault server (nil injector) is a pure pass-through: /health works,
// degraded false, rules_indexed 0, and chat relays verbatim.
func TestNilInjectorPassthrough(t *testing.T) {
	var got []byte
	up := echoUpstream(t, &got)
	defer up.Close()
	s := New("127.0.0.1:4141")
	s.SetProvider(&upstream.SingleUpstream{Key: "k", Detect: func(string) bool { return false }})
	s.upstreamBaseOverride = up.URL
	// no injector set → nil

	orig := `{"messages":[{"role":"user","content":"x"}]}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(orig)))
	if string(got) != orig {
		t.Fatalf("nil injector must pass body verbatim.\nwant %q got %q", orig, got)
	}
	hr := httptest.NewRecorder()
	s.Handler().ServeHTTP(hr, httptest.NewRequest("GET", "/health", nil))
	if hr.Code != 200 {
		t.Fatalf("health must work with nil injector: %d", hr.Code)
	}
}
