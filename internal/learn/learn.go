// Package learn wires the M3 capture → extract → lifecycle loop into a single
// CaptureSink the server can call. It owns a bounded async queue (drop-on-full,
// so the response path is NEVER blocked — locked invariant 2), an Extractor, and
// a lifecycle Sweeper. It also implements the server's CaptureStats interface so
// /health surfaces drafts_pending + the T2 cost meter.
package learn

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"oikos/internal/capture"
	"oikos/internal/extract"
	"oikos/internal/lifecycle"
)

// QueueSize bounds the async capture queue. On a full queue, captures are
// DROPPED (the client stream is never delayed — BR-A2-13).
const QueueSize = 256

// Learner is the off-path learning loop: an async queue draining captures into
// the Extractor (T0/T1/T2) + reinforcing the lifecycle. It satisfies
// server.CaptureSink (Enqueue) and server.CaptureStats (DraftsPending /
// ExtractCost*).
type Learner struct {
	vault   string
	ex      *extract.Extractor
	sweeper *lifecycle.Sweeper

	queue   chan capture.Capture
	dropped atomic.Int64
}

// New constructs a Learner over the vault with the given extract config. It does
// NOT start the worker; call Start.
func New(vault string, cfg extract.Config) *Learner {
	return &Learner{
		vault:   vault,
		ex:      extract.New(vault, cfg),
		sweeper: lifecycle.New(vault),
		queue:   make(chan capture.Capture, QueueSize),
	}
}

// Enqueue is the server.CaptureSink entry point. It NEVER blocks: a full queue
// drops the capture (and increments the dropped counter). The client stream has
// already been delivered verbatim before this is called. It delegates to
// TryEnqueue, discarding the accept/drop signal for callers that don't need it.
func (l *Learner) Enqueue(c capture.Capture) { _ = l.TryEnqueue(c) }

// TryEnqueue is the accept-aware enqueue (P2-1). It NEVER blocks and returns
// whether the capture was ACCEPTED into the queue (true) or DROPPED because the
// queue was full (false, and the dropped counter is incremented). The caller (the
// server tee) must count captures_enqueued ONLY when this returns true — the old
// void Enqueue gave the caller no drop signal, so the server counted every
// offered capture as enqueued, overcounting by exactly the dropped ones. A
// dropped capture is reflected in Dropped() (surfaced on /health), never in the
// enqueued tally.
//
// INTEGRATOR NOTE: to wire the fix, the server's CaptureSink interface should add
// `TryEnqueue(capture.Capture) bool` (or replace Enqueue's signature), and
// internal/server/capture_tap.go must gate `s.capture.enqueued.Add(1)` on the
// bool — e.g. `if s.captureSink.TryEnqueue(c) { s.capture.enqueued.Add(1) }` —
// and surface l.Dropped() as a `captures_dropped_queue` (or similar) key in
// /health via the CaptureStats interface (add a QueueDropped() int64 accessor;
// this package already exposes Dropped()). Until then the void Enqueue keeps
// working (drops are still counted in Dropped()), so nothing regresses.
func (l *Learner) TryEnqueue(c capture.Capture) bool {
	select {
	case l.queue <- c:
		return true
	default:
		l.dropped.Add(1) // queue full → drop (purity of the stream wins)
		return false
	}
}

// Dropped reports the number of captures dropped due to a full queue. Exposed for
// /health so the drop is OBSERVABLE (P2-1) — a silently-dropped capture with no
// counter was the invisible half of the bug.
func (l *Learner) Dropped() int64 { return l.dropped.Load() }

// QueueDropped is the CaptureStats-facing name for the queue-drop counter (P2-1),
// an alias of Dropped() so the integrator can surface it on /health via the
// stats interface without renaming the existing accessor.
func (l *Learner) QueueDropped() int64 { return l.dropped.Load() }

// Start launches the async worker + the lifecycle timer. The worker drains the
// queue and runs the extractor; the timer runs the lifecycle sweep. Both stop
// when stop is closed.
func (l *Learner) Start(stop <-chan struct{}) {
	// On startup, run one sweep (the on-startup sweep, §3).
	_, _ = l.sweeper.Sweep()

	go l.worker(stop)
	go l.sweepLoop(stop)
}

// ProcessOne synchronously drains+processes one capture (test seam; the demo
// uses it to make the learn step deterministic). Returns the extractor result.
func (l *Learner) ProcessOne(c capture.Capture) extract.Result {
	return l.process(c)
}

// SetExtractNow overrides the extractor's clock (test seam). The extractor's
// `now` gates the T1 retry-dedup window (P2-3): two INDEPENDENT sightings of the
// same correction must be spaced past the window to reinforce, so a test that
// drives the real Extractor→Reinforce→Sweep promote seam advances this clock
// between sightings to make them independent (and deterministic).
func (l *Learner) SetExtractNow(fn func() time.Time) { l.ex.SetNow(fn) }

// Sweep runs one lifecycle pass synchronously (test/demo seam). The async loop
// runs this on a timer; exposing it lets a test drive a deterministic
// dedup/reinforce/promote cycle through the REAL Extractor→Reinforce→Sweep seam.
func (l *Learner) Sweep() (lifecycle.SweepResult, error) { return l.sweeper.Sweep() }

func (l *Learner) worker(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			// Best-effort DRAIN on shutdown: process captures already in the queue
			// (e.g. produced by in-flight relays during the graceful HTTP drain)
			// before exiting, so a capture enqueued moments before stop is not lost
			// (codex review). Non-blocking — returns as soon as the queue is empty.
			for {
				select {
				case c := <-l.queue:
					l.process(c)
				default:
					return
				}
			}
		case c := <-l.queue:
			l.process(c)
		}
	}
}

// process runs the extractor on one capture and, when a draft already exists for
// the same title-hash, reinforces it via the lifecycle (off-path).
func (l *Learner) process(c capture.Capture) extract.Result {
	ex := c.ToExchange()
	res := l.ex.Process(ex)
	// A correction that matched an existing rule's title reinforces it (the
	// lifecycle promotes draft→live on reinforce-twice). We reinforce on every
	// staged/active write so a repeated correction crosses the promote threshold.
	// The LATEST correction's quality hint flows into the reinforce so the promote
	// gate can require hint >= new (RISK-4 / BR-A1.5-2) — not merely the count.
	if res.Status == "draft" || res.Status == "active" {
		l.sweeper.Reinforce(titleFromExchange(ex), lifecycle.Hint(res.Hint))
	}
	return res
}

// titleFromExchange derives the rule title the extractor would use, so the
// lifecycle reinforce keys on the SAME title-hash.
func titleFromExchange(ex extract.Exchange) string {
	// The extractor titles a sigil from its payload and a T1 draft from the user
	// text; both reduce to the first line of the relevant text. For reinforce we
	// use the user text's first line (close enough for the common single-line
	// correction; a sigil's title is its payload, also the first user line here).
	return extract.TitleForReinforce(ex)
}

func (l *Learner) sweepLoop(stop <-chan struct{}) {
	t := time.NewTicker(lifecycle.DefaultInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			_, _ = l.sweeper.Sweep()
		}
	}
}

// DraftsPending counts the `.md` files in the vault's _inbox/ (server
// CaptureStats). Best-effort; a read error yields 0.
func (l *Learner) DraftsPending() int {
	inbox := filepath.Join(l.vault, extract.OIKOS_DRAFT_DIR)
	n := 0
	_ = filepath.WalkDir(inbox, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && filepath.Ext(p) == ".md" {
			n++
		}
		return nil
	})
	return n
}

// ExtractCostToday is the T2 per-day spend (server CaptureStats). It reads the
// SAME meter the extractor writes (the extractor owns the live cost meter).
func (l *Learner) ExtractCostToday() float64 { return l.ex.CostToday() }

// ExtractCostCap is the configured per-day cost cap (server CaptureStats).
func (l *Learner) ExtractCostCap() float64 { return l.ex.CostCap() }
