package rules

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// Lifecycle status constants (spec §2.1). The injector/proxy whitelist is
// {active, live} (M3-R1); the NativeFileEmitter narrows to {live} (§5.4). A
// `draft` reaches NEITHER channel — it lives only in _inbox/ until promoted.
const (
	StatusDraft      = "draft"
	StatusActive     = "active"
	StatusLive       = "live"
	StatusSuperseded = "superseded"
	StatusRejected   = "rejected"
)

// nonInjectableStatuses are the statuses that WALL a rule out of the matchable
// index and every tool's context (M3-R1). A draft (auto-extracted, unvetted), a
// superseded rule, and a rejected rule must NEVER inject.
var nonInjectableStatuses = map[string]bool{
	StatusDraft:      true,
	StatusSuperseded: true,
	StatusRejected:   true,
}

// isInjectable reports whether a rule may enter the matchable index AND be
// injected via the proxy path. This is the NEW M3 wall: M2 indexed EVERY loaded
// rule regardless of status, so a `draft` written to _inbox/ was injected into
// every matching request — the exact opposite of the safety model.
//
// Binding reading (M3-R1 + the M2 no-regression constraint): the whitelist the
// spec names is {active, live}; an UNSET/empty status is the legacy default and
// stays injectable (a status-less hand-authored rule keeps working, exactly as
// M2 — the reference engine's "NULL=legacy" philosophy). Only the explicitly non-injectable
// statuses (draft/superseded/rejected) are walled out. So a `draft` in _inbox/
// can never enter the index, while every active/live/legacy rule still does.
func isInjectable(r Rule) bool {
	s := strings.ToLower(strings.TrimSpace(r.Status))
	return !nonInjectableStatuses[s]
}

// isLive reports whether a rule is `status:live` — the STRICTER gate the
// NativeFileEmitter uses (§5.4). The always-on native instruction file is the
// higher-trust channel: only `live` rules are emitted, never `active`, never a
// draft.
func isLive(r Rule) bool {
	return strings.ToLower(strings.TrimSpace(r.Status)) == StatusLive
}

// isEager reports whether a rule is in the eager subset (mirrors the
// rules-inject.sh eager subset). An empty load_mode defaults to eager so a
// plain rule with no load_mode is emitted; only an explicit non-eager mode
// (e.g. "on_demand") is excluded from the always-on native block.
func isEager(r Rule) bool {
	m := strings.ToLower(strings.TrimSpace(r.LoadMode))
	return m == "" || m == "eager"
}

// InjectableRules returns the subset of rs that may be indexed/injected
// (status ∈ {active, live}). BuildIndex is fed this; the Store keeps the FULL
// slice for the lifecycle sweep (which must see drafts to promote them).
func InjectableRules(rs []Rule) []Rule {
	out := make([]Rule, 0, len(rs))
	for _, r := range rs {
		if isInjectable(r) {
			out = append(out, r)
		}
	}
	return out
}

// demotionImmune reports whether a rule is immune to demote/supersede-by-decay
// (M3-R7, §3.5). A rule is immune iff it is timeless, a guardrail (tier 2), OR
// its criticality is >= 8. Criticality is parsed from a STRING (Rule.Criticality
// is `string`, matching the YAML frontmatter); reference engine stores it as Optional[int]
// (">=8 demotion-immune; NULL/legacy treated as 5"). The compare is Atoi-guarded
// so it NEVER panics: empty/legacy/non-numeric ("high") → treated as 5 → NOT
// immune.
//
// BY DESIGN (Denis 2026-06-24): identity (tier 1) is intentionally NOT immune —
// it decays per M3-spec BR-A1.5-6, like any other non-guardrail rule. This
// DIVERGES from the Hub guardrail/identity-immune convention (where identity is
// treated as untouchable) BY DESIGN: oikos identity rules are learned/asserted
// and must be allowed to age out if never reinforced; only tier-2 guardrails get
// the hard immunity. See docs/decisions/2026-06-24-identity-not-demotion-immune.md.
// This is WONTFIX, not an oversight — do not "fix" tier 1 to be immune.
func demotionImmune(r Rule) bool {
	if r.Timeless || tier(r.Kind) == 2 {
		return true
	}
	n, err := strconv.Atoi(strings.TrimSpace(r.Criticality))
	return err == nil && n >= 8
}

// DemotionImmune is the exported view of demotionImmune for the lifecycle
// sweep (which lives in another package).
func DemotionImmune(r Rule) bool { return demotionImmune(r) }

// ConfBucket exposes the coarse confidence bucket for the lifecycle sweep's
// class-cross write oracle (a bucket flip is one of the three durable-write
// triggers, §3.4).
func ConfBucket(r Rule) string { return confBucket(r) }

// DecayedEffWeight computes a rule's effective weight with time decay derived
// from a TIMESTAMP (M3-R8 / design-closure §5 "decay from timestamps, not
// ticks — laptops sleep"), not from the static DaysSinceReinforced counter that
// nothing advances. days = max(0, now - lastReinforced)/24h. A timeless rule or
// a rule with no half-life never decays. Pure; the caller supplies `now`.
func DecayedEffWeight(r Rule, lastReinforced, now time.Time) float64 {
	w := r.Weight
	if w == 0 {
		w = r.Confidence
	}
	if r.Timeless || r.HalfLife <= 0 || lastReinforced.IsZero() {
		return w
	}
	days := now.Sub(lastReinforced).Hours() / 24.0
	if days <= 0 {
		return w
	}
	return w * math.Exp(-days/r.HalfLife)
}

// EffWeight exposes the static (frontmatter-cached) effWeight for callers
// outside the package (the lifecycle sweep reads it to decide a demote-floor
// cross). It reads the cached DaysSinceReinforced — the sweep refreshes that
// field from last_reinforced_at coarsely (≤1/day/rule), so the hot path never
// does timestamp math.
func (r Rule) EffWeight() float64 { return r.effWeight() }
