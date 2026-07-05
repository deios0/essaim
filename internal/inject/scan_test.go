package inject

import (
	"testing"
	"time"
)

// F-D [belt-suspenders]: forward-progress guard in splitArrayElements. After
// scanValue returns a degenerate span (end < start) or valNone, the splitter must
// fail open (return nil,false) BEFORE appending, so a CPU-DoS hang can never
// return even if the bodyWellFormed gate upstream is ever weakened. Defense in
// depth: today bodyWellFormed catches these envelopes, but the splitter must be
// self-safe.

// splitNoHang runs splitArrayElements under a hard wall-clock budget on its own
// goroutine; a hang (no forward progress) trips the timeout instead of spinning.
func splitNoHang(t *testing.T, body []byte, arrStart, arrEnd int) (spans [][2]int, ok bool) {
	t.Helper()
	type res struct {
		spans [][2]int
		ok    bool
	}
	done := make(chan res, 1)
	go func() {
		s, o := splitArrayElements(body, arrStart, arrEnd)
		done <- res{s, o}
	}()
	select {
	case r := <-done:
		return r.spans, r.ok
	case <-time.After(2 * time.Second):
		t.Fatalf("splitArrayElements hung (no forward progress) on %q", body)
		return nil, false
	}
}

// locateArr is a tiny helper that finds the messages array bounds the way Build
// does, so the F-D tests feed splitArrayElements the same offsets it sees live.
func locateArr(t *testing.T, body []byte) (int, int) {
	t.Helper()
	s, e, ok := locateArray(body, "messages")
	if !ok {
		t.Fatalf("locateArray failed on %q", body)
	}
	return s, e
}

// F-D (a): `{"messages":[}]}` — an empty-then-close array. The `}` where an
// element is expected is a degenerate value; the splitter must fail open, never
// hang.
func TestSplitForwardProgressEmptyThenBrace(t *testing.T) {
	body := []byte(`{"messages":[}]}`)
	s, e, ok := locateArray(body, "messages")
	if !ok {
		// If locate itself fails open that's fine — the point is no hang at split.
		return
	}
	spans, sok := splitNoHang(t, body, s, e)
	if sok {
		t.Fatalf("malformed array must fail open (ok=false), got spans=%v", spans)
	}
}

// F-D (b): `{"messages":[{...},}]}` — a valid element followed by a stray `}`
// where the next element should start. The splitter must make forward progress
// past the first element then fail open on the degenerate second value, never
// hang.
func TestSplitForwardProgressElementThenBrace(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"x"},}]}`)
	s, e := locateArr(t, body)
	spans, sok := splitNoHang(t, body, s, e)
	if sok {
		t.Fatalf("array with a stray brace element must fail open, got spans=%v", spans)
	}
}

// Sanity: a well-formed array still splits to the right number of spans (the
// guard must not regress the happy path).
func TestSplitForwardProgressWellFormedStillSplits(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"a"},{"role":"system","content":"b"}]}`)
	s, e := locateArr(t, body)
	spans, ok := splitNoHang(t, body, s, e)
	if !ok || len(spans) != 2 {
		t.Fatalf("well-formed array must split to 2 spans, got ok=%v spans=%v", ok, spans)
	}
}
