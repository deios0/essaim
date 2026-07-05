package lifecycle

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"oikos/internal/rules"
)

func at(d int) time.Time { return time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC).AddDate(0, 0, d) }

func writeRule(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func loadRule(t *testing.T, path string) rules.Rule {
	t.Helper()
	rs, err := rules.LoadVaultWithPaths(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, rp := range rs {
		if rp.Path == path {
			return rp.Rule
		}
	}
	t.Fatalf("rule not found at %s", path)
	return rules.Rule{}
}

// Test 28: a repeated correction (same normalized title) reinforces — weight
// counter increments — ONE rule, not a duplicate.
func TestDedupTitleHashReinforcesNotDuplicates(t *testing.T) {
	s := New(t.TempDir())
	s.SetNow(func() time.Time { return at(0) })
	if got := s.Reinforce("Prefer Tabs", HintNew); got != 1 {
		t.Fatalf("first reinforce count = %d, want 1", got)
	}
	// A title differing only by case/whitespace reinforces the SAME entry.
	if got := s.Reinforce("  prefer   tabs ", HintNew); got != 2 {
		t.Fatalf("second (normalized-equal) reinforce count = %d, want 2", got)
	}
}

// Test 29: 25 reinforces ⇒ count 26 (NO reference engine cap of 20).
func TestReinforceHasNoBrainCap(t *testing.T) {
	s := New(t.TempDir())
	s.SetNow(func() time.Time { return at(0) })
	last := 0
	for i := 0; i < 26; i++ {
		last = s.Reinforce("Some Rule", HintNew)
	}
	if last != 26 {
		t.Fatalf("26 reinforces ⇒ count 26 (no cap), got %d", last)
	}
}

// Test 30: a draft reinforced twice (≥2 INDEPENDENT reinforces; weight reaches 3
// from the initial draft-creating 1) with hint ≥ new ⇒ promoted to live; a
// one-off draft stays draft. BR-A1.5-2 (RISK-5 off-by-one fix): the count must
// reach 3 — the draft-creating write (1) + two independent reinforces — NOT 2.
func TestPromoteDraftToLiveOnReinforceTwice(t *testing.T) {
	dir := t.TempDir()
	inbox := filepath.Join(dir, "_inbox")
	p := writeRule(t, inbox, "d.md",
		"---\nid: d\ntitle: Prefer Tabs\nstatus: draft\nconfidence: 0.65\nweight: 1\n---\nPrefer tabs over spaces.")
	other := writeRule(t, inbox, "o.md",
		"---\nid: o\ntitle: One Off\nstatus: draft\nconfidence: 0.5\nweight: 1\n---\nA one-off draft.")

	s := New(dir)
	s.SetNow(func() time.Time { return at(0) })
	// The draft-creating write counts as reinforce #1 (learn.process reinforces on
	// the create), then TWO independent reinforces ⇒ count 3 ⇒ promote.
	s.Reinforce("Prefer Tabs", HintNew) // create (count 1)
	s.Reinforce("Prefer Tabs", HintNew) // independent #1 (count 2)
	s.Reinforce("Prefer Tabs", HintNew) // independent #2 (count 3) ⇒ eligible

	// The one-off draft gets only its creating write (count 1) — must NOT promote.
	s.Reinforce("One Off", HintNew)

	res, err := s.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if loadRule(t, p).Status != "live" {
		t.Fatalf("thrice-counted (create+2 reinforce) draft must be promoted to live; got %q", loadRule(t, p).Status)
	}
	if loadRule(t, other).Status != "draft" {
		t.Fatalf("one-off draft must stay draft; got %q", loadRule(t, other).Status)
	}
	if len(res.Promoted) != 1 || res.Promoted[0] != "d" {
		t.Fatalf("Promoted = %v, want [d]", res.Promoted)
	}
}

// Test 31: a sigil naming a draft (active rule reinforced) promotes to live.
// Modeled as: an active rule, reinforced (the sigil repeat), crosses to live.
func TestPromoteDraftToLiveOnSigil(t *testing.T) {
	dir := t.TempDir()
	p := writeRule(t, filepath.Join(dir, "remembered", "2026-06-23"), "a.md",
		"---\nid: a\ntitle: Use Postgres\nstatus: active\nconfidence: 0.8\nweight: 1\n---\nAlways use PostgreSQL.")
	s := New(dir)
	s.SetNow(func() time.Time { return at(0) })
	// The original /remember write counts as #1; two further sigil repeats reach
	// count 3. A sigil's hint is validated (>= new), so the hint-gate is satisfied.
	s.Reinforce("Use Postgres", HintValidated) // original /remember (count 1)
	s.Reinforce("Use Postgres", HintValidated) // sigil repeat (count 2)
	s.Reinforce("Use Postgres", HintValidated) // sigil repeat (count 3) ⇒ eligible
	if _, err := s.Sweep(); err != nil {
		t.Fatal(err)
	}
	if loadRule(t, p).Status != "live" {
		t.Fatalf("sigil-repeated active rule must promote to live; got %q", loadRule(t, p).Status)
	}
}

// RISK-4: a draft that HITS the count threshold (rc>=3) but whose LATEST quality
// hint is `rejected` must NOT promote (BR-A1.5-2 requires hint >= new). A second
// draft that satisfies BOTH the count AND the hint DOES promote. This is the
// hint-gate that `validated` being dead let through: count-only promotion.
func TestPromoteRequiresLatestHintAtLeastNew(t *testing.T) {
	dir := t.TempDir()
	inbox := filepath.Join(dir, "_inbox")
	// "gated": reaches count 3 but the LATEST reinforce hint is rejected.
	gated := writeRule(t, inbox, "g.md",
		"---\nid: g\ntitle: Gated Draft\nstatus: draft\nconfidence: 0.65\nweight: 1\n---\nA draft whose latest hint is rejected.")
	// "ok": reaches count 3 AND the latest hint is >= new.
	ok := writeRule(t, inbox, "k.md",
		"---\nid: k\ntitle: Ok Draft\nstatus: draft\nconfidence: 0.65\nweight: 1\n---\nA draft whose latest hint is new.")

	s := New(dir)
	s.SetNow(func() time.Time { return at(0) })

	// Gated: create + 2 reinforces ⇒ count 3, but the LATEST hint is rejected.
	s.Reinforce("Gated Draft", HintNew)      // create (count 1)
	s.Reinforce("Gated Draft", HintNew)      // count 2
	s.Reinforce("Gated Draft", HintRejected) // count 3, latest hint REJECTED

	// Ok: create + 2 reinforces ⇒ count 3, latest hint new.
	s.Reinforce("Ok Draft", HintNew) // create (count 1)
	s.Reinforce("Ok Draft", HintNew) // count 2
	s.Reinforce("Ok Draft", HintNew) // count 3, latest hint new

	res, err := s.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if got := loadRule(t, gated).Status; got != "draft" {
		t.Fatalf("a draft at the count threshold whose LATEST hint is rejected must NOT promote; got %q", got)
	}
	if got := loadRule(t, ok).Status; got != "live" {
		t.Fatalf("a draft satisfying BOTH count and hint must promote to live; got %q", got)
	}
	if len(res.Promoted) != 1 || res.Promoted[0] != "k" {
		t.Fatalf("Promoted = %v, want [k] only", res.Promoted)
	}
}

// RISK-5 (off-by-one): the draft-CREATING write counts as sighting #1, so
// create + exactly ONE independent reinforce is only count 2 — BELOW the rc>=3
// promote threshold. It must stay a draft (the old rc>=2 gate promoted here, one
// reinforcement too early).
func TestPromoteOffByOneCreatePlusOneStaysDraft(t *testing.T) {
	dir := t.TempDir()
	inbox := filepath.Join(dir, "_inbox")
	p := writeRule(t, inbox, "d.md",
		"---\nid: d\ntitle: Borderline\nstatus: draft\nconfidence: 0.65\nweight: 1\n---\nA borderline correction.")
	s := New(dir)
	s.SetNow(func() time.Time { return at(0) })
	s.Reinforce("Borderline", HintNew) // create (count 1)
	s.Reinforce("Borderline", HintNew) // ONE independent reinforce (count 2)
	res, err := s.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if got := loadRule(t, p).Status; got != "draft" {
		t.Fatalf("create + ONE reinforce (count 2) must stay draft (RISK-5 off-by-one); got %q", got)
	}
	if len(res.Promoted) != 0 {
		t.Fatalf("nothing must promote at count 2; Promoted=%v", res.Promoted)
	}
}

// Test 33: the class-cross write oracle. (a) status change, (b) confBucket flip,
// (c) effWeight crossing the demote floor with an UNCHANGED bucket ⇒ EACH fires
// a write; a sub-bucket drift with no floor cross writes NOTHING.
func TestClassCrossWriteOnStatusOrBucketOrDemoteFloor(t *testing.T) {
	// (c) decay crosses the demote floor: a rule reinforced 100 days ago,
	// half_life 10 ⇒ effWeight ≈ exp(-10) ≈ 0 < 0.2 floor ⇒ demoted (status
	// change is the trigger here; the key is the FLOOR cross drives it even though
	// the static confBucket — from confidence 0.9 — would not flip).
	dir := t.TempDir()
	old := at(0).AddDate(0, 0, -100).Format(time.RFC3339)
	p := writeRule(t, dir, "decayed.md",
		"---\nid: decayed\ntitle: Old Rule\nstatus: live\nconfidence: 0.9\nweight: 1\nhalf_life_days: 10\nlast_reinforced_at: "+old+"\n---\nAn old rule.")
	s := New(dir)
	s.SetNow(func() time.Time { return at(0) })
	res, err := s.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if loadRule(t, p).Status != "superseded" {
		t.Fatalf("a decayed below-floor rule must be demoted; got %q", loadRule(t, p).Status)
	}
	if res.Writes < 1 {
		t.Fatalf("the floor cross must fire a durable write; writes=%d", res.Writes)
	}

	// A fresh rule with no decay and no class-cross ⇒ NO write (mtime unchanged).
	dir2 := t.TempDir()
	fresh := at(0).Format(time.RFC3339)
	p2 := writeRule(t, dir2, "fresh.md",
		"---\nid: fresh\ntitle: Fresh Rule\nstatus: live\nconfidence: 0.9\nweight: 1\nhalf_life_days: 30\nlast_reinforced_at: "+fresh+"\n---\nA fresh rule.")
	before, _ := os.Stat(p2)
	s2 := New(dir2)
	s2.SetNow(func() time.Time { return at(0) })
	res2, err := s2.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(p2)
	if res2.Writes != 0 {
		t.Fatalf("a no-class-cross sweep must write NOTHING; writes=%d", res2.Writes)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("a no-class-cross sweep must not touch the file mtime")
	}
}

// Test 34: 100 reinforces (hot counters) ⇒ the rule's .md mtime UNCHANGED; hits
// live only in the local store (the frontmatter-immutability rule).
func TestHotCountersNeverTouchFrontmatter(t *testing.T) {
	dir := t.TempDir()
	fresh := at(0).Format(time.RFC3339)
	p := writeRule(t, dir, "r.md",
		"---\nid: r\ntitle: Hot Rule\nstatus: live\nconfidence: 0.9\nweight: 1\nlast_reinforced_at: "+fresh+"\n---\nA hot rule.")
	before, _ := os.Stat(p)
	s := New(dir)
	s.SetNow(func() time.Time { return at(0) })
	for i := 0; i < 100; i++ {
		s.Reinforce("Hot Rule", HintNew)
	}
	after, _ := os.Stat(p)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("hot reinforce counters must NEVER touch frontmatter (the frontmatter-immutability rule)")
	}
	if s.ReinforceCount("Hot Rule") != 100 {
		t.Fatalf("reinforce count must be tracked locally; got %d", s.ReinforceCount("Hot Rule"))
	}
}

// Test 5 (BL-2 lifecycle): a decayed non-immune rule is demoted; a guardrail, a
// timeless, and a criticality:"8" rule are NOT demoted.
func TestDecayDemotesButImmuneNot(t *testing.T) {
	dir := t.TempDir()
	old := at(0).AddDate(0, 0, -100).Format(time.RFC3339)
	mk := func(id, extra string) string {
		return writeRule(t, dir, id+".md",
			"---\nid: "+id+"\ntitle: Rule "+id+"\nstatus: live\nconfidence: 0.9\nweight: 1\nhalf_life_days: 10\nlast_reinforced_at: "+old+"\n"+extra+"---\nbody "+id)
	}
	pNorm := mk("norm", "")
	pGuard := mk("guard", "kind: guardrail\n")
	pTimeless := mk("timeless", "timeless: true\n")
	pCrit := mk("crit", "criticality: \"8\"\n")

	s := New(dir)
	s.SetNow(func() time.Time { return at(0) })
	if _, err := s.Sweep(); err != nil {
		t.Fatal(err)
	}
	if loadRule(t, pNorm).Status != "superseded" {
		t.Fatalf("non-immune decayed rule must be demoted; got %q", loadRule(t, pNorm).Status)
	}
	for name, p := range map[string]string{"guardrail": pGuard, "timeless": pTimeless, "criticality8": pCrit} {
		if got := loadRule(t, p).Status; got != "live" {
			t.Fatalf("%s rule must NOT be demoted; got %q", name, got)
		}
	}
}

// Test 35: an opposite-meaning same-title correction supersedes the prior rule.
func TestSupersedeOnOppositeMeaning(t *testing.T) {
	dir := t.TempDir()
	p := writeRule(t, dir, "r.md",
		"---\nid: r\ntitle: Database Choice\nstatus: live\nconfidence: 0.9\nweight: 1\n---\nUse MySQL.")
	s := New(dir)
	s.SetNow(func() time.Time { return at(0) })
	written, err := s.MarkSupersede("Database Choice")
	if err != nil {
		t.Fatal(err)
	}
	if written == "" {
		t.Fatal("MarkSupersede must write the superseded rule")
	}
	if loadRule(t, p).Status != "superseded" {
		t.Fatalf("opposite-meaning correction must supersede the prior rule; got %q", loadRule(t, p).Status)
	}
	// An immune (guardrail) rule is NOT superseded.
	dir2 := t.TempDir()
	writeRule(t, dir2, "g.md",
		"---\nid: g\ntitle: Safety Rule\nkind: guardrail\nstatus: live\nconfidence: 0.9\nweight: 1\n---\nNever delete prod.")
	s2 := New(dir2)
	if w, _ := s2.MarkSupersede("Safety Rule"); w != "" {
		t.Fatal("a guardrail must NOT be superseded")
	}
}

// The sweep refreshes the cached days_since_reinforced from the timestamp on a
// class-cross (M3-R8) so the hot path effWeight stays current.
func TestSweepRefreshesCachedDays(t *testing.T) {
	dir := t.TempDir()
	old := at(0).AddDate(0, 0, -45).Format(time.RFC3339)
	// half_life 30, 45 days → effWeight ~0.22 (still above the 0.2 floor) but the
	// confBucket from confidence 0.9 stays H — so this sweep should NOT write
	// (no class-cross). Use a case that DOES cross to verify the refresh: 100 days.
	old100 := at(0).AddDate(0, 0, -100).Format(time.RFC3339)
	_ = old
	p := writeRule(t, dir, "r.md",
		"---\nid: r\ntitle: Decay Rule\nstatus: live\nconfidence: 0.9\nweight: 1\nhalf_life_days: 30\nlast_reinforced_at: "+old100+"\n---\nbody")
	s := New(dir)
	s.SetNow(func() time.Time { return at(0) })
	if _, err := s.Sweep(); err != nil {
		t.Fatal(err)
	}
	r := loadRule(t, p)
	// Demoted (100/30 ≈ 3.3 half-lives → effWeight ≈ 0.09 < 0.2) AND the cached
	// days refreshed to ~100.
	if r.Status != "superseded" {
		t.Fatalf("status = %q, want superseded", r.Status)
	}
	if r.DaysSinceReinforced < 99 || r.DaysSinceReinforced > 101 {
		t.Fatalf("days_since_reinforced not refreshed from timestamp: %v", r.DaysSinceReinforced)
	}
	_ = strings.TrimSpace("")
}

// P2-reinforce-ts: a reinforce event must re-anchor the decay clock. A rule
// last written 100 days ago (half_life 10) would, by its frontmatter timestamp
// alone, decay to ~0 and be demoted. But if it was REINFORCED recently, decay
// must anchor at the LATEST reinforcement, not creation — so it stays live AND
// the persisted last_reinforced_at advances to the reinforce time. Before the
// fix the sweep read only the stale frontmatter timestamp and wrongly demoted a
// freshly-reinforced rule.
func TestReinforceReAnchorsDecayClock(t *testing.T) {
	dir := t.TempDir()
	old := at(0).AddDate(0, 0, -100).Format(time.RFC3339)
	p := writeRule(t, dir, "r.md",
		"---\nid: r\ntitle: Old But Reinforced\nstatus: live\nconfidence: 0.9\nweight: 1\nhalf_life_days: 10\nlast_reinforced_at: "+old+"\n---\nbody")

	s := New(dir)
	s.SetNow(func() time.Time { return at(0) })

	// Reinforce NOW: the decay clock must re-anchor to this instant.
	s.Reinforce("Old But Reinforced", HintValidated)

	res, err := s.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	r := loadRule(t, p)
	// Anchored at NOW (0 days elapsed) ⇒ effWeight ≈ full ⇒ NOT demoted.
	if r.Status != "live" {
		t.Fatalf("a freshly-reinforced rule must NOT be demoted (decay re-anchored at the reinforce); got %q", r.Status)
	}
	for _, id := range res.Demoted {
		if id == "r" {
			t.Fatal("reinforced rule must not appear in Demoted")
		}
	}
	// The persisted last_reinforced_at must advance to ~now (within a day), and
	// the cached days_since_reinforced must reflect the re-anchor (≈0, not ≈100).
	newAnchor := parseTime(r)
	if newAnchor.Before(at(0).AddDate(0, 0, -1)) {
		t.Fatalf("last_reinforced_at must advance to the reinforce time (~now); got %v", r.LastReinforcedAt)
	}
	if r.DaysSinceReinforced > 1 {
		t.Fatalf("days_since_reinforced must re-anchor near 0 after a reinforce; got %v", r.DaysSinceReinforced)
	}
}

// P2-reinforce-ts: a rule that is NOT reinforced keeps decaying from its
// frontmatter timestamp (the re-anchor must NOT spuriously rescue a stale rule).
func TestNoReinforceStillDecaysFromFrontmatter(t *testing.T) {
	dir := t.TempDir()
	old := at(0).AddDate(0, 0, -100).Format(time.RFC3339)
	p := writeRule(t, dir, "r.md",
		"---\nid: r\ntitle: Stale Rule\nstatus: live\nconfidence: 0.9\nweight: 1\nhalf_life_days: 10\nlast_reinforced_at: "+old+"\n---\nbody")
	s := New(dir)
	s.SetNow(func() time.Time { return at(0) })
	// NO reinforce. The 100-day-old rule must still demote (decay from frontmatter).
	if _, err := s.Sweep(); err != nil {
		t.Fatal(err)
	}
	if loadRule(t, p).Status != "superseded" {
		t.Fatalf("an un-reinforced 100-day-old rule must still decay+demote; got %q", loadRule(t, p).Status)
	}
}

// P2-reinforce-ts churn regression: the re-anchor compares the in-session
// reinforce time against the frontmatter last_reinforced_at, but the frontmatter
// is stored at SECOND granularity (RFC3339). If the reinforce clock carries
// sub-second precision, a naive rt.After(last) keeps firing on EVERY sweep
// (rt=12:00:00.5 is forever "after" the second-truncated disk value 12:00:00)
// → a self-perpetuating frontmatter re-write (churn / dirty git). The existing
// tests miss this because at uses a zero-nanosecond clock. This test uses a
// sub-second clock and a frontmatter anchor ALREADY at the same second, then
// asserts the next sweep is a true no-op (file bytes + mtime unchanged).
func TestReinforceReAnchorNoChurnAtSecondGranularity(t *testing.T) {
	dir := t.TempDir()
	// A sub-second wall clock. Its SECOND-truncated value equals the frontmatter
	// anchor below, so once anchored there is nothing new to persist.
	subSec := time.Date(2026, 6, 23, 12, 0, 0, 500_000_000, time.UTC) // 12:00:00.5
	anchor := subSec.Truncate(time.Second).UTC().Format(time.RFC3339) // "...T12:00:00Z"
	p := writeRule(t, dir, "r.md",
		"---\nid: r\ntitle: Steady Rule\nstatus: live\nconfidence: 0.9\nweight: 1\nhalf_life_days: 10\nlast_reinforced_at: "+anchor+"\n---\nbody")

	s := New(dir)
	s.SetNow(func() time.Time { return subSec })

	// Reinforce at the sub-second instant; its truncated second == the on-disk
	// anchor, so the re-anchor must be a no-op.
	s.Reinforce("Steady Rule", HintValidated)

	// Capture the exact bytes + mtime before the sweeps.
	beforeBytes, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	beforeStat, _ := os.Stat(p)

	// Sweep TWICE — a churning re-anchor would rewrite frontmatter each time.
	for i := 0; i < 2; i++ {
		if _, err := s.Sweep(); err != nil {
			t.Fatal(err)
		}
	}

	afterBytes, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	afterStat, _ := os.Stat(p)

	if !bytes.Equal(beforeBytes, afterBytes) {
		t.Fatalf("frontmatter must NOT be rewritten when the reinforce is already at\nsecond granularity on disk (churn).\nbefore:\n%s\nafter:\n%s", beforeBytes, afterBytes)
	}
	if !beforeStat.ModTime().Equal(afterStat.ModTime()) {
		t.Fatal("a no-op sweep must not touch the file mtime (sub-second re-anchor churn)")
	}
}

// P0-2 regression: the reinforce count must survive a daemon restart so that
// "the same correction across days, with a restart between" promotes to live.
// We reinforce once, persist, construct a NEW Sweeper from disk (simulating a
// restart), reinforce again, and assert the count reaches 2 → the draft
// promotes. Before the fix, the in-mem counts map was lost on restart and the
// second session saw count=1 → the draft NEVER promoted.
func TestReinforceCountSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	inbox := filepath.Join(dir, "_inbox")
	p := writeRule(t, inbox, "d.md",
		"---\nid: d\ntitle: Prefer Tabs\nstatus: draft\nconfidence: 0.65\nweight: 1\n---\nPrefer tabs over spaces.")

	// Session 1: the draft-creating write + one independent reinforce (count 2),
	// then the daemon stops. Below the rc>=3 promote threshold (BR-A1.5-2), so it
	// must NOT promote yet.
	s1 := New(dir)
	s1.SetNow(func() time.Time { return at(0) })
	if got := s1.Reinforce("Prefer Tabs", HintNew); got != 1 { // create (count 1)
		t.Fatalf("session1 first reinforce count = %d, want 1", got)
	}
	if got := s1.Reinforce("Prefer Tabs", HintNew); got != 2 { // independent #1 (count 2)
		t.Fatalf("session1 second reinforce count = %d, want 2", got)
	}
	// Count 2 is below the rc>=3 promote threshold → must NOT promote yet.
	if res, err := s1.Sweep(); err != nil {
		t.Fatal(err)
	} else if len(res.Promoted) != 0 {
		t.Fatalf("two sightings (create+1) must not promote (threshold is 3); Promoted=%v", res.Promoted)
	}
	if loadRule(t, p).Status != "draft" {
		t.Fatalf("below the promote threshold the rule must still be a draft; got %q", loadRule(t, p).Status)
	}

	// Session 2: a BRAND-NEW Sweeper over the SAME vault (the daemon restarted).
	// The count from session 1 must have been persisted to disk and reloaded.
	s2 := New(dir)
	s2.SetNow(func() time.Time { return at(7) }) // a week later
	if got := s2.ReinforceCount("Prefer Tabs"); got != 2 {
		t.Fatalf("after restart the persisted count must be 2, got %d", got)
	}
	if got := s2.Reinforce("Prefer Tabs", HintNew); got != 3 { // independent #2 (count 3) ⇒ eligible
		t.Fatalf("session2 reinforce count = %d, want 3 (2 persisted + 1 new)", got)
	}
	res, err := s2.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if loadRule(t, p).Status != "live" {
		t.Fatalf("the same correction across a restart must promote to live; got %q", loadRule(t, p).Status)
	}
	if len(res.Promoted) != 1 || res.Promoted[0] != "d" {
		t.Fatalf("Promoted = %v, want [d]", res.Promoted)
	}
}
