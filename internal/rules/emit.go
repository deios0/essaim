package rules

// EmitEager builds the eager-live ranked block WITHOUT a per-request query
// (M3-R4, spec §5.1 — closes BL-3). It is the ONE ranked source the
// NativeFileEmitter consumes, producing the SAME GuardResult the proxy path
// produces so proxy- and file-injected tools agree.
//
// The construction problem it solves: `scored` is unexported with unexported
// fields, and its ONLY constructor is Match(query), which requires a query and
// runs BM25. There is no exported path to feed BloatGuard an "eager-live subset"
// without a query. EmitEager therefore lives in package `rules` (so it can name
// `scored`), SYNTHESIZES a scored candidate per rule passing isLive ∧ isEager
// with score:1.0, rel:1.0, relSet:true, and runs the bloat guard. With rel:1.0
// the similarity floor (guard.go:168-180) is a no-op (every eager-live rule is
// relevant by definition — there is no query), so the guard runs only its
// scope-partition, tier-sort, and byte-cap stages.
//
// rel:1.0 is MANDATORY: if it were left at rel:0/relSet:false the floor would
// fall back to `score`; and if score were 0 too, every rule would be floored →
// ErrNoMatch → the emitter would write an empty region forever, silently
// (B-1.1). Synthesizing score:1.0, rel:1.0, relSet:true guarantees the floor
// passes.
func (ix *Index) EmitEager(cfg GuardConfig) (GuardResult, error) {
	if ix == nil || ix.Len() == 0 {
		return GuardResult{}, ErrIndexEmpty
	}
	cands := make([]scored, 0, ix.Len())
	for _, r := range ix.rules {
		if !isLive(r) || !isEager(r) {
			continue
		}
		// Bypass the relevance floor: rel:1.0 ⇒ every eager-live rule clears it.
		cands = append(cands, scored{rule: r, score: 1.0, rel: 1.0, relSet: true})
	}
	if len(cands) == 0 {
		return GuardResult{}, ErrNoMatch
	}
	return BloatGuard(cands, cfg)
}
