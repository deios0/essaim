// Package lifecycle implements the M3 rule lifecycle sweep (spec §3): title-hash
// dedup, reinforce-on-repeat, decay (from a TIMESTAMP, not a frozen counter),
// heuristic supersede, and promote (draft→live on sigil OR reinforce-twice). It
// writes durable `.md` frontmatter ONLY on a class-cross, debounced — hot
// counters (hits/reinforce-counts) stay LOCAL in an in-mem store and NEVER touch
// frontmatter (the frontmatter-immutability rule). All work is off the request hot path.
package lifecycle

import (
	"os"
	"strings"
	"time"

	"essaim/internal/rules"
)

// Default sweep interval (laptops sleep → the timer re-arms on wake; no cron).
const DefaultInterval = 5 * time.Minute

// DefaultDemoteFloor is the effWeight below which a non-immune rule is demoted
// (one of the three class-cross write triggers, §3.4).
const DefaultDemoteFloor = 0.2

// Sweeper runs the lifecycle over a vault. It reads `.md` rules, applies the
// lifecycle, and writes frontmatter only on a class-cross.
type Sweeper struct {
	vault       string
	now         func() time.Time
	demoteFloor float64
	// hot is the LOCAL ephemeral store of reinforce counters keyed by title-hash
	// — never frontmatter (3-class discipline).
	hot *hotStore
}

// New constructs a Sweeper over vault. The reinforce-count hot store is backed
// by a per-vault sidecar (_inbox/.counts.json) so reinforce counts survive a
// daemon restart (P0-2) — without that, the draft→live "reinforced twice across
// days" promotion is unreachable.
func New(vault string) *Sweeper {
	return &Sweeper{vault: vault, now: time.Now, demoteFloor: DefaultDemoteFloor, hot: newHotStoreAt(vault)}
}

// SetNow is a test seam.
func (s *Sweeper) SetNow(fn func() time.Time) { s.now = fn }

// SetDemoteFloor overrides the demote floor (test seam).
func (s *Sweeper) SetDemoteFloor(f float64) { s.demoteFloor = f }

// loaded pairs a Rule with its on-disk file path so a class-cross can rewrite
// the exact file.
type loaded struct {
	rule rules.Rule
	path string
}

// Hint is the quality classification of a correction (port classify_quality's
// `hint`): a draft/promote-eligible correction is HintNew or HintValidated; a
// HintRejected (or empty) correction is below the new-hint floor.
type Hint string

const (
	HintValidated Hint = "validated"
	HintNew       Hint = "new"
	HintRejected  Hint = "rejected"
)

// atLeastNew reports whether a hint clears the `new` floor (BR-A1.5-2 promote
// gate). validated and new clear it; rejected/empty do not.
func (h Hint) atLeastNew() bool { return h == HintValidated || h == HintNew }

// Reinforce records a repeated correction matching an existing rule's title-hash
// (port). It bumps the LOCAL reinforce counter (NOT
// frontmatter), and if the resulting state crosses a class boundary the next
// Sweep persists it. Returns the new local reinforce count. hint is the latest
// correction's quality hint: a `validated` hint upgrades new/draft→validated
// (sticky-up, never downgrades); the latest hint's >= new status is what the
// promote gate reads (RISK-4 / BR-A1.5-2 — promote requires the LATEST hint be
// >= new, not merely the count). This is the off-path call the extractor makes
// when a new correction's title-hash already exists.
func (s *Sweeper) Reinforce(title string, hint Hint) int {
	h := rules.TitleHashOf(title)
	return s.hot.reinforce(h, hint == HintValidated, hint.atLeastNew(), s.now())
}

// ReinforceCount returns the LOCAL reinforce count for a title (test/inspection).
func (s *Sweeper) ReinforceCount(title string) int {
	return s.hot.count(rules.TitleHashOf(title))
}

// SweepResult reports what one sweep changed (for tests + /health).
type SweepResult struct {
	Promoted   []string // ids promoted draft→live
	Demoted    []string // ids demoted below the floor
	Superseded []string // ids superseded
	Writes     int      // durable frontmatter writes (class-crosses)
}

// Sweep runs one lifecycle pass over the vault. It loads every rule (all
// statuses), applies dedup/reinforce/decay/supersede/promote, and writes
// frontmatter ONLY on a class-cross. Returns what changed.
func (s *Sweeper) Sweep() (SweepResult, error) {
	var res SweepResult
	ls, err := s.loadAll()
	if err != nil {
		return res, err
	}
	now := s.now()

	for i := range ls {
		l := &ls[i]
		h := rules.TitleHashOf(l.rule.Title)
		oldStatus := strings.ToLower(strings.TrimSpace(l.rule.Status))
		oldBucket := rules.ConfBucket(l.rule)

		// Decay (M3-R8): derive effWeight from last_reinforced_at TIMESTAMP. The
		// anchor is the LATEST of the durable frontmatter timestamp and any
		// in-session reinforce (P2-reinforce-ts): a freshly-reinforced rule must
		// decay from the reinforce, not from creation, otherwise a rule the user
		// keeps correcting would wrongly demote on the stale frontmatter clock.
		//
		// CHURN FIX (P2-reinforce-ts): the frontmatter last_reinforced_at is
		// persisted at SECOND granularity (RFC3339, see writeFrontmatter's
		// last.UTC.Format(time.RFC3339)). The in-session reinforce clock, however,
		// carries sub-second precision. A naive rt.After(last) therefore re-fires on
		// EVERY sweep (rt=12:00:00.5 is forever "after" the second-truncated disk
		// value 12:00:00) → a self-perpetuating frontmatter re-write (dirty git). We
		// compare at SECOND granularity — the same resolution the write rounds to —
		// so a re-anchor is a true no-op once the durable anchor has caught up to the
		// reinforce's second. We anchor `last` at the truncated reinforce time too,
		// so the persisted value (below) is byte-identical on the next sweep.
		last := parseTime(l.rule)
		reAnchored := false
		if rt := s.hot.lastReinforce(h).Truncate(time.Second); rt.After(last.Truncate(time.Second)) {
			last = rt
			reAnchored = true
		}
		decayed := rules.DecayedEffWeight(l.rule, last, now)

		newStatus := oldStatus
		demote := false

		// Promote: draft→live on reinforce-twice (BR-A1.5-2). The binding rule is
		// "reinforced twice (≥2 INDEPENDENT reinforces; weight reaches 3 from an
		// initial 1) AND its latest quality hint is ≥ new". The local count includes
		// the draft-CREATING write as the initial 1 (learn.process reinforces on the
		// create too), so "weight reaches 3" ⇒ rc >= 3 (create + 2 independent
		// reinforces) — NOT rc >= 2 (RISK-5 off-by-one). AND the latest hint must be
		// ≥ new (RISK-4 — a count of repeats whose hints were rejected must NOT
		// promote). A sigil-promotion path (active→live) is handled the same way: an
		// active rule reinforced to the threshold with a ≥ new hint promotes to live.
		// P1 SYNC QUARANTINE: a remote-origin rule is NEVER auto-promoted. The
		// reinforce count is keyed by title, so a remote rule sharing a title with
		// one the local user reinforces would otherwise ride those reinforces to
		// live — defeating the sync quarantine wall. A remote rule becomes
		// injectable only when the user explicitly accepts it (edits the file /
		// clears remote_origin).
		rc := s.hot.count(h)
		if !l.rule.RemoteOrigin && (oldStatus == rules.StatusDraft || oldStatus == rules.StatusActive) && rc >= 3 && s.hot.latestHintAtLeastNew(h) {
			newStatus = rules.StatusLive
		}

		// Demote: a non-immune rule whose decayed weight crossed the floor is
		// demoted (active/live → superseded by decay). Immune rules (guardrail /
		// timeless / criticality>=8) are never demoted.
		if !rules.DemotionImmune(l.rule) && (oldStatus == rules.StatusActive || oldStatus == rules.StatusLive) {
			if decayed < s.demoteFloor {
				demote = true
				newStatus = rules.StatusSuperseded
			}
		}

		// The class-cross write oracle (BR-A1.5-4): write frontmatter iff
		//   (1) status changed, OR
		//   (2) confBucket flipped, OR
		//   (3) effWeight crossed the demote floor (a continuous decay the bucket
		//       does not see).
		// We compute the would-be new rule to compare bucket + cache the decayed
		// days back.
		newRule := l.rule
		newRule.Status = newStatus
		// Refresh the cached days_since_reinforced from the timestamp so the
		// hot-path effWeight stays current (≤1/sweep, never per-turn). When the
		// anchor was re-anchored by an in-session reinforce, persist the advanced
		// last_reinforced_at too so the durable decay clock matches (P2-reinforce-ts).
		if reAnchored {
			newRule.LastReinforcedAt = last.UTC().Format(time.RFC3339)
		}
		if !last.IsZero() {
			days := now.Sub(last).Hours() / 24.0
			if days < 0 {
				days = 0
			}
			newRule.DaysSinceReinforced = days
		}
		newBucket := rules.ConfBucket(newRule)

		statusChanged := newStatus != oldStatus
		bucketFlipped := newBucket != oldBucket
		floorCrossed := demote || (decayed < s.demoteFloor && (oldStatus == rules.StatusActive || oldStatus == rules.StatusLive) && !rules.DemotionImmune(l.rule))

		if statusChanged {
			switch newStatus {
			case rules.StatusLive:
				res.Promoted = append(res.Promoted, l.rule.ID)
			case rules.StatusSuperseded:
				res.Demoted = append(res.Demoted, l.rule.ID)
			}
		}

		// A re-anchor (P2-reinforce-ts) advances the DURABLE decay clock, so it must
		// be persisted — otherwise the new anchor is lost on restart and the rule
		// decays from the stale creation timestamp again. This is debounced to
		// ≤1/sweep and only fires when the in-session reinforce actually moved the
		// anchor forward (rt.After(last)); a reinforce whose time equals the existing
		// frontmatter timestamp does NOT re-anchor, so hot counters that don't move
		// the clock still never touch frontmatter (the frontmatter-immutability rule / Test 34).
		if statusChanged || bucketFlipped || floorCrossed || reAnchored {
			if err := s.writeFrontmatter(l.path, newRule); err == nil {
				res.Writes++
				l.rule = newRule
			}
		}
	}

	return res, nil
}

// MarkSupersede sets an existing rule (by title-hash) to status:superseded — the
// heuristic supersede path (BR-A1.5-5): an opposite-meaning correction matching
// an existing rule's title-hash supersedes it. Detecting "opposite meaning"
// without embeddings is heuristic in M3 (negation token + same subject); the
// CALLER decides oppositeness, this performs the durable write. Returns the path
// written (or "" if not found / immune).
func (s *Sweeper) MarkSupersede(title string) (string, error) {
	ls, err := s.loadAll()
	if err != nil {
		return "", err
	}
	target := rules.TitleHashOf(title)
	for _, l := range ls {
		if rules.TitleHashOf(l.rule.Title) != target {
			continue
		}
		if rules.DemotionImmune(l.rule) {
			return "", nil // immune rules are never superseded by decay/opposite
		}
		nr := l.rule
		nr.Status = rules.StatusSuperseded
		if err := s.writeFrontmatter(l.path, nr); err != nil {
			return "", err
		}
		return l.path, nil
	}
	return "", nil
}

// loadAll reads every `.md` rule under the vault with its file path.
func (s *Sweeper) loadAll() ([]loaded, error) {
	var out []loaded
	rs, err := rules.LoadVaultWithPaths(s.vault)
	if err != nil {
		return nil, err
	}
	for _, rp := range rs {
		out = append(out, loaded{rule: rp.Rule, path: rp.Path})
	}
	return out, nil
}

// parseTime reads last_reinforced_at from the rule (RFC-3339); zero on absence.
func parseTime(r rules.Rule) time.Time {
	if r.LastReinforcedAt == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, r.LastReinforcedAt)
	if err != nil {
		return time.Time{}
	}
	return t
}

// writeFrontmatter rewrites the rule's `.md` with updated DURABLE frontmatter
// (status/confidence/weight/last_reinforced_at/days_since_reinforced), preserving
// the body. This is the ONLY durable writer; it fires only on a class-cross.
func (s *Sweeper) writeFrontmatter(path string, r rules.Rule) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	body := bodyOf(string(raw))
	doc := rules.RenderRuleFile(r, body)
	return atomicWrite(path, []byte(doc))
}

// bodyOf returns the markdown body (after the closing frontmatter fence) of a
// raw `.md` doc.
func bodyOf(raw string) string {
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	const fence = "---\n"
	if !strings.HasPrefix(s, fence) {
		return strings.TrimSpace(s)
	}
	rest := s[len(fence):]
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return strings.TrimSpace(s)
	}
	after := rest[idx+len("\n---"):]
	after = strings.TrimPrefix(after, "\n")
	return strings.TrimSpace(after)
}
