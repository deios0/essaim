package learn

import (
	"testing"

	"oikos/internal/capture"
	"oikos/internal/extract"
)

// P2-1: Enqueue drops on a full queue, but the drop was INVISIBLE to the caller
// (the server), which then unconditionally counted the capture as enqueued —
// overcounting captures_enqueued by exactly the dropped ones. The fix exposes the
// accept/drop outcome (TryEnqueue returns whether the capture was accepted) so the
// integrator counts enqueued ONLY on acceptance, and the Dropped() counter is
// accurate for /health.

// TryEnqueue reports acceptance truthfully: it returns true up to capacity and
// false once the queue is full, and the false count matches Dropped().
func TestTryEnqueueReportsAcceptance(t *testing.T) {
	l := New(t.TempDir(), extract.Config{})
	accepted, dropped := 0, 0
	total := QueueSize + 100
	for i := 0; i < total; i++ {
		if l.TryEnqueue(capture.Capture{}) {
			accepted++
		} else {
			dropped++
		}
	}
	if accepted != QueueSize {
		t.Fatalf("accepted=%d, want exactly QueueSize=%d (the buffered capacity)", accepted, QueueSize)
	}
	if dropped != total-QueueSize {
		t.Fatalf("dropped=%d, want %d", dropped, total-QueueSize)
	}
	// Dropped() (the /health counter) must equal the drops the caller observed —
	// no over/undercount.
	if l.Dropped() != int64(dropped) {
		t.Fatalf("Dropped() = %d, want %d (must match observed drops for /health)", l.Dropped(), dropped)
	}
}

// A dropped capture must NOT be counted as enqueued: simulate the integrator's
// wiring (count enqueued only when TryEnqueue returns true) and confirm the
// enqueued tally excludes drops.
func TestDroppedNotCountedAsEnqueued(t *testing.T) {
	l := New(t.TempDir(), extract.Config{})
	enqueued := 0
	total := QueueSize + 50
	for i := 0; i < total; i++ {
		if l.TryEnqueue(capture.Capture{}) {
			enqueued++ // the integrator increments ONLY on acceptance
		}
	}
	if enqueued != QueueSize {
		t.Fatalf("enqueued (accept-gated) = %d, want %d — a dropped capture must not be counted", enqueued, QueueSize)
	}
	if int64(enqueued)+l.Dropped() != int64(total) {
		t.Fatalf("enqueued(%d) + dropped(%d) must equal total offered(%d)", enqueued, l.Dropped(), total)
	}
}

// Backward-compat: the void Enqueue (the current server.CaptureSink method) still
// works and still counts drops, so nothing regresses before the integrator wires
// TryEnqueue.
func TestEnqueueVoidStillDropsAndCounts(t *testing.T) {
	l := New(t.TempDir(), extract.Config{})
	for i := 0; i < QueueSize+40; i++ {
		l.Enqueue(capture.Capture{})
	}
	if l.Dropped() < 40 {
		t.Fatalf("void Enqueue must still drop+count on a full queue; dropped=%d", l.Dropped())
	}
}
