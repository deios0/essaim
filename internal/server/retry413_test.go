package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// F-B [BLOCKER — do-no-harm]: 413 / context-overflow retry-once.
//
// Injection can push a request that fit the model's window over it → the IDE
// gets a 413 essaim caused. Per v1.1 §5.4 / A-2.4: after the first upstream send,
// if status==413 OR the body matches a context-overflow signature AND not yet
// retried → re-send the EXACT ORIGINAL unmutated body once (degraded=true).
// Total upstream retries ≤ 1 per request (shared `retried` bool).

// Test F-B (413): a fake upstream that returns 413 when the body contains the
// essaim block but 200 without it ⇒ essaim retries once with origBody and the
// client gets 200 + the original (un-injected) response.
func TestRetryOnceOn413WithOriginalBody(t *testing.T) {
	var calls int32
	var sawBlockFirst, secondWasClean atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		buf := new(strings.Builder)
		_, _ = copyReader(buf, r.Body)
		body := buf.String()
		hasBlock := strings.Contains(body, "<!-- essaim:rules:begin v=1 -->")
		if hasBlock {
			// Injected request: simulate it being pushed over the context window.
			if n == 1 {
				sawBlockFirst.Store(true)
			}
			w.WriteHeader(http.StatusRequestEntityTooLarge) // 413
			_, _ = w.Write([]byte(`{"error":{"message":"too large"}}`))
			return
		}
		// Un-injected (original) request: succeeds.
		if n == 2 {
			secondWasClean.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"clean-ok"}}]}`))
	}))
	defer up.Close()

	// Vault rule that lexically matches a "database" query so the block IS injected.
	dir := vaultWith(t, map[string]string{
		"pg.md": "---\nid: pg\ntitle: Use Postgres\nweight: 0.9\nconfidence: 0.9\nstatus: live\n---\nAlways use the PostgreSQL database, never MySQL.",
	})
	s := serverWithVault(t, up.URL, dir)

	orig := `{"model":"gpt-4o","messages":[{"role":"user","content":"what database should I use?"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(orig))
	s.Handler().ServeHTTP(rec, req)

	if !sawBlockFirst.Load() {
		t.Fatalf("first upstream send must carry the injected essaim block (precondition)")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected exactly 2 upstream calls (inject 413 + retry clean), got %d", got)
	}
	if !secondWasClean.Load() {
		t.Fatalf("the retry must re-send the ORIGINAL unmutated body (no essaim block)")
	}
	if rec.Code != 200 {
		t.Fatalf("client must get 200 from the clean retry, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "clean-ok") {
		t.Fatalf("client must get the clean upstream response; got %q", rec.Body.String())
	}
	// The 413 must NOT have leaked to the client.
	if strings.Contains(rec.Body.String(), "too large") {
		t.Fatalf("the 413 error body must not reach the client; got %q", rec.Body.String())
	}
	if !s.inj.degraded() {
		t.Fatalf("a 413-triggered retry must mark the server degraded (spec §5.5)")
	}
}

// Test F-B (context-overflow body signature): an upstream that returns a 400
// with a context_length_exceeded body when the block is present, 200 without,
// ⇒ same single retry with origBody.
func TestRetryOnceOnContextOverflowBody(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		buf := new(strings.Builder)
		_, _ = copyReader(buf, r.Body)
		if strings.Contains(buf.String(), "<!-- essaim:rules:begin v=1 -->") {
			w.WriteHeader(http.StatusBadRequest) // 400 with overflow signature
			_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length exceeded","code":"context_length_exceeded"}}`))
			return
		}
		_ = n
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"clean-ok"}}]}`))
	}))
	defer up.Close()

	dir := vaultWith(t, map[string]string{
		"pg.md": "---\nid: pg\ntitle: Use Postgres\nweight: 0.9\nconfidence: 0.9\nstatus: live\n---\nAlways use the PostgreSQL database, never MySQL.",
	})
	s := serverWithVault(t, up.URL, dir)

	orig := `{"model":"gpt-4o","messages":[{"role":"user","content":"what database should I use?"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(orig))
	s.Handler().ServeHTTP(rec, req)

	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected exactly 2 upstream calls (overflow + clean retry), got %d", got)
	}
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "clean-ok") {
		t.Fatalf("client must get the clean retry 200/response; code=%d body=%q", rec.Code, rec.Body.String())
	}
	if !s.inj.degraded() {
		t.Fatalf("a context-overflow retry must mark degraded")
	}
}

// Test F-B (no double retry): a SECOND 413 (even the clean body 413s) is relayed
// verbatim to the client — never a third upstream attempt. Total retries ≤ 1.
func TestNoSecondRetryOnPersistent413(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"error":{"message":"still too large"}}`))
	}))
	defer up.Close()

	dir := vaultWith(t, map[string]string{
		"pg.md": "---\nid: pg\ntitle: Use Postgres\nweight: 0.9\nconfidence: 0.9\nstatus: live\n---\nAlways use the PostgreSQL database, never MySQL.",
	})
	s := serverWithVault(t, up.URL, dir)

	orig := `{"model":"gpt-4o","messages":[{"role":"user","content":"what database should I use?"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(orig))
	s.Handler().ServeHTTP(rec, req)

	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("a persistent 413 must produce EXACTLY 2 upstream calls (no third), got %d", got)
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("the second 413 must be relayed verbatim to the client, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "still too large") {
		t.Fatalf("the second 413 body must reach the client verbatim; got %q", rec.Body.String())
	}
}

// copyReader is a tiny io.Copy that avoids importing io in the test for one use.
func copyReader(dst *strings.Builder, src interface{ Read([]byte) (int, error) }) (int64, error) {
	var total int64
	buf := make([]byte, 4096)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			dst.Write(buf[:n])
			total += int64(n)
		}
		if err != nil {
			return total, nil
		}
	}
}
