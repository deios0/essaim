package server

import (
	"sync/atomic"

	"oikos/internal/capture"
	"oikos/internal/inject"
)

// CaptureSink consumes a finished Capture off the response path. The server is
// agnostic to what the sink does (extract/learn); cmd/oikos wires the real one.
// A nil sink disables capture (the M2 default — pure verbatim relay).
type CaptureSink interface {
	Enqueue(capture.Capture)
}

// captureCounters are the /health-surfaced capture meters (BR-A2-16). They are
// atomic so the off-path tee and the /health handler never race.
type captureCounters struct {
	droppedBytes atomic.Int64
	dropped      atomic.Int64
	enqueued     atomic.Int64
}

// captureCtx threads the pre-injection snapshot + the off-path reassembler
// through a single relay. A nil captureCtx means "do not capture this relay"
// (the retry-overflow relay, the /v1/completions bypass, and every fail-open
// path pass nil — BR-A2-15). It is the ONLY thing the M3 tee adds to the M2
// relay; the client write path is byte-for-byte unchanged.
//
// CAP-1 (non-blocking tee): the SSE reassembly does NOT run inline on the relay
// goroutine. The relay hands each just-written chunk to a bounded buffered
// channel via a NON-BLOCKING send; a dedicated drain goroutine owns the
// reassembler and parses OFF the relay path. On a full channel the chunk is
// DROPPED (droppedBytes += len, lossy=true) — the relay NEVER blocks, so the
// reassembler can never backpressure the next resp.Body.Read / client write.
// finish() closes the channel and JOINS the drain goroutine, so the assembled
// text is complete+race-free before the Capture is built.
type captureCtx struct {
	snap   inject.Snapshot
	stream *capture.StreamReassembler // owned EXCLUSIVELY by the drain goroutine
	// lossy is set when the tee dropped bytes for THIS relay (the assistant_text
	// is incomplete → T2 is refused downstream, M3-R11). Touched ONLY by the relay
	// goroutine: the non-blocking-send drop path in teeStreamBytes, and the
	// post-join reconcile in joinStreamDrain/finish (both run on the relay
	// goroutine, after the drain goroutine has exited at the channel-close join
	// barrier) — never concurrently.
	lossy bool
	// teeCh is the bounded hand-off from the relay to the drain goroutine. nil
	// until the first stream tee starts the goroutine (lazy: non-stream relays
	// never allocate it).
	teeCh   chan []byte
	teeDone chan struct{}
	dropped *atomic.Int64 // server droppedBytes meter (MINOR-6), shared pointer
	// nonStreamBuf accumulates a bounded copy of a non-streaming JSON body so the
	// off-path consumer can read choices[0].message.content (BR-A2-9). Capped.
	nonStreamBuf []byte
	nonStreamCap bool
}

// maxNonStreamCapture caps the off-path non-stream body copy (a non-stream
// response is small; this only guards a pathological body).
const maxNonStreamCapture = 4 << 20 // 4 MiB

// teeQueueDepth bounds the relay→reassembler hand-off (CAP-1). A reassembler
// that falls behind drops chunks (drop-on-full) rather than backpressuring the
// client. Sized so a transient parse hiccup is absorbed without dropping, while
// a genuinely-stalled reassembler cannot grow memory without bound.
const teeQueueDepth = 256

// newCaptureCtx builds a capture context for a primary relay. Returns nil when
// capture is disabled (no sink) or the snapshot is empty (nothing was built /
// honest miss with no clean messages).
func (s *Server) newCaptureCtx(snap inject.Snapshot) *captureCtx {
	if s.captureSink == nil {
		return nil
	}
	if snap.CleanMessagesJSON == nil {
		return nil // no trusted clean messages → nothing safe to learn from
	}
	return &captureCtx{snap: snap, stream: capture.NewStreamReassembler(), dropped: &s.capture.droppedBytes}
}

// teeDrainHook, when non-nil, is called by the drain goroutine before processing
// EACH chunk. It is a TEST SEAM ONLY (nil in production) — used to make the
// reassembler deliberately slow so a test can prove the slow drain does NOT delay
// the verbatim client stream (CAP-1). It must never be set outside tests.
var teeDrainHook func()

// startStreamDrain lazily launches the drain goroutine that OWNS the reassembler
// and parses off the relay path (CAP-1). Idempotent.
func (cc *captureCtx) startStreamDrain() {
	if cc.teeCh != nil {
		return
	}
	cc.teeCh = make(chan []byte, teeQueueDepth)
	cc.teeDone = make(chan struct{})
	go func() {
		defer close(cc.teeDone)
		for b := range cc.teeCh {
			if teeDrainHook != nil {
				teeDrainHook()
			}
			cc.stream.Write(b) // the ONLY writer of cc.stream
		}
	}()
}

// teeStreamBytes hands the just-written client bytes to the off-path drain
// goroutine via a NON-BLOCKING send (CAP-1). The bytes were ALREADY written +
// flushed to the client before this is called, AND the send never blocks, so the
// reassembler can never delay the next read/write of the verbatim client stream.
// On a full channel the chunk is dropped (droppedBytes += len, lossy=true).
func (cc *captureCtx) teeStreamBytes(buf []byte) {
	if cc == nil || cc.stream == nil {
		return
	}
	cc.startStreamDrain()
	// Copy: the relay reuses its read buffer for the next Read, so the bytes must
	// be owned by the channel item. (A copy is O(chunk) on the relay path, but it
	// is a plain memmove — it does NO parsing/allocation-heavy work, unlike the
	// old inline reassembly; the parse is what moved off-path.)
	cp := make([]byte, len(buf))
	copy(cp, buf)
	select {
	case cc.teeCh <- cp:
	default:
		// Reassembler fell behind → drop this chunk rather than block the client.
		if cc.dropped != nil {
			cc.dropped.Add(int64(len(cp)))
		}
		cc.lossy = true
	}
}

// joinStreamDrain closes the hand-off channel and waits for the drain goroutine
// to finish, so cc.stream is complete and race-free before finish() reads it.
// Safe to call when no stream drain was ever started (non-stream / empty relay).
func (cc *captureCtx) joinStreamDrain() {
	if cc == nil || cc.teeCh == nil {
		return
	}
	close(cc.teeCh)
	<-cc.teeDone
	cc.teeCh = nil // idempotent: a second join is a no-op
	// Reconcile cap/overflow lossy AFTER the drain has processed everything.
	if cc.stream.CapHit() || cc.stream.Overflowed() {
		cc.lossy = true
	}
}

// teeNonStreamBytes copies buf into the bounded non-stream buffer (capped).
func (cc *captureCtx) teeNonStreamBytes(buf []byte) {
	if cc == nil {
		return
	}
	if cc.nonStreamCap {
		return
	}
	if len(cc.nonStreamBuf)+len(buf) > maxNonStreamCapture {
		remain := maxNonStreamCapture - len(cc.nonStreamBuf)
		if remain > 0 {
			cc.nonStreamBuf = append(cc.nonStreamBuf, buf[:remain]...)
		}
		cc.nonStreamCap = true
		cc.lossy = true
		return
	}
	cc.nonStreamBuf = append(cc.nonStreamBuf, buf...)
}

// finish assembles the Capture and enqueues it to the sink (off the response
// path). isStream selects the assistant-text source; partial marks an
// EOF-before-[DONE] / client-disconnect / cap. It enforces the HARD INVARIANT
// (no complete oikos block in any captured body or the assistant text) AFTER
// redaction, dropping the capture on a violation.
func (s *Server) finish(cc *captureCtx, isStream, partial bool) {
	if cc == nil || s.captureSink == nil {
		return
	}
	var assistant string
	if isStream {
		// CAP-1: drain + join the off-path reassembler before reading its text, so
		// the assembled assistant_text is complete and there is no concurrent
		// access to cc.stream (the drain goroutine is the only other toucher).
		cc.joinStreamDrain()
		assistant = cc.stream.Text()
		if !cc.stream.SawDone() {
			partial = true
		}
		if cc.stream.CapHit() {
			partial = true
		}
		if cc.stream.Overflowed() {
			partial = true // CAP-2: a dropped over-long line ⇒ assistant_text incomplete
		}
	} else {
		assistant = capture.AssistantTextFromNonStream(cc.nonStreamBuf)
		if cc.nonStreamCap {
			partial = true
		}
	}

	c := capture.Capture{
		OriginalMessages: capture.ParseCleanMessages(cc.snap.CleanMessagesJSON),
		AssistantText:    assistant,
		MatchedRuleIDs:   cc.snap.MatchedRuleIDs,
		Model:            cc.snap.Model,
		Stream:           isStream,
		Partial:          partial,
		Lossy:            cc.lossy,
	}
	// Redact credentials over the FLATTENED contents + assistant text (§4.7),
	// then enforce the hard invariant.
	c.Redact()
	if c.ViolatesHardInvariant() {
		s.capture.dropped.Add(1)
		return // a complete oikos block leaked into the capture → drop it
	}
	// A sink that reports acceptance (the learner's non-blocking TryEnqueue) lets us
	// count only ACCEPTED captures — a full-queue DROP must not be counted as
	// enqueued (P2: enqueued overcounted drops, hiding silent moat degradation).
	// Optional interface so any other/legacy sink keeps the plain Enqueue path.
	if te, ok := s.captureSink.(interface {
		TryEnqueue(capture.Capture) bool
	}); ok {
		if te.TryEnqueue(c) {
			s.capture.enqueued.Add(1)
		}
		return
	}
	s.captureSink.Enqueue(c)
	s.capture.enqueued.Add(1)
}
