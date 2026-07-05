package extract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedNow() time.Time { return time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC) }

func newExtractor(t *testing.T, cfg Config) (*Extractor, string) {
	t.Helper()
	vault := t.TempDir()
	e := New(vault, cfg)
	e.SetNow(fixedNow)
	return e, vault
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// Test 12: a sigil `/remember prefer tabs` in the newest user msg ⇒ one
// status:active rule at remembered/<date>/<id>.md with body `prefer tabs`.
func TestSigilRememberWritesActiveDatedFile(t *testing.T) {
	e, vault := newExtractor(t, Config{})
	res := e.Process(Exchange{NewUserLines: []string{"/remember prefer tabs over spaces"}})
	if res.Tier != "T0" || res.Status != "active" {
		t.Fatalf("want T0/active, got %+v", res)
	}
	want := filepath.Join(vault, "remembered", "2026-06-23")
	if !strings.HasPrefix(res.WrotePath, want) {
		t.Fatalf("file must be under %s, got %s", want, res.WrotePath)
	}
	doc := readFile(t, res.WrotePath)
	if !strings.Contains(doc, "status: active") {
		t.Fatalf("frontmatter must be status: active, got:\n%s", doc)
	}
	if !strings.Contains(doc, "prefer tabs over spaces") {
		t.Fatalf("body must be the payload, got:\n%s", doc)
	}
}

// Test 16: `/rule api_key=sk_live_...` ⇒ NOTHING written (credential hard-zero).
func TestSigilCredentialDropped(t *testing.T) {
	e, vault := newExtractor(t, Config{})
	res := e.Process(Exchange{NewUserLines: []string{"/rule api_key=sk_live_abcdefghijklmnop1234"}})
	if res.WrotePath != "" {
		t.Fatalf("a credential sigil must write nothing, got %s", res.WrotePath)
	}
	// No file anywhere in the vault.
	var found bool
	_ = filepath.WalkDir(vault, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".md") {
			found = true
		}
		return nil
	})
	if found {
		t.Fatal("no .md should be written for a credential sigil")
	}
}

// Test 17: wrong-case `/Remember` and non-line-leading `please /remember` are
// NOT sigils → fall through to T1 (which, lacking pref signal, writes nothing).
func TestSigilCaseSensitiveAndLineLeading(t *testing.T) {
	e, _ := newExtractor(t, Config{})
	for _, line := range []string{"/Remember prefer tabs", "please /remember to call mom"} {
		res := e.Process(Exchange{NewUserLines: []string{line}, UserText: line})
		if res.Tier == "T0" && res.Status == "active" {
			t.Fatalf("%q must NOT be a sigil", line)
		}
	}
}

// Test 15: the same `/remember` line resent across 3 turns ⇒ exactly ONE active
// rule (seen-set idempotency).
func TestSigilResentHistoryNotReduplicated(t *testing.T) {
	e, vault := newExtractor(t, Config{})
	line := "/remember always use the staging database for tests"
	for i := 0; i < 3; i++ {
		e.Process(Exchange{NewUserLines: []string{line}})
	}
	n := countMD(t, filepath.Join(vault, "remembered"))
	if n != 1 {
		t.Fatalf("a resent sigil must write exactly ONE rule, got %d", n)
	}
}

// Test 14: a `/remember` in turn N, then turn N+1 with a NEW question whose
// NewUserLines no longer contain the sigil ⇒ the turn-N rule survives (not
// re-lost) and no second write occurs.
func TestSigilInPriorTurnHistoryNotRelost(t *testing.T) {
	e, vault := newExtractor(t, Config{})
	// Turn N: the sigil is in the per-turn delta.
	e.Process(Exchange{NewUserLines: []string{"/remember prefer the repository pattern for data access"}})
	// Turn N+1: a fresh question; the delta is just the new line (no sigil).
	e.Process(Exchange{NewUserLines: []string{"what is the repository pattern?"},
		UserText: "what is the repository pattern?"})
	n := countMD(t, filepath.Join(vault, "remembered"))
	if n != 1 {
		t.Fatalf("the turn-N sigil rule must persist (exactly 1), got %d", n)
	}
}

// Test 21: `всегда используй табы` ⇒ score 0.65, pref_hits 1 ⇒ staged as draft.
func TestT1StagesSinglePrefAboveFloor(t *testing.T) {
	e, vault := newExtractor(t, Config{})
	res := e.Process(Exchange{UserText: "всегда используй табы", AssistantText: "ок"})
	if res.Tier != "T1" || res.Status != "draft" {
		t.Fatalf("want T1/draft, got %+v (skipped=%q)", res, res.Skipped)
	}
	if !strings.HasPrefix(res.WrotePath, filepath.Join(vault, "_inbox")) {
		t.Fatalf("draft must be under _inbox/, got %s", res.WrotePath)
	}
	doc := readFile(t, res.WrotePath)
	if !strings.Contains(doc, "status: draft") {
		t.Fatalf("draft frontmatter must be status: draft, got:\n%s", doc)
	}
}

// FIX 2 (review fix), end-to-end through the real Process→writeDraft seam: the
// FIRST draft write must drop an _inbox/.gitignore so a git-tracked vault never
// accidentally commits the unreviewed draft (or the .counts.json sidecar).
func TestFirstDraftWritesInboxGitignore(t *testing.T) {
	e, vault := newExtractor(t, Config{})
	res := e.Process(Exchange{UserText: "всегда используй табы", AssistantText: "ок"})
	if res.Status != "draft" {
		t.Fatalf("precondition: expected a T1 draft, got %+v (skipped=%q)", res, res.Skipped)
	}
	gi := filepath.Join(vault, "_inbox", ".gitignore")
	body, err := os.ReadFile(gi)
	if err != nil {
		t.Fatalf("first draft must create _inbox/.gitignore: %v", err)
	}
	// Must ignore everything in _inbox/ except the marker itself (covers the draft
	// .md AND the .counts.json sidecar, current and future).
	if !strings.Contains(string(body), "*") || !strings.Contains(string(body), "!.gitignore") {
		t.Fatalf("_inbox/.gitignore must ignore-all-but-self, got:\n%s", body)
	}
}

// Test 20 (BR-A8): the 8-let-opyta class is NOT staged (score 0.5, pref 0).
func TestT1Rejects8LetOpytaClass(t *testing.T) {
	e, vault := newExtractor(t, Config{})
	for _, txt := range []string{"у меня 8 лет опыта", "I have 8 years of experience"} {
		res := e.Process(Exchange{UserText: txt, AssistantText: "ok"})
		if res.WrotePath != "" {
			t.Fatalf("%q must NOT be staged, got %s", txt, res.WrotePath)
		}
	}
	if countMD(t, filepath.Join(vault, "_inbox")) != 0 {
		t.Fatal("no draft should exist for the 8-let-opyta class")
	}
}

// Test 13: `всегда используй табы` staged proves the gate keys on the COUNT
// (pref_hits 1), NOT the preference_signal flag (which is unset at 1 hit).
func TestClassifyQualityCountNotFlag(t *testing.T) {
	q := classifyQuality("всегда используй табы", "")
	if q.PrefHits != 1 {
		t.Fatalf("pref_hits = %d, want 1", q.PrefHits)
	}
	for _, f := range q.Flags {
		if f == "preference_signal" {
			t.Fatal("preference_signal flag must be ABSENT at 1 hit")
		}
	}
	// And the extractor stages it (gate on count).
	e, _ := newExtractor(t, Config{})
	res := e.Process(Exchange{UserText: "всегда используй табы"})
	if res.Status != "draft" {
		t.Fatalf("single-pref correction must be staged (gate on count), got %+v", res)
	}
}

// Test 23 (M3-R6): allow_cloud=false ⇒ T2 never runs, no network contacted.
func TestT2OffByDefaultNoNetwork(t *testing.T) {
	var called bool
	gate := func(string) (string, float64, error) { called = true; return "WHAT: x\nHOW: y\nBETTER: z", 0.01, nil }
	// AllowCloud false (default) even though a Gate is wired.
	e, _ := newExtractor(t, Config{AllowCloud: false, Gate: gate})
	e.Process(Exchange{UserText: "always prefer composition over inheritance because it's the rule"})
	if called {
		t.Fatal("T2 gate must NOT be called when allow_cloud=false")
	}
}

// Test 24: gate returns SKIP ⇒ no refine (the T1 draft stands, no T2 body).
func TestT2SkipSentinelWritesNothing(t *testing.T) {
	gate := func(string) (string, float64, error) { return "SKIP", 0.01, nil }
	e, _ := newExtractor(t, Config{AllowCloud: true, Gate: gate})
	res := e.Process(Exchange{UserText: "always prefer composition over inheritance because rule"})
	// T1 still staged; T2 did not refine → tier stays T1.
	if res.Tier != "T1" {
		t.Fatalf("SKIP must leave the T1 draft (tier T1), got %+v", res)
	}
}

// Test 25 (F-9): gate returns prose with `WHAT:` mid-sentence but no anchored
// WHAT/HOW/BETTER lines ⇒ keep==[] ⇒ no refine.
func TestT2ProseContainingWhatColonNotWritten(t *testing.T) {
	out := "I think the answer to WHAT: you asked is unclear, so here is some prose."
	if l, ok := runGate(out); ok {
		t.Fatalf("prose containing WHAT: mid-sentence must yield no lesson, got %q", l)
	}
}

// Test 26: a non-SKIP gate refines the EXISTING T1 draft (no second draft).
func TestT2RefinesExistingT1Draft(t *testing.T) {
	gate := func(string) (string, float64, error) {
		return "WHAT: prefer composition\nHOW: inject deps\nBETTER: avoid deep inheritance", 0.01, nil
	}
	e, vault := newExtractor(t, Config{AllowCloud: true, Gate: gate})
	res := e.Process(Exchange{UserText: "always prefer composition over inheritance because rule"})
	if res.Tier != "T2" {
		t.Fatalf("want T2 refine, got %+v", res)
	}
	if n := countMD(t, filepath.Join(vault, "_inbox")); n != 1 {
		t.Fatalf("T2 must REFINE (1 draft), not create a second; got %d", n)
	}
	doc := readFile(t, res.WrotePath)
	if !strings.Contains(doc, "WHAT: prefer composition") || !strings.Contains(doc, "status: draft") {
		t.Fatalf("draft must be refined with the WHAT/HOW/BETTER body AND stay draft:\n%s", doc)
	}
}

// Test 27: per-day cost cap reached ⇒ T2 stops; CostToday is visible.
func TestT2CostCapEnforcedAndVisible(t *testing.T) {
	calls := 0
	gate := func(string) (string, float64, error) {
		calls++
		return "WHAT: a\nHOW: b\nBETTER: c", 0.10, nil
	}
	e, _ := newExtractor(t, Config{AllowCloud: true, Gate: gate, CostCapPerDay: 0.15})
	// First exchange: spends 0.10 (under cap) → runs.
	e.Process(Exchange{UserText: "always prefer composition over inheritance because rule one"})
	// Second exchange: 0.10 spent already >= would-exceed; but the cap check is
	// "spent >= cap" → 0.10 < 0.15 so it runs once more, spending 0.20.
	e.Process(Exchange{UserText: "never use globals because shared mutable state is the rule two"})
	spentMid := e.cfg.CostToday(fixedNow())
	if spentMid < 0.15 {
		t.Fatalf("two calls should exceed the cap, spent=%v", spentMid)
	}
	// Third exchange: now spent >= cap → T2 must NOT run.
	before := calls
	e.Process(Exchange{UserText: "always document public APIs because it is the rule three"})
	if calls != before {
		t.Fatalf("T2 must stop once the cost cap is reached; calls went %d→%d", before, calls)
	}
}

// Test 46 (F-11): a lossy capture skips T2 but still runs T1 on the user
// correction (which comes from CleanMessagesJSON, never lossy).
func TestLossyCaptureSkipsT2KeepsT1(t *testing.T) {
	var gateCalled bool
	gate := func(string) (string, float64, error) {
		gateCalled = true
		return "WHAT: a\nHOW: b\nBETTER: c", 0.01, nil
	}
	e, vault := newExtractor(t, Config{AllowCloud: true, Gate: gate})
	res := e.Process(Exchange{
		UserText:      "always prefer composition over inheritance because rule",
		AssistantText: "truncated...",
		Lossy:         true,
	})
	if gateCalled {
		t.Fatal("T2 must be skipped on a lossy capture")
	}
	if res.Status != "draft" || countMD(t, filepath.Join(vault, "_inbox")) != 1 {
		t.Fatalf("T1 must still stage a draft on a lossy capture, got %+v", res)
	}
}

// P1-learn: TWO `/remember` lines in ONE user message ⇒ TWO active rules, each in
// its own file (BR-A4 one-file-per-rule). The loop must NOT return on the first
// sigil (the P1 bug: only the first was learned, the rest silently dropped).
func TestMultipleSigilsAllLearned(t *testing.T) {
	e, vault := newExtractor(t, Config{})
	res := e.Process(Exchange{NewUserLines: []string{
		"/remember prefer tabs over spaces",
		"/remember always run tests before deploy",
	}})
	// The exchange short-circuits at T0 (a sigil wrote) and reports active.
	if res.Tier != "T0" || res.Status != "active" {
		t.Fatalf("want T0/active, got %+v (skipped=%q)", res, res.Skipped)
	}
	if res.Hint != "validated" {
		t.Fatalf("a deliberate sigil must carry hint=validated, got %q", res.Hint)
	}
	// BOTH rules must be on disk as DISTINCT files.
	root := filepath.Join(vault, "remembered", "2026-06-23")
	if n := countMD(t, root); n != 2 {
		t.Fatalf("two sigils must write TWO distinct rule files, got %d", n)
	}
	// The two bodies must both be present (distinct content → distinct files).
	var haveTabs, haveTests bool
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		doc := readFile(t, p)
		if strings.Contains(doc, "prefer tabs over spaces") {
			haveTabs = true
		}
		if strings.Contains(doc, "always run tests before deploy") {
			haveTests = true
		}
		return nil
	})
	if !haveTabs || !haveTests {
		t.Fatalf("both rule bodies must be persisted (tabs=%v tests=%v)", haveTabs, haveTests)
	}
}

// P1-learn: the 2nd (and later) sigil must run the SAME credential gate as the
// first. A clean 1st sigil writes its rule; a credential-bearing 2nd sigil is
// dropped (never persisted), yet the 1st rule still stands. The per-sigil guard
// must NOT be skipped for the tail sigils.
func TestMultipleSigilsSecondCredentialGated(t *testing.T) {
	e, vault := newExtractor(t, Config{})
	res := e.Process(Exchange{NewUserLines: []string{
		"/remember prefer tabs over spaces",
		"/rule api_key=sk_live_abcdefghijklmnop1234",
	}})
	// A sigil wrote → short-circuit at T0, active.
	if res.Tier != "T0" || res.Status != "active" {
		t.Fatalf("want T0/active from the clean 1st sigil, got %+v", res)
	}
	// Exactly ONE rule: the clean one. The credential sigil wrote NOTHING.
	root := filepath.Join(vault, "remembered")
	if n := countMD(t, root); n != 1 {
		t.Fatalf("the credential 2nd sigil must NOT persist; want 1 rule, got %d", n)
	}
	// The persisted rule must be the clean one, and must NOT contain the secret.
	var leaked bool
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		if strings.Contains(readFile(t, p), "sk_live_abcdefghijklmnop1234") {
			leaked = true
		}
		return nil
	})
	if leaked {
		t.Fatal("a credential from the 2nd sigil must NEVER be persisted")
	}
}

// P1-learn: a credential-bearing FIRST sigil followed by a clean SECOND sigil.
// The credential is dropped but the scan must CONTINUE so the clean 2nd rule is
// still learned (the credential-drop must not abort the whole message).
func TestMultipleSigilsFirstCredentialSecondClean(t *testing.T) {
	e, vault := newExtractor(t, Config{})
	res := e.Process(Exchange{NewUserLines: []string{
		"/rule api_key=sk_live_abcdefghijklmnop1234",
		"/remember always run tests before deploy",
	}})
	// The clean 2nd sigil wrote → active (a write wins over a rejection).
	if res.Tier != "T0" || res.Status != "active" {
		t.Fatalf("want T0/active from the clean 2nd sigil, got %+v", res)
	}
	root := filepath.Join(vault, "remembered")
	if n := countMD(t, root); n != 1 {
		t.Fatalf("only the clean sigil persists; want 1 rule, got %d", n)
	}
	var haveTests, leaked bool
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		doc := readFile(t, p)
		if strings.Contains(doc, "always run tests before deploy") {
			haveTests = true
		}
		if strings.Contains(doc, "sk_live_abcdefghijklmnop1234") {
			leaked = true
		}
		return nil
	})
	if !haveTests {
		t.Fatal("the clean 2nd sigil must be learned even when the 1st is a credential")
	}
	if leaked {
		t.Fatal("the 1st-sigil credential must never be persisted")
	}
}

// P1-learn: two IDENTICAL sigils in one message dedup to ONE rule (the seen-set
// applies per-sigil), and a resend of the whole message adds nothing.
func TestMultipleSigilsDuplicateDedup(t *testing.T) {
	e, vault := newExtractor(t, Config{})
	msg := Exchange{NewUserLines: []string{
		"/remember prefer tabs over spaces",
		"/remember prefer tabs over spaces",
	}}
	e.Process(msg)
	if n := countMD(t, filepath.Join(vault, "remembered")); n != 1 {
		t.Fatalf("two identical sigils dedup to ONE rule, got %d", n)
	}
	// Resend the whole message: still one.
	e.Process(msg)
	if n := countMD(t, filepath.Join(vault, "remembered")); n != 1 {
		t.Fatalf("a resent duplicate message must add nothing, got %d", n)
	}
}

func countMD(t *testing.T, root string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(p, ".md") {
			n++
		}
		return nil
	})
	return n
}
