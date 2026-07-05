package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"oikos/internal/upstream"
)

// Fix #1 [BLOCKER]: the server-owned client must NOT inject Accept-Encoding: gzip
// nor auto-decompress. A gzip upstream's wire bytes and its Content-Encoding: gzip
// header must reach the client byte-identically (the verbatim-relay invariant).
//
// Regression guard: with http.DefaultClient (which sets DisableCompression=false),
// the transport adds Accept-Encoding: gzip, transparently decompresses, and STRIPS
// Content-Encoding — so the client would see plaintext and no Content-Encoding.
func TestRelayPreservesGzipBytesAndEncodingHeader(t *testing.T) {
	const plaintext = `{"choices":[{"delta":{"content":"hi"}}]}`

	// Build the exact gzip wire bytes the upstream will emit.
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write([]byte(plaintext)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	wire := gz.Bytes()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Upstream declares gzip encoding and writes the gzip wire bytes verbatim.
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(wire)
	}))
	defer up.Close()

	s := New("127.0.0.1:4141")
	s.SetProvider(&upstream.SingleUpstream{Key: "k", Detect: func(string) bool { return false }})
	s.upstreamBaseOverride = up.URL

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	s.Handler().ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding must be preserved verbatim; want %q, got %q", "gzip", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), wire) {
		t.Fatalf("relay must deliver the EXACT gzip wire bytes (no auto-decompress).\nwant % x\ngot  % x", wire, rec.Body.Bytes())
	}
}

// Fix #4 [MED]: RFC 7230 §6.1 hop-by-hop headers (and headers named in the
// Connection value) must NOT be forwarded to the upstream.
func TestRelayStripsHopByHopHeadersOutbound(t *testing.T) {
	var gotConnection, gotTE, gotCustom string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotConnection = r.Header.Get("Connection")
		gotTE = r.Header.Get("TE")
		gotCustom = r.Header.Get("X-Custom")
		_, _ = io.WriteString(w, "ok")
	}))
	defer up.Close()

	s := New("127.0.0.1:4141")
	s.SetProvider(&upstream.SingleUpstream{Key: "k", Detect: func(string) bool { return false }})
	s.upstreamBaseOverride = up.URL

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	// Connection names X-Custom → X-Custom becomes hop-by-hop and must be dropped.
	req.Header.Set("Connection", "X-Custom")
	req.Header.Set("TE", "trailers")
	req.Header.Set("X-Custom", "secret-hop-value")
	s.Handler().ServeHTTP(rec, req)

	if gotCustom != "" {
		t.Fatalf("X-Custom (named in Connection) must be stripped; upstream saw %q", gotCustom)
	}
	if gotConnection != "" {
		t.Fatalf("Connection must be stripped; upstream saw %q", gotConnection)
	}
	if gotTE != "" {
		t.Fatalf("TE must be stripped; upstream saw %q", gotTE)
	}
}

func TestChatCompletionsRelaysSSEByteForByte(t *testing.T) {
	const sse = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer up.Close()

	s := New("127.0.0.1:4141")
	s.SetToken("t")
	s.SetProvider(&upstream.SingleUpstream{Key: "k", Detect: func(string) bool { return false }})
	s.upstreamBaseOverride = up.URL // test seam: force base

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	req.Header.Set("Authorization", "Bearer t")
	s.Handler().ServeHTTP(rec, req)

	if rec.Body.String() != sse {
		t.Fatalf("relay must be byte-identical.\nwant %q\ngot  %q", sse, rec.Body.String())
	}
}

// /v1/completions is a plain bypass forward (no injection, no mutation): it must
// relay the upstream body verbatim too.
func TestCompletionsBypassRelaysVerbatim(t *testing.T) {
	const body = "{\"choices\":[{\"text\":\"plain\"}]}"
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, body)
	}))
	defer up.Close()

	s := New("127.0.0.1:4141")
	s.SetProvider(&upstream.SingleUpstream{Key: "k", Detect: func(string) bool { return false }})
	s.upstreamBaseOverride = up.URL

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/completions", strings.NewReader(`{"prompt":"x"}`))
	s.Handler().ServeHTTP(rec, req)

	if rec.Body.String() != body {
		t.Fatalf("bypass relay must be byte-identical.\nwant %q\ngot  %q", body, rec.Body.String())
	}
	if gotPath != "/v1/completions" {
		t.Fatalf("bypass must forward to /v1/completions, got %q", gotPath)
	}
}

// A client disconnect (cancelled request context) must cancel the upstream call.
func TestClientDisconnectCancelsUpstream(t *testing.T) {
	upstreamCancelled := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if fl, ok := w.(http.Flusher); ok {
			_, _ = io.WriteString(w, "data: start\n\n")
			fl.Flush()
		}
		// Block until the (forwarded) client context is cancelled.
		<-r.Context().Done()
		close(upstreamCancelled)
	}))
	defer up.Close()

	s := New("127.0.0.1:4141")
	s.SetProvider(&upstream.SingleUpstream{Key: "k", Detect: func(string) bool { return false }})
	s.upstreamBaseOverride = up.URL

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"stream":true}`)).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		s.Handler().ServeHTTP(rec, req)
		close(done)
	}()

	// Give the relay a moment to reach the upstream, then disconnect.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-upstreamCancelled:
		// good: upstream saw the cancellation propagate from the client.
	case <-time.After(2 * time.Second):
		t.Fatal("client disconnect did not cancel the upstream request")
	}
	<-done
}
