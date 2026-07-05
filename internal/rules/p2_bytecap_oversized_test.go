package rules

import (
	"strings"
	"testing"
)

// P2-4: the byte-cap always keeps ≥1 rule (a lone guardrail is NEVER starved —
// invariant preserved). But an UNBOUNDED oversized first rule blew the eager
// budget: its single rendered line alone could be many KB, so the wrapped block
// exceeded EagerBytes by an arbitrary amount, defeating the whole point of the
// cap. The fix keeps the first rule (same ID/Title/Kind — guardrail identity
// intact) but TRUNCATES its rendered body so the final wrapped block fits the
// cap. This bounds the eager budget without ever dropping the guardrail.
func TestBloatGuardOversizedFirstRuleBounded(t *testing.T) {
	huge := strings.Repeat("y", 10000) // alone dwarfs the cap
	guard := Rule{ID: "g", Title: "Guard", Body: huge, Kind: "guardrail", Confidence: 0.9, Weight: 0.9}
	const cap = 4096
	res, err := BloatGuard([]scored{scoredOf(guard, 0.9)}, GuardConfig{EagerBytes: cap})
	if err != nil {
		t.Fatalf("BloatGuard: %v", err)
	}
	// Invariant: the guardrail is STILL kept (never starved).
	if len(res.Kept) != 1 || res.Kept[0].ID != "g" {
		t.Fatalf("oversized guardrail must still be kept (≥1): %+v", res.Kept)
	}
	if res.Kept[0].Kind != "guardrail" || res.Kept[0].Title != "Guard" {
		t.Fatalf("kept guardrail must retain identity (kind+title): %+v", res.Kept[0])
	}
	// The wrapped block must now fit the eager budget (bug: it did not).
	wrapped := WrapBlock(RenderBody(res.Kept))
	if len(wrapped) > cap {
		t.Fatalf("wrapped block %d bytes exceeds cap %d — oversized first rule not bounded", len(wrapped), cap)
	}
	// The truncation must be VISIBLE (flagged), not a silent cut.
	if !strings.Contains(wrapped, truncationMarker) {
		t.Fatalf("truncated rule must carry a visible marker %q; wrapped=%q", truncationMarker, wrapped)
	}
}

// P2-4: a first rule that FITS the cap must be left byte-identical (no spurious
// truncation of a normally-sized rule).
func TestBloatGuardFittingFirstRuleUntouched(t *testing.T) {
	guard := Rule{ID: "g", Title: "Guard", Body: "keep this exact body", Kind: "guardrail", Confidence: 0.9, Weight: 0.9}
	res, err := BloatGuard([]scored{scoredOf(guard, 0.9)}, GuardConfig{EagerBytes: 4096})
	if err != nil {
		t.Fatalf("BloatGuard: %v", err)
	}
	if len(res.Kept) != 1 || res.Kept[0].Body != "keep this exact body" {
		t.Fatalf("a fitting first rule must be untouched: %+v", res.Kept)
	}
	if strings.Contains(WrapBlock(RenderBody(res.Kept)), truncationMarker) {
		t.Fatalf("a fitting rule must NOT be marked truncated")
	}
}
