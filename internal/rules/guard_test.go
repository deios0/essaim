package rules

import (
	"strings"
	"testing"
)

// scoredOf builds a scored candidate (test helper; scored is unexported).
func scoredOf(r Rule, s float64) scored { return scored{rule: r, score: s} }

// Test 34: bloatguard_bytecap — many rules summing > cap ⇒ wrapped ≤ cap,
// omitted reported, ≥1 kept.
func TestBloatGuardByteCap(t *testing.T) {
	var cands []scored
	for i := 0; i < 30; i++ {
		body := strings.Repeat("word ", 80) // ~400 bytes each
		cands = append(cands, scoredOf(Rule{ID: string(rune('a' + i)), Title: "T", Body: body, Weight: 0.9, Confidence: 0.9}, 0.9))
	}
	res, err := BloatGuard(cands, GuardConfig{EagerBytes: 4096})
	if err != nil {
		t.Fatalf("BloatGuard: %v", err)
	}
	if len(res.Kept) == 0 {
		t.Fatalf("must keep ≥1")
	}
	if res.Omitted == 0 {
		t.Fatalf("must report omitted")
	}
	wrapped := WrapBlock(RenderBody(res.Kept))
	if len(wrapped) > 4096 {
		t.Fatalf("wrapped block %d bytes exceeds cap 4096", len(wrapped))
	}
}

// Test 35: bloatguard_criticality_first — a conf=0.85 guardrail beats a conf=0.99
// non-guardrail when the cap fits one.
func TestBloatGuardCriticalityFirst(t *testing.T) {
	big := strings.Repeat("x", 200)
	guard := Rule{ID: "g", Title: "Guard", Body: big, Kind: "guardrail", Confidence: 0.85, Weight: 0.85}
	plain := Rule{ID: "p", Title: "Plain", Body: big, Kind: "", Confidence: 0.99, Weight: 0.99}
	// Cap that fits exactly one ~ (200 + 64 overhead + wrapper).
	res, err := BloatGuard([]scored{scoredOf(plain, 0.99), scoredOf(guard, 0.85)},
		GuardConfig{EagerBytes: 350})
	if err != nil {
		t.Fatalf("BloatGuard: %v", err)
	}
	if len(res.Kept) != 1 || res.Kept[0].ID != "g" {
		t.Fatalf("guardrail must be kept over higher-confidence plain: %+v", res.Kept)
	}
}

// Test 36: bloatguard_similarity_floor — all candidates below floor ⇒ ErrNoMatch.
func TestBloatGuardSimilarityFloor(t *testing.T) {
	cands := []scored{
		scoredOf(Rule{ID: "a", Title: "A", Body: "b"}, 0.4),
		scoredOf(Rule{ID: "b", Title: "B", Body: "b"}, 0.3),
	}
	_, err := BloatGuard(cands, GuardConfig{MatchFloor: 0.6})
	if err != ErrNoMatch {
		t.Fatalf("want ErrNoMatch, got %v", err)
	}
}

// Test 37: bloatguard_project_scope_local_first — local rule ranked before a
// zone-wide rule with higher raw score.
func TestBloatGuardProjectScopeLocalFirst(t *testing.T) {
	local := Rule{ID: "local", Title: "Local", Body: "lb", ProjectTag: "myproj", Confidence: 0.7, Weight: 0.7}
	zone := Rule{ID: "zone", Title: "Zone", Body: "zb", Confidence: 0.99, Weight: 0.99}
	res, err := BloatGuard(
		[]scored{scoredOf(zone, 0.99), scoredOf(local, 0.70)},
		GuardConfig{CWD: "/home/x/myproj", EagerBytes: 4096},
	)
	if err != nil {
		t.Fatalf("BloatGuard: %v", err)
	}
	if len(res.Kept) < 2 || res.Kept[0].ID != "local" {
		t.Fatalf("local rule must rank first: %+v", res.Kept)
	}
}

// Test 38: bloatguard_keeps_at_least_one_oversized_guardrail.
func TestBloatGuardKeepsOversizedGuardrail(t *testing.T) {
	huge := strings.Repeat("y", 10000) // alone exceeds the cap
	guard := Rule{ID: "g", Title: "Guard", Body: huge, Kind: "guardrail", Confidence: 0.9, Weight: 0.9}
	other := Rule{ID: "o", Title: "O", Body: "small", Confidence: 0.9, Weight: 0.9}
	res, err := BloatGuard([]scored{scoredOf(guard, 0.9), scoredOf(other, 0.9)},
		GuardConfig{EagerBytes: 4096})
	if err != nil {
		t.Fatalf("BloatGuard: %v", err)
	}
	if len(res.Kept) != 1 || res.Kept[0].ID != "g" {
		t.Fatalf("oversized guardrail must still be kept (≥1): %+v", res.Kept)
	}
	if res.Omitted != 1 {
		t.Fatalf("the other rule must be omitted: omitted=%d", res.Omitted)
	}
}

// Empty candidates ⇒ ErrNoMatch (honest miss).
func TestBloatGuardEmptyIsNoMatch(t *testing.T) {
	if _, err := BloatGuard(nil, GuardConfig{}); err != ErrNoMatch {
		t.Fatalf("empty candidates must be ErrNoMatch, got %v", err)
	}
}

// Top-k cap (default 10) limits the candidate set before the byte-cap.
func TestBloatGuardTopK(t *testing.T) {
	var cands []scored
	for i := 0; i < 60; i++ {
		cands = append(cands, scoredOf(Rule{ID: string(rune('A'+i%26)) + string(rune('0'+i/26)), Title: "T", Body: "small body", Confidence: 0.9, Weight: 0.9}, 0.9))
	}
	res, err := BloatGuard(cands, GuardConfig{TopK: 5, EagerBytes: 100000})
	if err != nil {
		t.Fatalf("BloatGuard: %v", err)
	}
	if len(res.Kept) > 5 {
		t.Fatalf("top-k=5 must keep ≤5, kept %d", len(res.Kept))
	}
}
