package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"essaim/internal/inject"
	"essaim/internal/rules"
)

// F-E [conformance] A-2 skip-on-unsupported (fail-open). If the target model
// would 400 on an extra leading instruction message, essaim SKIPS injection
// entirely: forwards the VERBATIM origBody (incl. any stale essaim block — strip
// NOT run), enqueues no capture, classifies NOT degraded. Never turn a working
// request into a 400.

// serverWithUnsupported builds a server whose injector skips injection for the
// given model prefixes (the static A-2.3 config), pointed at the echo upstream.
func serverWithUnsupported(t *testing.T, up, dir string, unsupported func(string) bool) *Server {
	t.Helper()
	s := serverWithVault(t, up, dir)
	s.inj.injectUnsupported = unsupported
	return s
}

// V1.1-9: skip_injection_when_upstream_marked_unsupported. Upstream/model marked
// unsupported AND the incoming body contains a PRE-EXISTING stale essaim block ⇒
// forwarded upstream-bound bytes are byte-identical to origBody (stale block NOT
// stripped), no degraded, normal 200. (F-V2 verbatim-incl-stale.)
func TestSkipInjectionWhenModelUnsupportedForwardsVerbatimInclStale(t *testing.T) {
	var got []byte
	up := echoUpstream(t, &got)
	defer up.Close()
	dir := vaultWith(t, map[string]string{
		"pg.md": "---\nid: pg\ntitle: Use Postgres\nweight: 0.9\nconfidence: 0.9\nstatus: live\n---\nAlways use the PostgreSQL database, never MySQL.",
	})
	// Strict upstream: any "strict-" model would 400 on an extra instruction msg.
	s := serverWithUnsupported(t, up.URL, dir, func(m string) bool {
		return strings.HasPrefix(m, "strict-")
	})

	// origBody already carries a stale essaim block (an echoed prior-turn block).
	stale := `<!-- essaim:rules:begin v=1 -->\n- [H] Old: old rule\n<!-- essaim:rules:end -->`
	orig := `{"model":"strict-local","messages":[` +
		`{"role":"system","content":"` + stale + `"},` +
		`{"role":"user","content":"what database should I use?"}` +
		`]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(orig))
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("skip path must yield the upstream's normal 200, got %d", rec.Code)
	}
	// Forwarded bytes must be byte-identical to origBody — stale block NOT stripped.
	if string(got) != orig {
		t.Fatalf("skip must forward VERBATIM origBody (incl. stale block).\nwant %q\ngot  %q", orig, got)
	}
	// A fresh essaim block must NOT have been injected.
	if strings.Contains(string(got), "Always use the PostgreSQL database") {
		t.Fatalf("no fresh essaim block may be injected on the skip path:\n%s", got)
	}
	// Skip is NOT degraded (honest, policy-driven no-injection).
	if s.inj.degraded() {
		t.Fatalf("skip-on-unsupported must be degraded=false (A-2.3)")
	}
}

// A SUPPORTED model in the same server still injects (the skip predicate is
// model-scoped, not a global off-switch).
func TestSupportedModelStillInjectsWhenSkipConfigured(t *testing.T) {
	var got []byte
	up := echoUpstream(t, &got)
	defer up.Close()
	dir := vaultWith(t, map[string]string{
		"pg.md": "---\nid: pg\ntitle: Use Postgres\nweight: 0.9\nconfidence: 0.9\nstatus: live\n---\nAlways use the PostgreSQL database, never MySQL.",
	})
	s := serverWithUnsupported(t, up.URL, dir, func(m string) bool {
		return strings.HasPrefix(m, "strict-")
	})

	orig := `{"model":"gpt-4o","messages":[{"role":"user","content":"what database should I use?"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(orig))
	s.Handler().ServeHTTP(rec, req)

	if !strings.Contains(string(got), "<!-- essaim:rules:begin v=1 -->") {
		t.Fatalf("a supported model must still get injection:\n%s", got)
	}
}

// V1.1-8 / F-V7: raw_model_key_object_level_only. A `user` content string
// contains the literal text `"model":"strict-x"`; the REAL top-level model is
// `gpt-4o`. The skip decision must use the object-level `gpt-4o` (⇒ supported ⇒
// inject), NOT the in-content `strict-x` (which would falsely skip).
func TestSkipUsesObjectLevelModelNotInContentSubstring(t *testing.T) {
	var got []byte
	up := echoUpstream(t, &got)
	defer up.Close()
	dir := vaultWith(t, map[string]string{
		"pg.md": "---\nid: pg\ntitle: Use Postgres\nweight: 0.9\nconfidence: 0.9\nstatus: live\n---\nAlways use the PostgreSQL database, never MySQL.",
	})
	s := serverWithUnsupported(t, up.URL, dir, func(m string) bool {
		return strings.HasPrefix(m, "strict-")
	})

	// The user pasted an API example containing "model":"strict-x"; the real
	// top-level model is gpt-4o (supported). The ask is squarely on the pg rule's
	// topic (postgres vs mysql for my database) so it clears the relevance floor
	// honestly — the point under test is purely that the in-content "model" key does
	// not flip the skip decision, not that paste-noise alone forces an inject.
	orig := `{"model":"gpt-4o","messages":[{"role":"user","content":"with \"model\":\"strict-x\", for my database should I use postgres or mysql?"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(orig))
	s.Handler().ServeHTTP(rec, req)

	if !strings.Contains(string(got), "<!-- essaim:rules:begin v=1 -->") {
		t.Fatalf("in-content \"model\" substring must NOT trigger a skip; injection expected:\n%s", got)
	}
}

// Unit: safeBuild returns ErrInjectUnsupported (not a degrading error) and the
// verbatim origBody when the model is unsupported, so /health stays clean.
func TestSafeBuildReturnsUnsupportedHonestMiss(t *testing.T) {
	dir := vaultWith(t, map[string]string{
		"pg.md": "---\nid: pg\ntitle: Use Postgres\nweight: 0.9\nconfidence: 0.9\nstatus: live\n---\nAlways use the PostgreSQL database, never MySQL.",
	})
	store, err := rules.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	in := newInjectorWithStore(store, rules.GuardConfig{EagerBytes: 4096})
	in.injectUnsupported = func(m string) bool { return m == "strict-local" }

	orig := []byte(`{"model":"strict-local","messages":[{"role":"user","content":"what database should I use?"}]}`)
	body, _, berr := in.safeBuild(context.Background(), orig)
	if berr != inject.ErrInjectUnsupported {
		t.Fatalf("want ErrInjectUnsupported, got %v", berr)
	}
	if string(body) != string(orig) {
		t.Fatalf("unsupported skip must return verbatim origBody")
	}
	if in.degraded() {
		t.Fatalf("ErrInjectUnsupported must not mark degraded")
	}
}
