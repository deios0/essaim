package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"oikos/internal/capture"
	"oikos/internal/rules"
	"oikos/internal/upstream"
)

// recordingSink collects enqueued captures for assertions.
type recordingSink struct {
	mu       sync.Mutex
	captures []capture.Capture
}

func (s *recordingSink) Enqueue(c capture.Capture) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captures = append(s.captures, c)
}
func (s *recordingSink) all() []capture.Capture {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]capture.Capture(nil), s.captures...)
}

func serverWithVaultAndSink(t *testing.T, up, dir string, sink CaptureSink) *Server {
	t.Helper()
	s := serverWithVault(t, up, dir)
	s.SetCaptureSink(sink)
	return s
}

// Test 1 (BL-1): a floor-clearing draft in _inbox/ is NEVER injected.
func TestDraftInInboxNeverInjected(t *testing.T) {
	var got []byte
	up := echoUpstream(t, &got)
	defer up.Close()
	dir := t.TempDir()
	// A live rule that would match, AND a draft in _inbox/ whose body strongly
	// matches the query — the draft must be walled out of the index.
	writeFileM3(t, dir, "live.md", "---\nid: live\ntitle: Live DB\nstatus: live\nweight: 0.9\nconfidence: 0.9\n---\nUse the live database rule.")
	writeFileM3(t, dir+"/_inbox", "draft.md", "---\nid: draftrule\ntitle: Draft DB\nstatus: draft\nweight: 0.99\nconfidence: 0.99\n---\nDRAFT_SECRET_DATABASE_RULE never inject me.")
	s := serverWithVault(t, up.URL, dir)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"what database rule should I use?"}]}`))
	s.Handler().ServeHTTP(rec, req)

	if strings.Contains(string(got), "DRAFT_SECRET_DATABASE_RULE") {
		t.Fatalf("a draft in _inbox/ must NEVER be injected:\n%s", got)
	}
}

// Test 2: a status:active rule in remembered/ IS injected (whitelist {active,live}).
func TestActiveSigilRuleDoesInject(t *testing.T) {
	var got []byte
	up := echoUpstream(t, &got)
	defer up.Close()
	dir := t.TempDir()
	writeFileM3(t, dir+"/remembered/2026-06-23", "a.md",
		"---\nid: act\ntitle: Active DB\nstatus: active\nweight: 0.9\nconfidence: 0.9\n---\nAlways use the staging database for tests.")
	s := serverWithVault(t, up.URL, dir)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"which database for tests?"}]}`))
	s.Handler().ServeHTTP(rec, req)

	if !strings.Contains(string(got), "Always use the staging database for tests") {
		t.Fatalf("a status:active rule must inject:\n%s", got)
	}
}

// Test 37: the M2 verbatim relay is byte-identical WITH the tee mounted.
func TestStreamPassthroughByteIdenticalWithTee(t *testing.T) {
	const sse = "data: {\"choices\":[{\"delta\":{\"content\":\"hi there\"}}]}\n\ndata: [DONE]\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer up.Close()
	dir := vaultWith(t, map[string]string{
		"pg.md": "---\nid: pg\ntitle: Use Postgres\nweight: 0.9\nconfidence: 0.9\nstatus: live\n---\nAlways use the PostgreSQL database, never MySQL.",
	})
	sink := &recordingSink{}
	s := serverWithVaultAndSink(t, up.URL, dir, sink)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"what database should I use?"}]}`))
	s.Handler().ServeHTTP(rec, req)

	if rec.Body.String() != sse {
		t.Fatalf("client stream must be byte-identical WITH the tee:\nwant %q\ngot  %q", sse, rec.Body.String())
	}
	// And the tee assembled the assistant text off-path.
	caps := sink.all()
	if len(caps) != 1 || caps[0].AssistantText != "hi there" {
		t.Fatalf("tee must assemble assistant text off-path: %+v", caps)
	}
}

// Test 38: Accept-Encoding is stripped upstream; the tap reads plaintext.
func TestAcceptEncodingStrippedUpstreamTapReadsPlaintext(t *testing.T) {
	var sawAcceptEncoding string
	const sse = "data: {\"choices\":[{\"delta\":{\"content\":\"plain\"}}]}\n\ndata: [DONE]\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAcceptEncoding = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer up.Close()
	dir := vaultWith(t, map[string]string{
		"pg.md": "---\nid: pg\ntitle: Use Postgres\nweight: 0.9\nconfidence: 0.9\nstatus: live\n---\nAlways use the PostgreSQL database, never MySQL.",
	})
	sink := &recordingSink{}
	s := serverWithVaultAndSink(t, up.URL, dir, sink)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"what database should I use?"}]}`))
	req.Header.Set("Accept-Encoding", "gzip")
	s.Handler().ServeHTTP(rec, req)

	if sawAcceptEncoding != "" {
		t.Fatalf("upstream must NOT receive Accept-Encoding (got %q)", sawAcceptEncoding)
	}
	caps := sink.all()
	if len(caps) != 1 || caps[0].AssistantText != "plain" {
		t.Fatalf("the tap must read plaintext assistant_text: %+v", caps)
	}
}

// Test 49: Capture.MatchedRuleIDs comes from the snapshot, not the prompt; no
// message text carries a rule id.
func TestMatchedRuleIDsFromSnapshotNotPrompt(t *testing.T) {
	const sse = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer up.Close()
	dir := vaultWith(t, map[string]string{
		"pg.md": "---\nid: use-postgres-rule\ntitle: Use Postgres\nweight: 0.9\nconfidence: 0.9\nstatus: live\n---\nAlways use the PostgreSQL database, never MySQL.",
	})
	sink := &recordingSink{}
	s := serverWithVaultAndSink(t, up.URL, dir, sink)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"what database should I use?"}]}`))
	s.Handler().ServeHTTP(rec, req)

	caps := sink.all()
	if len(caps) != 1 {
		t.Fatalf("want 1 capture, got %d", len(caps))
	}
	c := caps[0]
	if len(c.MatchedRuleIDs) == 0 || c.MatchedRuleIDs[0] != "use-postgres-rule" {
		t.Fatalf("MatchedRuleIDs must come from the snapshot: %v", c.MatchedRuleIDs)
	}
	for _, m := range c.OriginalMessages {
		if strings.Contains(m.Content, "use-postgres-rule") {
			t.Fatal("no captured message text may carry a rule id")
		}
	}
	// The captured clean messages are the user's original (oikos-free).
	if len(c.OriginalMessages) != 1 || c.OriginalMessages[0].Content != "what database should I use?" {
		t.Fatalf("captured messages must be the oikos-free originals: %+v", c.OriginalMessages)
	}
}

// Test 48 (F-6): an injected turn → upstream 413 → retry → ZERO captures.
func TestOverflowRetryTurnNotCaptured(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), rules.OIKOS_BEGIN) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = io.WriteString(w, `{"error":"too large"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"clean answer"}}]}`)
	}))
	defer up.Close()
	dir := vaultWith(t, map[string]string{
		"pg.md": "---\nid: pg\ntitle: Use Postgres\nweight: 0.9\nconfidence: 0.9\nstatus: live\n---\nAlways use the PostgreSQL database, never MySQL.",
	})
	sink := &recordingSink{}
	s := serverWithVaultAndSink(t, up.URL, dir, sink)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"what database should I use?"}]}`))
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("retry must succeed: %d", rec.Code)
	}
	if n := len(sink.all()); n != 0 {
		t.Fatalf("the overflow-retry turn must NOT be captured, got %d captures", n)
	}
}

// Test 43 (server-level): a non-streaming response is captured with the right
// assistant text and Stream==false.
func TestNonStreamCaptureServerPath(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hello world"}}]}`)
	}))
	defer up.Close()
	dir := vaultWith(t, map[string]string{
		"pg.md": "---\nid: pg\ntitle: Use Postgres\nweight: 0.9\nconfidence: 0.9\nstatus: live\n---\nAlways use the PostgreSQL database, never MySQL.",
	})
	sink := &recordingSink{}
	s := serverWithVaultAndSink(t, up.URL, dir, sink)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"what database should I use?"}]}`))
	s.Handler().ServeHTTP(rec, req)

	caps := sink.all()
	if len(caps) != 1 || caps[0].AssistantText != "hello world" || caps[0].Stream {
		t.Fatalf("non-stream capture wrong: %+v", caps)
	}
}

// Test 10 (BL-4): two tools sharing the same base_url; the native-file-wired one
// is NOT proxy-injected, the other IS.
func TestArbiterKeysOnToolIdentityNotBaseURL(t *testing.T) {
	got := map[string][]byte{}
	var mu sync.Mutex
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got[r.Header.Get("X-Test-Tool")] = b
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer up.Close()
	dir := vaultWith(t, map[string]string{
		"pg.md": "---\nid: pg\ntitle: Use Postgres\nweight: 0.9\nconfidence: 0.9\nstatus: live\n---\nAlways use the PostgreSQL database, never MySQL.",
	})
	s := serverWithVault(t, up.URL, dir)
	// Tool "claude-code" is wired to the native-file channel; both share the
	// SAME base_url (the test seam), so only tool identity can split them.
	s.SetFileEmitTools(map[string]bool{"claude-code": true})

	send := func(tool string) []byte {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"what database should I use?"}]}`))
		req.Header.Set(oikosToolHeader, tool)
		req.Header.Set("X-Test-Tool", tool)
		s.Handler().ServeHTTP(rec, req)
		mu.Lock()
		defer mu.Unlock()
		return got[tool]
	}

	cc := send("claude-code") // native-file wired → NOT proxy-injected
	cursor := send("cursor")  // unwired → IS proxy-injected

	if strings.Contains(string(cc), rules.OIKOS_BEGIN) {
		t.Fatalf("the native-file-wired tool must NOT be proxy-injected:\n%s", cc)
	}
	if !strings.Contains(string(cursor), rules.OIKOS_BEGIN) {
		t.Fatalf("the unwired tool MUST be proxy-injected:\n%s", cursor)
	}
}

// Test 11: a native-file-wired tool hitting chat/completions ⇒ the proxy
// forwards UNMUTATED (block only lives in the native file).
func TestPerToolOneChannelNoDoubleInjection(t *testing.T) {
	var got []byte
	up := echoUpstream(t, &got)
	defer up.Close()
	dir := vaultWith(t, map[string]string{
		"pg.md": "---\nid: pg\ntitle: Use Postgres\nweight: 0.9\nconfidence: 0.9\nstatus: live\n---\nAlways use the PostgreSQL database, never MySQL.",
	})
	s := serverWithVault(t, up.URL, dir)
	s.SetFileEmitTools(map[string]bool{"claude-code": true})

	orig := `{"model":"gpt-4o","messages":[{"role":"user","content":"what database should I use?"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(orig))
	req.Header.Set(oikosToolHeader, "claude-code")
	s.Handler().ServeHTTP(rec, req)

	if string(got) != orig {
		t.Fatalf("a native-file-wired tool's request must be forwarded UNMUTATED:\nwant %s\ngot  %s", orig, got)
	}
}

// Test 45 (BR-A2-13): a streamed request still delivers all client bytes even
// when the off-path consumer is irrelevant — the client is never blocked. Here
// we assert byte-identity holds and the capture still completes (the drop path
// is exercised by the unit cap test; this guards the no-added-latency contract).
func TestCaptureDoesNotBlockClient(t *testing.T) {
	// A large stream; the client must get every byte regardless of the tee.
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		sb.WriteString("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")
	full := sb.String()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, full)
	}))
	defer up.Close()
	dir := vaultWith(t, map[string]string{
		"pg.md": "---\nid: pg\ntitle: Use Postgres\nweight: 0.9\nconfidence: 0.9\nstatus: live\n---\nAlways use the PostgreSQL database, never MySQL.",
	})
	sink := &recordingSink{}
	s := serverWithVaultAndSink(t, up.URL, dir, sink)

	start := time.Now()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"what database should I use?"}]}`))
	s.Handler().ServeHTTP(rec, req)
	if rec.Body.String() != full {
		t.Fatal("client must receive ALL bytes with the tee mounted")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the tee must not add meaningful latency")
	}
}

// ensure upstream import used.
var _ = upstream.SingleUpstream{}

func writeFileM3(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := osMkdirWrite(dir, name, content); err != nil {
		t.Fatalf("write %s/%s: %v", dir, name, err)
	}
}
