package rules

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Bloat-guard errors. ErrNoMatch (floor emptied the set) and ErrIndexEmpty (no
// rules at all) are HONEST misses — degraded=false (spec §5.5). They are distinct
// so /health can report rules_indexed:0 vs an honest no-match.
var (
	ErrNoMatch    = errors.New("oikos: no rule cleared the similarity floor")
	ErrIndexEmpty = errors.New("oikos: rule index is empty")
)

// GuardConfig holds the tunable bloat-guard knobs (spec §5.3). Zero values fall
// back to the spec defaults via withDefaults.
type GuardConfig struct {
	TopK       int     // OIKOS_TOP_K (default 10, cap 50)
	MatchFloor float64 // OIKOS_MATCH_FLOOR (default 0.60)
	EagerBytes int     // OIKOS_EAGER_BYTES (default 4096)
	CWD        string  // current project dir, for scope partition
}

const (
	defaultTopK       = 10
	maxTopK           = 50
	defaultMatchFloor = 0.60
	defaultEagerBytes = 4096
)

func (c GuardConfig) withDefaults() GuardConfig {
	if c.TopK <= 0 {
		c.TopK = defaultTopK
	}
	if c.TopK > maxTopK {
		c.TopK = maxTopK
	}
	if c.MatchFloor <= 0 {
		c.MatchFloor = defaultMatchFloor
	}
	if c.EagerBytes <= 0 {
		c.EagerBytes = defaultEagerBytes
	}
	return c
}

// GuardConfigFromEnv reads the bloat-guard knobs from the environment
// (OIKOS_TOP_K, OIKOS_MATCH_FLOOR, OIKOS_EAGER_BYTES, OIKOS_CWD). Unset/invalid
// values fall back to the spec defaults. CWD defaults to the process working
// directory when OIKOS_CWD is unset.
func GuardConfigFromEnv() GuardConfig {
	cfg := GuardConfig{}
	if v := os.Getenv("OIKOS_TOP_K"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.TopK = n
		}
	}
	if v := os.Getenv("OIKOS_MATCH_FLOOR"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.MatchFloor = f
		}
	}
	if v := os.Getenv("OIKOS_EAGER_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.EagerBytes = n
		}
	}
	if v := os.Getenv("OIKOS_CWD"); v != "" {
		cfg.CWD = v
	} else if wd, err := os.Getwd(); err == nil {
		cfg.CWD = wd
	}
	return cfg
}

// GuardResult is the bloat guard's output: the rules to inject (already ordered
// for render) plus accounting for /health and capture.
type GuardResult struct {
	Kept       []Rule
	OmittedIDs []string
	Omitted    int
}

// MatchAndGuard runs the T0 lexical match over query then the full bloat-guard
// pipeline, returning the rules to inject (render order) and accounting. It is
// the single exported entry point the server uses, keeping the scored type
// internal. ErrNoMatch (floor emptied) and ErrIndexEmpty (no rules) are honest
// misses (spec §5.5).
func (ix *Index) MatchAndGuard(query string, cfg GuardConfig) (GuardResult, error) {
	if ix == nil || ix.Len() == 0 {
		return GuardResult{}, ErrIndexEmpty
	}
	cands := ix.Match(query)
	return BloatGuard(cands, cfg)
}

// wrapperReserve is the byte overhead the wrapper sentinels add (OIKOS_BEGIN +
// "\n" + ... + "\n" + OIKOS_END). It is reserved against EagerBytes so the final
// wrapped block fits the cap (spec §2.1, §5.3.e). Defined in render.go alongside
// the sentinels; declared here as the value used by the byte-cap.
//
// We compute it from the sentinel constants to keep them in one place.
var wrapperReserve = len(OIKOS_BEGIN) + len("\n") + len("\n") + len(OIKOS_END)

// perRuleTagOverhead approximates the non-body bytes a rendered rule line costs
// (the "- [H] Title: " prefix + newline). Spec §5.3.e uses ~64B/rule.
const perRuleTagOverhead = 64

// truncationMarker is appended to an oversized first rule's body when it is
// truncated to fit the eager byte-cap (P2-4). It is a VISIBLE flag (never a
// silent cut) so a reader — human or model — sees the rule body was clipped, and
// so the injected block stays honest about what was omitted.
const truncationMarker = " …[oikos: truncated]"

// BloatGuard applies the ordered bloat-guard pipeline (spec §5.3) to a scored
// candidate set:
//
//	(a) project-scope partition: local (scope/glob/project_tag matches cwd) first,
//	    then zone-wide.
//	(b) top-k: keep at most TopK by score within the partition order.
//	(c) similarity floor: drop score < MatchFloor; if empty ⇒ ErrNoMatch.
//	(d) criticality-first tier sort: stable by (tier DESC, effWeight DESC, score DESC);
//	    a guardrail is never the dropped one.
//	(e) hard byte-cap: accumulate until the next line would exceed EagerBytes minus
//	    the wrapper reserve; always keep ≥1.
//
// The returned Kept slice is in render order (already tier-sorted). renderBody
// re-sorts defensively but the order is identical.
func BloatGuard(cands []scored, cfg GuardConfig) (GuardResult, error) {
	cfg = cfg.withDefaults()
	if len(cands) == 0 {
		return GuardResult{}, ErrNoMatch
	}

	// (a) project-scope partition: local ahead of zone-wide, each keeping the
	// incoming score order (which is score DESC, id ASC). We tag locality so it
	// survives the tier sort in (d) as the PRIMARY axis — local-first is a
	// partition invariant, not just an initial order (spec §5.3.a; Test 37).
	type cand struct {
		scored
		local bool
	}
	var local, zone []cand
	for _, c := range cands {
		if scopeMatchesCWD(c.rule, cfg.CWD) {
			local = append(local, cand{c, true})
		} else {
			zone = append(zone, cand{c, false})
		}
	}
	ordered := append(append(make([]cand, 0, len(cands)), local...), zone...)

	// (b) top-k.
	if len(ordered) > cfg.TopK {
		ordered = ordered[:cfg.TopK]
	}

	// (c) similarity floor (F-A): cut on the ABSOLUTE relevance score (rel), NOT
	// the normalized BM25 score. The normalized score pins the top candidate to
	// 1.0 regardless of how irrelevant it is, so a floor on `score` never fires —
	// any rule sharing one trigram with the query would be injected (the product
	// kill-risk). `rel` is query-WORD coverage in [0,1], independent of the other
	// candidates, so a "Postgres" rule scores rel≈0 for "what's the weather" and
	// is dropped, while it clears the floor for "which database should I use?".
	//
	// A scored candidate built by hand (tests / future callers) with rel==0 but a
	// positive score is treated as "relevance unknown" and falls back to the score
	// gate, so the existing BloatGuard unit tests that set only `score` still work.
	floored := ordered[:0:0]
	for _, c := range ordered {
		relevance := c.rel
		if !c.relSet {
			relevance = c.score // back-compat: hand-built candidate, no rel computed
		}
		if relevance >= cfg.MatchFloor {
			floored = append(floored, c)
		}
	}
	if len(floored) == 0 {
		return GuardResult{}, ErrNoMatch
	}

	// (d) demote-then-promote tier sort (criticality-first). Stable so equal
	// keys keep the partition/score order. Sort key (highest first):
	// local-partition → tier(kind) → effWeight → score. Locality is primary so a
	// project-local rule outranks a higher-weight zone-wide one (Test 37); within
	// a partition a guardrail is never the dropped one (Test 35).
	sort.SliceStable(floored, func(a, b int) bool {
		if floored[a].local != floored[b].local {
			return floored[a].local // true (local) sorts first
		}
		ta, tb := tier(floored[a].rule.Kind), tier(floored[b].rule.Kind)
		if ta != tb {
			return ta > tb
		}
		wa, wb := floored[a].rule.effWeight(), floored[b].rule.effWeight()
		if wa != wb {
			return wa > wb
		}
		return floored[a].score > floored[b].score
	})

	// (e) hard byte-cap, reserving the wrapper bytes; always keep ≥1.
	budget := cfg.EagerBytes - wrapperReserve
	if budget < 0 {
		budget = 0
	}
	res := GuardResult{}
	var used int
	for i, fc := range floored {
		c := fc.scored
		lineCost := renderLineLen(c.rule)
		if i == 0 {
			// Always keep the highest-tier rule — a single guardrail is NEVER
			// starved (spec §5.3.e). But an UNBOUNDED oversized rule (a single
			// rendered line many KB long) would blow the eager budget, defeating the
			// cap. P2-4: keep the rule (same ID/Title/Kind — guardrail identity
			// intact) but TRUNCATE its body so its rendered line fits the budget. The
			// invariant holds (≥1 kept, the guardrail is present) AND the block stays
			// bounded. renderLine(bounded) ≤ budget ⇒ WrapBlock(RenderBody) ≤ EagerBytes.
			kept := capRuleBody(c.rule, budget)
			res.Kept = append(res.Kept, kept)
			used += renderLineLen(kept)
			continue
		}
		if used+1+lineCost > budget { // +1 for the joining newline
			res.Omitted++
			res.OmittedIDs = append(res.OmittedIDs, c.rule.ID)
			continue
		}
		res.Kept = append(res.Kept, c.rule)
		used += 1 + lineCost
	}
	return res, nil
}

// capRuleBody returns r unchanged when its rendered single line already fits the
// eager budget; otherwise it returns a COPY of r whose Body is truncated (on a
// rune boundary) and flagged with truncationMarker so the rendered line's length
// is ≤ budget (P2-4). Only Body is changed — ID/Title/Kind/weights are preserved,
// so a truncated guardrail keeps its identity, tier, and durable ranking fields.
// The wrapped block for a single kept rule is wrapperReserve + len(renderLine(r)),
// so bounding renderLine to `budget` (= EagerBytes − wrapperReserve) bounds the
// wrapped block to EagerBytes.
func capRuleBody(r Rule, budget int) Rule {
	if renderLineLen(r) <= budget {
		return r
	}
	// The non-body overhead of the rendered line (prefix + tag overhead) that we
	// cannot shrink: render with an empty body, then keep as much of oneline(Body)
	// as leaves room for the marker under budget.
	base := r
	base.Body = ""
	fixed := renderLineLen(base) // "- [H] Title: " + tag overhead, body empty
	room := budget - fixed - len(truncationMarker)
	if room < 0 {
		room = 0
	}
	oneLineBody := oneline(r.Body)
	if room > len(oneLineBody) {
		room = len(oneLineBody)
	}
	// Back up to a UTF-8 rune boundary so truncation never splits a multi-byte
	// rune. Guard room < len: when room == len(oneLineBody) the whole body fits and
	// indexing oneLineBody[room] would panic (out of range) — gemini review.
	for room > 0 && room < len(oneLineBody) && !utf8RuneStart(oneLineBody[room]) {
		room--
	}
	out := r
	out.Body = oneLineBody[:room] + truncationMarker
	return out
}

// utf8RuneStart reports whether b could be the first byte of a UTF-8 rune (i.e.
// it is NOT a continuation byte 0b10xxxxxx). Mirrors utf8.RuneStart without the
// import churn; used to keep body truncation on a rune boundary.
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// renderLineLen estimates the byte length one rendered rule line will consume,
// including the per-rule tag overhead (spec §5.3.e ~64B/rule).
func renderLineLen(r Rule) int {
	return len(renderLine(r)) + perRuleTagOverhead
}

// scopeMatchesCWD reports whether a rule is project-local for the given cwd
// (spec §5.3.a). A rule is local iff its project_tag equals the cwd's base name,
// OR any of its glob_paths matches the cwd path. With no cwd, nothing is local
// (everything is treated as zone-wide; v1 scope is semantic-only per the design
// closure, but the partition seam is honored).
func scopeMatchesCWD(r Rule, cwd string) bool {
	if cwd == "" {
		return false
	}
	base := filepath.Base(cwd)
	if r.ProjectTag != "" && r.ProjectTag == base {
		return true
	}
	for _, g := range r.GlobPaths {
		if g == "" {
			continue
		}
		if ok, _ := filepath.Match(g, cwd); ok {
			return true
		}
		if ok, _ := filepath.Match(g, base); ok {
			return true
		}
		if strings.Contains(cwd, strings.TrimSuffix(g, "/**")) && strings.HasSuffix(g, "/**") {
			return true
		}
	}
	return false
}
