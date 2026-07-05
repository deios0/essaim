package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// P2-2: a NON-2xx upstream response (4xx/5xx incl 429/500) must NOT be fed into
// the capture/learn pipeline — a model error is not a training signal. Before the
// fix, forwardChat passed a LIVE capture context to relayResponse on the error
// path, so an upstream 500/429 trained the learner with the error body as the
// "assistant answer". The overflow-retry path already passed nil; this closes the
// same hole for every other non-2xx status.
func TestUpstreamErrorNotCaptured(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"server_500", http.StatusInternalServerError, `{"error":{"message":"internal error"}}`},
		{"rate_limit_429", http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`},
		{"bad_request_400", http.StatusBadRequest, `{"error":{"message":"bad request"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
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

			// The error status + body must still relay verbatim to the client.
			if rec.Code != tc.status {
				t.Fatalf("client must see the upstream status %d, got %d", tc.status, rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "error") {
				t.Fatalf("client must see the upstream error body, got %q", rec.Body.String())
			}
			// But the learner must NOT be trained on the model error.
			if n := len(sink.all()); n != 0 {
				t.Fatalf("a non-2xx upstream response must NOT be captured, got %d captures", n)
			}
		})
	}
}

// P2-2 positive control: a 2xx response IS still captured (the fix must not
// disable capture for the success path).
func TestUpstream2xxStillCaptured(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"use postgres"}}]}`)
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

	if n := len(sink.all()); n != 1 {
		t.Fatalf("a 2xx response must still be captured, got %d captures", n)
	}
}
