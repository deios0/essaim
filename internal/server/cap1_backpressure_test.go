package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// streamObserver is a flushing ResponseWriter that records the client byte stream
// and signals (via gotAll) the moment the full expected payload has been written
// — so a test can prove the client received every byte WITHOUT waiting on the
// off-path reassembler.
type streamObserver struct {
	mu     sync.Mutex
	hdr    http.Header
	body   bytes.Buffer
	want   int
	gotAll chan struct{}
	closed bool
}

func newStreamObserver(want int) *streamObserver {
	return &streamObserver{hdr: make(http.Header), want: want, gotAll: make(chan struct{})}
}
func (o *streamObserver) Header() http.Header { return o.hdr }
func (o *streamObserver) WriteHeader(int)     {}
func (o *streamObserver) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.body.Write(p)
	if !o.closed && o.body.Len() >= o.want {
		o.closed = true
		close(o.gotAll)
	}
	return len(p), nil
}
func (o *streamObserver) Flush() {}
func (o *streamObserver) got() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.body.String()
}

var _ http.Flusher = (*streamObserver)(nil)

// CAP-1: a deliberately-SLOW/blocked off-path reassembler must NOT delay or alter
// the verbatim client byte stream. We block the drain goroutine entirely (the
// teeDrainHook waits on a gate the test controls), serve the request, and assert
// the client received EVERY byte byte-exactly while the reassembler was still
// blocked — proving the tee is non-blocking (the parse is off the relay path).
// Then we release the gate so finish() can join cleanly.
func TestSlowReassemblerDoesNotDelayClientStream(t *testing.T) {
	// Build the upstream SSE payload. It MUST span many relay reads (>4096 bytes,
	// the relay's read buffer) so that an inline/synchronous tee blocking on the
	// FIRST chunk would leave the client missing later bytes — that is the
	// backpressure this test detects.
	const events = 1000
	var sb strings.Builder
	for i := 0; i < events; i++ {
		sb.WriteString("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")
	sse := sb.String()

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

	// Block the off-path reassembler drain on a gate. While the gate is closed the
	// reassembler makes NO progress — if the tee were synchronous/inline this would
	// stall the client stream.
	gate := make(chan struct{})
	var once sync.Once
	teeDrainHook = func() { <-gate }
	t.Cleanup(func() { teeDrainHook = nil })

	obs := newStreamObserver(len(sse))
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"what database should I use?"}]}`))

	done := make(chan struct{})
	go func() {
		s.Handler().ServeHTTP(obs, req)
		close(done)
	}()

	// The client must receive ALL bytes even though the reassembler is BLOCKED.
	select {
	case <-obs.gotAll:
		// good — the client stream was delivered without waiting on the slow tee.
	case <-time.After(3 * time.Second):
		once.Do(func() { close(gate) })
		t.Fatal("client stream was delayed by the slow reassembler (CAP-1 backpressure)")
	}

	// Byte-exactness: the client stream is verbatim despite the slow tee.
	if got := obs.got(); got != sse {
		t.Fatalf("client stream must be byte-identical despite the slow tee:\nwant %q\ngot  %q", sse, got)
	}

	// Release the reassembler so finish() can join the drain and complete.
	once.Do(func() { close(gate) })
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ServeHTTP did not complete after releasing the reassembler")
	}

	// The capture still assembled off-path once the drain caught up.
	caps := sink.all()
	if len(caps) != 1 || caps[0].AssistantText != strings.Repeat("x", events) {
		t.Fatalf("the off-path tee must still assemble the assistant text (len=%d): %+v", len(caps[0].AssistantText), caps)
	}
}
