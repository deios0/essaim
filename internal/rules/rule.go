// Package rules loads Obsidian-style Markdown rules (YAML frontmatter + body),
// builds an in-memory lexical index (T0 BM25/trigram), ranks a matched set
// through the bloat guard, and renders a deterministic, cache-stable injection
// body. It is the rule-store facet (C2) feeding the B1 injection mechanic.
//
// Everything here is pure-Go and CGO-free. M5 SEMANTIC relevance is a curated
// concept-expansion table (concepts.go), applied at index-build time — NOT an
// ONNX/CGO embedder (rejected to preserve the single-static-binary + zero-
// phone-home invariants; see ADR 2026-06-27-m5-semantic-concept-expansion). The
// request path reads an immutable *Index via an atomic pointer load; rebuilds
// happen off the hot path and swap the pointer (build-then-swap).
package rules

import (
	"math"
	"strings"
)

// Rule is one loaded `.md` rule: YAML frontmatter fields plus the Markdown body.
// Only DURABLE fields participate in render (cache-stability, spec §2.4); no
// per-turn state (hits/last_used/timestamps) ever appears in a Rule.
type Rule struct {
	ID          string   `yaml:"id"`
	Title       string   `yaml:"title"`
	Body        string   `yaml:"-"` // the Markdown body (after the frontmatter)
	Kind        string   `yaml:"kind"`
	Scope       string   `yaml:"scope"`
	ProjectTag  string   `yaml:"project_tag"`
	LoadMode    string   `yaml:"load_mode"`
	GlobPaths   []string `yaml:"glob_paths"`
	Weight      float64  `yaml:"weight"`
	Status      string   `yaml:"status"`
	Confidence  float64  `yaml:"confidence"`
	Timeless    bool     `yaml:"timeless"`
	Criticality string   `yaml:"criticality"`
	HalfLife    float64  `yaml:"half_life_days"`

	// DaysSinceReinforced is read from frontmatter (key: days_since_reinforced)
	// and drives effWeight decay. Never recomputed/written by the request path
	// (spec §7 durable-coarse). 0 ⇒ no decay applied unless HalfLife is set. The
	// lifecycle sweep refreshes it (≤1/day/rule) from LastReinforcedAt so the hot
	// path never does timestamp math (M3-R8).
	DaysSinceReinforced float64 `yaml:"days_since_reinforced"`

	// LastReinforcedAt is the RFC-3339 timestamp of the last reinforce (M3-R8 /
	// design-closure §5 "decay from timestamps, not ticks"). The lifecycle sweep
	// derives `days = now - last_reinforced_at` at evaluation time; the hot path
	// reads only the cached DaysSinceReinforced. Empty ⇒ no timestamp decay.
	LastReinforcedAt string `yaml:"last_reinforced_at"`

	// RemoteOrigin marks a rule that arrived from a team SYNC pull. The sync layer
	// quarantines every incoming remote rule as a status:draft in _inbox/ and
	// stamps this flag; the lifecycle sweep then refuses to AUTO-promote it (the
	// reinforce count is keyed by title, so without this a remote rule sharing a
	// title with one the local user reinforces would ride those reinforces to
	// live). A remote rule becomes injectable only when the user explicitly
	// accepts it (edits the file / clears this marker). Preserved across sweep
	// rewrites by RenderRuleFile.
	RemoteOrigin bool `yaml:"remote_origin"`
}

// tier maps a rule kind to its criticality tier for the demote-then-promote
// sort (spec §5.3.d). A guardrail is NEVER the rule dropped to fit the cap.
func tier(kind string) int {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "guardrail":
		return 2
	case "identity":
		return 1
	default:
		return 0
	}
}

// Tier is the exported view of tier for callers outside the package (the sync
// layer's privileged-tier clamp: a remote rule may never acquire a tier>0 kind
// on import). "Privileged" means exactly what the injector means by it — guardrail
// (2) and identity (1) — so the wall can never drift from the hot path.
func Tier(kind string) int { return tier(kind) }

// effWeight computes the effective weight with exponential time decay, READ from
// durable frontmatter fields only (spec §5.3.d, §7). A timeless rule never
// decays. A rule with no half-life never decays. The decay is read, never
// written: this function is pure.
func (r Rule) effWeight() float64 {
	w := r.Weight
	if w == 0 {
		// Fall back to confidence as the durable weight when weight is unset, so
		// a rule with only a confidence still ranks meaningfully.
		w = r.Confidence
	}
	if r.Timeless || r.HalfLife <= 0 || r.DaysSinceReinforced <= 0 {
		return w
	}
	return w * math.Exp(-r.DaysSinceReinforced/r.HalfLife)
}

// confBucket derives a coarse confidence bucket ("H"|"M"|"L") from the durable
// confidence/weight WITH HYSTERESIS so a tiny drift cannot flip a bucket every
// turn (spec §2.4): that would change the rendered block bytes and bust the
// upstream prompt cache (Tests 4, 7). The function is pure, deterministic,
// locale-independent, and map-iteration-independent.
//
// Hysteresis is implemented by quantizing to a coarse step (0.1) BEFORE
// thresholding: an epsilon drift that stays within the same 0.1 band yields the
// identical bucket. Thresholds: H ≥ 0.8, M ≥ 0.5, else L.
func confBucket(r Rule) string {
	c := r.Confidence
	if c == 0 {
		c = r.Weight
	}
	// Quantize to a 0.1 grid (round to nearest tenth) — the hysteresis band.
	q := math.Round(c*10) / 10
	switch {
	case q >= 0.8:
		return "H"
	case q >= 0.5:
		return "M"
	default:
		return "L"
	}
}

// oneline collapses a rule body to a single deterministic line: runs of
// whitespace (including newlines) become a single space, leading/trailing
// whitespace is trimmed. Idempotent; locale- and map-iteration-independent.
func oneline(body string) string {
	var b strings.Builder
	b.Grow(len(body))
	prevSpace := false
	for _, ch := range body {
		switch ch {
		case ' ', '\t', '\n', '\r', '\f', '\v':
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		default:
			b.WriteRune(ch)
			prevSpace = false
		}
	}
	return strings.TrimSpace(b.String())
}
