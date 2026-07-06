package learn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"essaim/internal/capture"
	"essaim/internal/extract"
)

func countMD(t *testing.T, root string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(p, ".md") {
			n++
		}
		return nil
	})
	return n
}

// End-to-end (off-path): a sigil capture lands an active rule in remembered/.
func TestLearnerSigilEndToEnd(t *testing.T) {
	vault := t.TempDir()
	l := New(vault, extract.Config{})
	c := capture.Capture{
		OriginalMessages: []capture.ChatMessage{
			{Role: "user", Content: "/remember Always use PostgreSQL, never MySQL"},
		},
		AssistantText: "Understood.",
	}
	res := l.ProcessOne(c)
	if res.Status != "active" {
		t.Fatalf("sigil capture must write an active rule, got %+v", res)
	}
	if countMD(t, filepath.Join(vault, "remembered")) != 1 {
		t.Fatal("exactly one active rule expected under remembered/")
	}
	doc, _ := os.ReadFile(res.WrotePath)
	if !strings.Contains(string(doc), "Always use PostgreSQL, never MySQL") {
		t.Fatalf("rule body wrong:\n%s", doc)
	}
}

// A T1 correction with a preference signal lands a draft in _inbox/.
func TestLearnerT1DraftEndToEnd(t *testing.T) {
	vault := t.TempDir()
	l := New(vault, extract.Config{})
	c := capture.Capture{
		OriginalMessages: []capture.ChatMessage{
			{Role: "user", Content: "always prefer composition over inheritance because it is the rule"},
		},
		AssistantText: "ok",
	}
	res := l.ProcessOne(c)
	if res.Status != "draft" {
		t.Fatalf("a preference correction must stage a draft, got %+v (skipped=%q)", res, res.Skipped)
	}
	if l.DraftsPending() != 1 {
		t.Fatalf("DraftsPending = %d, want 1", l.DraftsPending())
	}
}

// Enqueue never blocks and drops on a full queue (the response path is never
// blocked — BR-A2-13).
func TestLearnerEnqueueDropsOnFull(t *testing.T) {
	l := New(t.TempDir(), extract.Config{})
	// Fill the queue beyond capacity WITHOUT starting the worker.
	for i := 0; i < QueueSize+50; i++ {
		l.Enqueue(capture.Capture{})
	}
	if l.Dropped() < 50 {
		t.Fatalf("a full queue must drop captures, dropped=%d", l.Dropped())
	}
}

// End-to-end through the REAL Extractor→Reinforce→Sweep seam (RISK-4/RISK-5):
// the SAME preference correction seen THREE times as INDEPENDENT sightings (the
// draft-creating write + two independent reinforces ⇒ count 3) with a hint >= new
// on every sighting promotes the draft to `live`. Two sightings (create + 1) must
// NOT promote (off-by-one).
//
// P2-3: the T1 retry-dedup window means byte-identical sightings only count as
// INDEPENDENT when spaced past extract.T1DedupWindow — so this test advances the
// extractor's clock between sightings. (A burst of retries WITHIN the window is
// deduped and never reinforces; that is covered in the extract package.)
func TestLearnerPromotesOnThreeSightingsThroughRealSeam(t *testing.T) {
	vault := t.TempDir()
	l := New(vault, extract.Config{})
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	clock := base
	l.SetExtractNow(func() time.Time { return clock })
	corr := capture.Capture{
		OriginalMessages: []capture.ChatMessage{
			{Role: "user", Content: "always prefer composition over inheritance because it is the rule"},
		},
		AssistantText: "ok",
	}

	// Sighting #1 — the draft-creating write.
	if res := l.ProcessOne(corr); res.Status != "draft" {
		t.Fatalf("first sighting must stage a draft, got %+v (skipped=%q)", res, res.Skipped)
	}
	// Sighting #2 — one INDEPENDENT reinforce (past the retry window). After this
	// the count is 2 (< 3) so a sweep must NOT promote.
	clock = clock.Add(extract.T1DedupWindow + time.Minute)
	l.ProcessOne(corr)
	if res, err := l.Sweep(); err != nil {
		t.Fatal(err)
	} else if len(res.Promoted) != 0 {
		t.Fatalf("two sightings (create+1) must not promote through the real seam; Promoted=%v", res.Promoted)
	}
	// Sighting #3 — the second INDEPENDENT reinforce (past the window again). Count
	// reaches 3 with a hint >= new ⇒ the next sweep promotes.
	clock = clock.Add(extract.T1DedupWindow + time.Minute)
	l.ProcessOne(corr)
	res, err := l.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Promoted) != 1 {
		t.Fatalf("three sightings (create+2) with hint>=new must promote exactly one rule; Promoted=%v", res.Promoted)
	}
	// Confirm the draft on disk is now live.
	var live int
	_ = filepath.WalkDir(filepath.Join(vault, extract.ESSAIM_DRAFT_DIR), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), "status: live") {
			live++
		}
		return nil
	})
	if live != 1 {
		t.Fatalf("the thrice-seen draft must be promoted to live on disk; live=%d", live)
	}
}

// The 8-let-opyta class produces no draft (false-positive aversion) end-to-end.
func TestLearnerRejectsNoise(t *testing.T) {
	vault := t.TempDir()
	l := New(vault, extract.Config{})
	l.ProcessOne(capture.Capture{
		OriginalMessages: []capture.ChatMessage{{Role: "user", Content: "I have 8 years of experience"}},
		AssistantText:    "Great.",
	})
	if l.DraftsPending() != 0 {
		t.Fatal("the 8-years-experience noise must NOT stage a draft")
	}
}

// P2-3 regression: a RETRY STORM — the identical T1 correction re-sent many times
// WITHIN the retry window (a client that retries a request) must NOT inflate the
// reinforce count and must NOT promote the draft. Only the FIRST sighting counts;
// the burst is deduped. This is the bug: before the fix, N resends drove N
// reinforces and spuriously crossed the promote threshold.
func TestLearnerRetryStormWithinWindowDoesNotPromote(t *testing.T) {
	vault := t.TempDir()
	l := New(vault, extract.Config{})
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	clock := base
	l.SetExtractNow(func() time.Time { return clock })
	corr := capture.Capture{
		OriginalMessages: []capture.ChatMessage{
			{Role: "user", Content: "always prefer composition over inheritance because it is the rule"},
		},
		AssistantText: "ok",
	}

	// First sighting stages the draft.
	if res := l.ProcessOne(corr); res.Status != "draft" {
		t.Fatalf("first sighting must stage a draft, got %+v (skipped=%q)", res, res.Skipped)
	}
	// A storm of retries, each a few seconds apart — all WITHIN the window, so all
	// deduped (no reinforce).
	for i := 0; i < 10; i++ {
		clock = clock.Add(2 * time.Second)
		if res := l.ProcessOne(corr); res.Status == "draft" || res.Status == "active" {
			t.Fatalf("retry #%d within the window must be deduped (no reinforce), got %+v", i+1, res)
		}
	}
	// Even after many retries, a sweep must NOT promote — the draft was seen once.
	res, err := l.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Promoted) != 0 {
		t.Fatalf("a retry storm within the window must NOT promote; Promoted=%v", res.Promoted)
	}
}
