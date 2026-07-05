package rules

import (
	"strings"
	"testing"
	"time"
)

// M5 — SEMANTIC relevance. These tests define "done" for the milestone (the
// task's ACCEPTANCE CONTRACT). They go THROUGH the real BuildIndex →
// MatchAndGuard path so they exercise the actual concept-expansion seam wired
// into the index, not a hand-built bypass.
//
// The M5 problem: the lexical floor (BM25 + IDF word-coverage) needs a SHARED
// WORD between query and rule body. A rule "Always use PostgreSQL, never MySQL."
// (NO word "database" in the body) does NOT fire for "what database should I use
// for my app?" — yet they are semantically related (PostgreSQL IS a database).
// M5 closes that gap via a curated, embedded concept-expansion table WITHOUT
// regressing the no-false-positive property (weather → ZERO injection).

// m5PostgresVault is the EXACT acceptance rule: the body has NO word "database"
// — only "PostgreSQL"/"MySQL". Pre-M5 this rule could not fire for a "database"
// query (no shared word). Post-M5 the concept expansion (postgresql/mysql →
// database/sql/rdbms) makes it fire.
func m5PostgresVault() *Index {
	return BuildIndex([]Rule{{
		ID:         "use-postgres",
		Title:      "Use Postgres",
		Body:       "Always use PostgreSQL, never MySQL.",
		Kind:       "guardrail",
		Weight:     0.95,
		Confidence: 0.95,
	}})
}

// TestSemanticInjectsRelatedRuleWithoutSharedWord [M5 acceptance #1]: rule
// "Always use PostgreSQL, never MySQL." + query "what database should I use for
// my app?" → the rule IS now injected (semantic match on PostgreSQL ↔ database).
func TestSemanticInjectsRelatedRuleWithoutSharedWord(t *testing.T) {
	ix := m5PostgresVault()
	res, err := ix.MatchAndGuard("what database should I use for my app?", GuardConfig{})
	if err != nil {
		t.Fatalf("semantic query must inject the Postgres rule, got err=%v", err)
	}
	if len(res.Kept) != 1 || res.Kept[0].ID != "use-postgres" {
		t.Fatalf("semantic query must inject the use-postgres rule (no shared word); kept=%+v err=%v", res.Kept, err)
	}
}

// TestSemanticStillRejectsIrrelevant [M5 acceptance #2]: the SAME vault + query
// "what is the weather today?" → STILL zero injection (no false-positive
// regression). The concept table never expands an unrelated word into a tech
// concept, so weather can never reach the DB rule.
func TestSemanticStillRejectsIrrelevant(t *testing.T) {
	ix := m5PostgresVault()
	res, err := ix.MatchAndGuard("what is the weather today?", GuardConfig{})
	if err != ErrNoMatch || len(res.Kept) > 0 {
		t.Fatalf("weather query must STILL inject NOTHING after M5; got err=%v kept=%+v", err, res.Kept)
	}
}

// TestSemanticUnder15ms [M5 acceptance #3]: a realistic vault + query → the full
// match→guard stays well under the 15ms intercept budget. The semantic signal is
// precomputed at index (build) time, so the request path adds only a handful of
// O(1) map lookups (query-word concept expansion) on top of the existing scan.
func TestSemanticUnder15ms(t *testing.T) {
	ix := BuildIndex(realisticVault())
	q := "what database engine should I pick for a new web service, and how should I format the python code?"

	// Warm once, then measure the steady-state match→guard.
	_, _ = ix.MatchAndGuard(q, GuardConfig{})

	const iters = 200
	start := time.Now()
	for i := 0; i < iters; i++ {
		_, _ = ix.MatchAndGuard(q, GuardConfig{})
	}
	per := time.Since(start) / iters
	if per > 15*time.Millisecond {
		t.Fatalf("match→guard averaged %v/op, want <15ms (semantic must be precomputed at index time)", per)
	}
	t.Logf("match→guard (semantic, %d-rule vault): %v/op", len(realisticVault()), per)
}

// realisticVault is a mixed dev/tech vault used by the latency test (and the
// multi-rule semantic moat test) — enough rules and body text to be a fair
// steady-state intercept measurement.
func realisticVault() []Rule {
	return []Rule{
		{ID: "pg", Title: "Use Postgres", Body: "Always use PostgreSQL, never MySQL.", Kind: "guardrail", Weight: 0.95, Confidence: 0.95},
		{ID: "py-style", Title: "Python style", Body: "Format Python with black; never autopep8. Keep lines under 100 columns.", Weight: 0.8, Confidence: 0.8},
		{ID: "tabs", Title: "Indent with tabs", Body: "Indent source code with tabs, never spaces.", Weight: 0.7, Confidence: 0.7},
		{ID: "secrets", Title: "No secrets in code", Body: "Never commit credentials, API keys or tokens to the repository. Use environment variables.", Kind: "guardrail", Weight: 0.99, Confidence: 0.99},
		{ID: "tests", Title: "Write tests first", Body: "Practice test-driven development: write a failing test before the implementation.", Weight: 0.75, Confidence: 0.75},
		{ID: "git", Title: "Conventional commits", Body: "Write conventional commit messages: feat, fix, docs, chore, refactor.", Weight: 0.6, Confidence: 0.6},
		{ID: "docker", Title: "Pin images", Body: "Pin container base images to a digest, never the latest tag.", Weight: 0.7, Confidence: 0.7},
		{ID: "rest", Title: "REST naming", Body: "Name REST endpoints with plural nouns and use HTTP verbs for actions.", Weight: 0.65, Confidence: 0.65},
	}
}

// TestSemanticMultiRuleMoat: in a mixed vault, a clearly-DB query semantically
// fires ONLY the database rule (no shared word), a python query fires the python
// rule, and an off-topic query (cooking) fires nothing — proving the expansion
// is concept-scoped and never bleeds across domains.
func TestSemanticMultiRuleMoat(t *testing.T) {
	ix := BuildIndex(realisticVault())

	// DB query, no shared word with the pg rule body.
	res, err := ix.MatchAndGuard("which database should I choose for my app?", GuardConfig{})
	if err != nil || len(res.Kept) == 0 || res.Kept[0].ID != "pg" {
		t.Fatalf("database query must semantically fire the pg rule first; kept=%+v err=%v", res.Kept, err)
	}
	for _, r := range res.Kept {
		if r.ID == "secrets" || r.ID == "rest" || r.ID == "git" || r.ID == "docker" {
			t.Fatalf("database query bled into unrelated rule %q; kept=%+v", r.ID, res.Kept)
		}
	}

	// Off-topic: cooking. Nothing in the vault is about cooking.
	res2, err2 := ix.MatchAndGuard("how long should I boil an egg?", GuardConfig{})
	if err2 == nil && len(res2.Kept) > 0 {
		t.Fatalf("cooking query must inject nothing; kept=%+v", res2.Kept)
	}
}

// TestSemanticDoesNotRegressImperativeFloor: the P1-c imperative-only false
// inject must STILL be floored after M5 (the concept expansion must not
// reintroduce the bug). "always remember to call mom" must inject NOTHING.
func TestSemanticDoesNotRegressImperativeFloor(t *testing.T) {
	ix := m5PostgresVault()
	res, err := ix.MatchAndGuard("always remember to call mom", GuardConfig{})
	if err != ErrNoMatch || len(res.Kept) > 0 {
		t.Fatalf("imperative-only query must STILL inject nothing after M5; got err=%v kept=%+v", err, res.Kept)
	}
}

// offTopicBattery45 is the PERMANENT 45-query off-topic regression corpus for
// TestSemanticOffTopicBatteryNoFalsePositive. Every entry is a genuinely
// off-topic request (weather, cooking, sports, travel, feelings, creative
// writing, everyday life) — NONE is about a rule in realisticVault. Several are
// the exact traps the M5 concept-expansion moat regression injected on: their
// only in-vocabulary content word is a polysemous concept VALUE ("language",
// "ui", "style", "code", "container", "web", "version", "data", "image",
// "model", "search", "build", "service", "query", "spec") that M5 added to the
// corpus vocabulary, or a literal rule-body word ("commit", "write"). Pre-fix
// these injected 11+ rules; post-fix (OOV content words counted in the relevance
// denominator) they MUST all inject ZERO at the default 0.60 floor. Keep this
// list ≥45 and append new traps here — never delete one.
func offTopicBattery45() []string {
	return []string{
		// weather (3)
		"what is the weather today?", "will it rain tomorrow afternoon?", "how cold is it outside right now?",
		// cooking (3)
		"how long should I boil an egg?", "how do I bake chocolate chip cookies?", "what wine pairs with steak?",
		// sports (3)
		"who won the football game last night?", "when is the next olympic marathon?", "how do I throw a curveball?",
		// travel (3)
		"plan a trip to the mountains this weekend", "what is the cheapest flight to rome?", "is the beach crowded in july?",
		// feelings (3)
		"i feel sad and lonely tonight", "how do I stop feeling anxious?", "why am I so tired lately?",
		// creative writing (3)
		"write me a poem about the ocean", "sing me a song about summer", "tell me a bedtime story",
		// the documented concept-value / literal-word traps (8)
		"i want to learn a new language", "good ui for my living room", "style my hair for the wedding",
		"what is my favorite color code", "i want to commit to a relationship", "a container ship sank off the coast",
		"what time is it in tokyo right now?", "the latest version of the movie was great",
		// more polysemous concept-value traps (9)
		"a web of lies tangled the story", "the language of love is universal", "please style this room nicely",
		"my image in the mirror looked tired", "i need a service to clean my house", "what model of car should I buy?",
		"i want to query my doctor about symptoms", "the build quality of this chair is poor", "sketch an image of a sunset",
		// everyday life (10)
		"recommend a good movie to watch", "build me a treehouse for the kids", "how do I plant tomatoes in spring?",
		"what dog breed is good with children?", "remind me to buy milk and eggs", "is it going to snow on christmas?",
		"how do I tie a bow tie?", "what is the capital of australia?", "my car will not start this morning",
		"back up the old family photos before the trip",
		// floor-fix-f5 concept-VALUE traps (10): each off-topic query's only in-vocab
		// content word is a polysemous concept VALUE the floor-fix-f5 additions put
		// into the corpus vocabulary ("framework", "package", "dependency", "queue",
		// "messaging", "monitoring", "cloud", "concurrency", "branch", "merge"). These
		// MUST still inject ZERO at the 0.60 floor — the additions widen the table but
		// the OOV-content-word denominator keeps the moat shut. Append-only; never delete.
		"i admire the framework of his argument", "i need a holiday package to greece",
		"i depend on my family for support", "there was a long queue at the bakery",
		"the cloud cover made for a grey afternoon", "stop messaging me so late at night",
		"the doctor is monitoring my blood pressure", "we should merge our two book clubs",
		"a branch fell off the oak tree in the storm", "doing two things at once is hard concurrency for me",
	}
}

// TestSemanticOffTopicBatteryNoFalsePositive [M5 acceptance #2 — PERMANENT
// REGRESSION GUARD]: a 45-query OFF-TOPIC battery must inject ZERO rules at the
// default 0.60 floor against the concept-rich realisticVault — the vault where
// the M5 concept-expansion regression actually manifested (the single-rule pg
// vault has no colliding concepts). The original M5 SKIPPED out-of-vocabulary
// query words from the relevance denominator, so an off-topic multi-word query
// whose only in-vocab word was an incidental concept-value (e.g. "learn a new
// LANGUAGE" → a python rule) scored rel=1.0 and false-injected. Counting OOV
// content words in the denominator drops such a query well below the floor. This
// test pins the moat shut so the regression can never silently return.
func TestSemanticOffTopicBatteryNoFalsePositive(t *testing.T) {
	battery := offTopicBattery45()
	if len(battery) < 45 {
		t.Fatalf("off-topic battery must have >=45 queries, got %d", len(battery))
	}
	ix := BuildIndex(realisticVault())
	var fps []string
	for _, q := range battery {
		res, err := ix.MatchAndGuard(q, GuardConfig{})
		if err == nil && len(res.Kept) > 0 {
			ids := make([]string, 0, len(res.Kept))
			for _, r := range res.Kept {
				ids = append(ids, r.ID)
			}
			fps = append(fps, q+" -> "+strings.Join(ids, ","))
		}
	}
	if len(fps) != 0 {
		t.Fatalf("off-topic battery injected %d/%d rule-sets at the 0.60 floor (want 0); false positives:\n  %s",
			len(fps), len(battery), strings.Join(fps, "\n  "))
	}

	// And the SAME battery against the single-rule pg vault stays zero too (it was
	// already zero pre-fix, but assert it never regresses either).
	pg := m5PostgresVault()
	for _, q := range battery {
		if res, err := pg.MatchAndGuard(q, GuardConfig{}); err == nil && len(res.Kept) > 0 {
			t.Fatalf("off-topic query %q must inject nothing against the pg vault; kept=%+v", q, res.Kept)
		}
	}

	// The semantic TARGET must STILL fire in the very same realistic vault, so the
	// no-false-positive guard is not achieved by simply floor-ing everything.
	if res, err := ix.MatchAndGuard("what database should I use for my app?", GuardConfig{}); err != nil || len(res.Kept) == 0 || res.Kept[0].ID != "pg" {
		t.Fatalf("the semantic target must still fire the pg rule in the realistic vault; err=%v kept=%+v", err, res.Kept)
	}
}

// TestSemanticOOVDenominatorMechanism pins the ROOT-CAUSE fix precisely: the
// relevance denominator counts a query's OUT-OF-VOCABULARY content words instead
// of silently skipping them. It contrasts two queries that BOTH cover exactly one
// concept value ("language") to demonstrate the mechanism is the OOV COUNT, not a
// per-word blacklist:
//   - "language" ALONE (a genuinely-short, on-topic-shaped query, no OOV content)
//     covers the python rule and fires — the fix must NOT over-floor a short query.
//   - "i want to learn a new language" (the same concept value, but with the OOV
//     content words "learn"+"new") dilutes to rel below the floor and is dropped.
//
// If the fix were a word blacklist on "language" the first case would also floor;
// this proves it is the denominator, preserving genuine short matches.
func TestSemanticOOVDenominatorMechanism(t *testing.T) {
	ix := BuildIndex(realisticVault())

	// Genuinely short, on-topic: bare concept value, zero OOV content words ⇒ fires.
	res, err := ix.MatchAndGuard("language", GuardConfig{})
	if err != nil || len(res.Kept) == 0 {
		t.Fatalf("a bare on-topic concept value must still fire (no over-flooring of short queries); err=%v kept=%+v", err, res.Kept)
	}

	// Same concept value, but embedded in an off-topic sentence with OOV content
	// words ⇒ diluted below the floor ⇒ ZERO. This is the regression the fix closes.
	res2, err2 := ix.MatchAndGuard("i want to learn a new language", GuardConfig{})
	if err2 != ErrNoMatch || len(res2.Kept) > 0 {
		t.Fatalf("an off-topic sentence whose only in-vocab word is a concept value must inject nothing; err=%v kept=%+v", err2, res2.Kept)
	}
}
