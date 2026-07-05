package rules

import (
	"math"
	"testing"
	"time"
)

// Test 4 (BL-2): criticality is a STRING; the >=8 immunity compare is
// Atoi-guarded. "8" ⇒ immune; "7" ⇒ not; "" ⇒ 5 ⇒ not; "high" ⇒ Atoi fails ⇒
// 5 ⇒ not (and NEVER panics).
func TestCriticalityStringGe8Immune(t *testing.T) {
	cases := []struct {
		crit string
		want bool
	}{
		{"8", true},
		{"9", true},
		{"10", true},
		{"7", false},
		{"", false},
		{"high", false}, // Atoi fails → 5 → not immune, no panic
		{" 8 ", true},   // trimmed
		{"not-a-number", false},
	}
	for _, c := range cases {
		r := Rule{Kind: "preference", Criticality: c.crit}
		if got := demotionImmune(r); got != c.want {
			t.Errorf("demotionImmune(criticality=%q) = %v, want %v", c.crit, got, c.want)
		}
	}
	// A guardrail and a timeless rule are immune regardless of criticality.
	if !demotionImmune(Rule{Kind: "guardrail"}) {
		t.Error("guardrail must be demotion-immune")
	}
	if !demotionImmune(Rule{Kind: "preference", Timeless: true}) {
		t.Error("timeless rule must be demotion-immune")
	}
	// BY DESIGN (Denis 2026-06-24, WONTFIX): identity (tier 1) is intentionally
	// NOT demotion-immune — it decays like any non-guardrail rule (M3-spec
	// BR-A1.5-6), diverging from the Hub guardrail/identity-immune convention.
	// See docs/decisions/2026-06-24-identity-not-demotion-immune.md. This test
	// LOCKS the decision: if a future change makes tier 1 immune, it trips here.
	if demotionImmune(Rule{Kind: "identity"}) {
		t.Error("identity (tier 1) must NOT be demotion-immune (by design, WONTFIX) — see ADR 2026-06-24")
	}
	// An identity rule still earns immunity via the explicit opt-in levers.
	if !demotionImmune(Rule{Kind: "identity", Timeless: true}) {
		t.Error("a timeless identity rule must be immune (opt-in lever)")
	}
	if !demotionImmune(Rule{Kind: "identity", Criticality: "8"}) {
		t.Error("a criticality>=8 identity rule must be immune (opt-in lever)")
	}
}

// Test 32 (F-5/F-7): decay derives from a TIMESTAMP (last_reinforced_at), not a
// frozen field. A rule reinforced 60 days ago with half_life 30 ⇒ effWeight
// reflects ~exp(-2) on the next sweep.
func TestDecayFromTimestampAdvances(t *testing.T) {
	r := Rule{Weight: 1.0, HalfLife: 30}
	now := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)
	last := now.Add(-60 * 24 * time.Hour) // 60 days ago
	got := DecayedEffWeight(r, last, now)
	want := 1.0 * math.Exp(-2.0) // days/half_life = 60/30 = 2
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("DecayedEffWeight = %v, want ~%v (exp(-2))", got, want)
	}
	// Timeless never decays.
	rt := Rule{Weight: 1.0, HalfLife: 30, Timeless: true}
	if got := DecayedEffWeight(rt, last, now); got != 1.0 {
		t.Fatalf("timeless rule must not decay: got %v want 1.0", got)
	}
	// No half-life never decays.
	rn := Rule{Weight: 1.0}
	if got := DecayedEffWeight(rn, last, now); got != 1.0 {
		t.Fatalf("no-half-life rule must not decay: got %v want 1.0", got)
	}
}

// Test 6 (BL-3): EmitEager synthesizes scored candidates and bypasses the floor
// — an eager-live set is returned ranked (NOT ErrNoMatch) though no query ran.
func TestEmitEagerSynthesizesScoredBypassesFloor(t *testing.T) {
	rs := []Rule{
		{ID: "a", Title: "Use Postgres", Body: "Always use PostgreSQL", Status: "live", Weight: 0.9},
		{ID: "b", Title: "Tabs", Body: "Prefer tabs over spaces", Status: "live", Weight: 0.8},
	}
	ix := BuildIndex(rs)
	res, err := ix.EmitEager(GuardConfig{})
	if err != nil {
		t.Fatalf("EmitEager errored on a live set: %v", err)
	}
	if len(res.Kept) != 2 {
		t.Fatalf("EmitEager kept %d rules, want 2 (both eager-live)", len(res.Kept))
	}
}

// Test 7 (BL-3): a non-empty eager-live set ⇒ a non-empty emitted block (guards
// the silent floor-to-empty failure — the core BL-3 regression).
func TestEmitEagerNonEmptyForLiveSet(t *testing.T) {
	rs := []Rule{{ID: "a", Title: "Use Postgres", Body: "Always use PostgreSQL", Status: "live", Weight: 0.9}}
	ix := BuildIndex(rs)
	res, err := ix.EmitEager(GuardConfig{})
	if err != nil {
		t.Fatalf("EmitEager: %v", err)
	}
	if got := RenderBody(res.Kept); got == "" {
		t.Fatal("EmitEager produced an EMPTY block for a non-empty live set (floor-to-empty regression)")
	}
}

// Test 8 (BL-3): only drafts present ⇒ ErrNoMatch (no panic) ⇒ the emitter
// writes an empty-but-well-fenced region.
func TestEmitEagerEmptyLiveSetIsErrNoMatch(t *testing.T) {
	rs := []Rule{
		{ID: "d", Title: "Draft", Body: "draft rule", Status: "draft", Weight: 0.9},
		{ID: "a", Title: "Active", Body: "active rule", Status: "active", Weight: 0.9}, // active, NOT live
	}
	ix := BuildIndex(rs)
	_, err := ix.EmitEager(GuardConfig{})
	if err != ErrNoMatch {
		t.Fatalf("EmitEager with no LIVE rule: got %v, want ErrNoMatch", err)
	}
}

// EmitEager excludes non-eager rules (load_mode on_demand).
func TestEmitEagerExcludesNonEager(t *testing.T) {
	rs := []Rule{
		{ID: "a", Title: "Eager", Body: "eager rule", Status: "live", Weight: 0.9, LoadMode: "eager"},
		{ID: "b", Title: "OnDemand", Body: "on demand rule", Status: "live", Weight: 0.9, LoadMode: "on_demand"},
		{ID: "c", Title: "Default", Body: "default mode rule", Status: "live", Weight: 0.9}, // "" → eager
	}
	ix := BuildIndex(rs)
	res, err := ix.EmitEager(GuardConfig{})
	if err != nil {
		t.Fatalf("EmitEager: %v", err)
	}
	if len(res.Kept) != 2 {
		t.Fatalf("EmitEager kept %d, want 2 (eager + default, NOT on_demand)", len(res.Kept))
	}
	for _, r := range res.Kept {
		if r.ID == "b" {
			t.Fatal("EmitEager must exclude an on_demand rule")
		}
	}
}

// InjectableRules walls out draft/superseded/rejected (M3-R1) but keeps
// active/live AND legacy unset-status rules (the M2 no-regression default).
func TestInjectableRulesFilters(t *testing.T) {
	rs := []Rule{
		{ID: "live", Status: "live"},
		{ID: "active", Status: "active"},
		{ID: "draft", Status: "draft"},
		{ID: "superseded", Status: "superseded"},
		{ID: "rejected", Status: "rejected"},
		{ID: "empty", Status: ""}, // legacy: stays injectable (M2 compat)
	}
	got := InjectableRules(rs)
	ids := map[string]bool{}
	for _, r := range got {
		ids[r.ID] = true
	}
	if len(got) != 3 || !ids["live"] || !ids["active"] || !ids["empty"] {
		t.Fatalf("InjectableRules must keep live+active+legacy-empty, dropping draft/superseded/rejected; got %v", ids)
	}
	if ids["draft"] || ids["superseded"] || ids["rejected"] {
		t.Fatalf("InjectableRules must wall out draft/superseded/rejected; got %v", ids)
	}
}
