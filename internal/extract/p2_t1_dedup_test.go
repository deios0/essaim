package extract

import (
	"testing"
	"time"
)

// P2-3: a T1 draft correction seen more than once (a client retry or a duplicate
// send of the SAME request) must not inflate the reinforce count. The FIRST T1
// sighting produces a draft (Status "draft" → the learn loop reinforces); a
// NON-INDEPENDENT repeat of the identical correction must NOT return Status
// "draft" again (which would trigger a second, spurious reinforce and can wrongly
// promote the draft). This mirrors the T0 sigil seen-set: a duplicate is a no-op
// (WrotePath=="" and non-"draft"/"active"), so the learn loop does not reinforce.
func TestT1DraftDedupesRepeatedCorrection(t *testing.T) {
	e := New(t.TempDir(), Config{})
	ex := Exchange{
		UserText:      "always prefer composition over inheritance because it is the rule",
		AssistantText: "ok",
	}

	// Sighting #1 — the draft-creating write.
	first := e.Process(ex)
	if first.Status != "draft" {
		t.Fatalf("first T1 sighting must stage a draft, got %+v (skipped=%q)", first, first.Skipped)
	}
	if first.WrotePath == "" {
		t.Fatalf("first T1 sighting must write a draft path, got %+v", first)
	}

	// Sighting #2 — the identical correction, non-independent (retry/duplicate).
	// It must NOT return Status "draft" (would drive a spurious reinforce). A
	// duplicate is a clean no-op the learn loop ignores.
	second := e.Process(ex)
	if second.Status == "draft" || second.Status == "active" {
		t.Fatalf("a repeated identical T1 correction must NOT re-report draft/active "+
			"(it would inflate the reinforce count); got %+v", second)
	}
	if second.WrotePath != "" {
		t.Fatalf("a deduped T1 repeat must not report a fresh write path; got %+v", second)
	}
}

// P2-3: two DISTINCT T1 corrections are independent and BOTH stage drafts — the
// dedup keys on the correction identity+content, it does not swallow a genuinely
// different correction.
func TestT1DraftDedupAllowsDistinctCorrections(t *testing.T) {
	e := New(t.TempDir(), Config{})
	a := e.Process(Exchange{
		UserText:      "always prefer composition over inheritance because it is the rule",
		AssistantText: "ok",
	})
	b := e.Process(Exchange{
		UserText:      "you must never use global mutable state because it is a rule",
		AssistantText: "ok",
	})
	if a.Status != "draft" || b.Status != "draft" {
		t.Fatalf("two distinct T1 corrections must both stage drafts; a=%+v b=%+v", a, b)
	}
	if a.WrotePath == b.WrotePath {
		t.Fatalf("two distinct corrections must not collapse to the same draft; a=%q b=%q", a.WrotePath, b.WrotePath)
	}
}

// P2-3: the SAME correction restated in a LATER conversation (past the retry
// window) is an INDEPENDENT sighting — it must re-report Status "draft" so the
// learn loop reinforces it (this is what legitimately promotes a rule). Only a
// repeat WITHIN the window (a retry) is deduped.
func TestT1DraftReinforcesIndependentRepeatPastWindow(t *testing.T) {
	e := New(t.TempDir(), Config{})
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	clock := base
	e.SetNow(func() time.Time { return clock })

	ex := Exchange{
		UserText:      "always prefer composition over inheritance because it is the rule",
		AssistantText: "ok",
	}

	// Sighting #1 — draft.
	if r := e.Process(ex); r.Status != "draft" {
		t.Fatalf("first sighting must stage a draft, got %+v", r)
	}
	// A retry a few seconds later — deduped.
	clock = base.Add(3 * time.Second)
	if r := e.Process(ex); r.Status == "draft" || r.Status == "active" {
		t.Fatalf("a retry within the window must be deduped, got %+v", r)
	}
	// The same preference restated well past the window — INDEPENDENT sighting,
	// must re-report draft so the lifecycle reinforces it.
	clock = base.Add(T1DedupWindow + time.Minute)
	if r := e.Process(ex); r.Status != "draft" {
		t.Fatalf("an independent restatement past the retry window must re-report draft "+
			"(so it reinforces and can promote), got %+v (skipped=%q)", r, r.Skipped)
	}
}
