package rules

import (
	"strings"
	"testing"
)

// F-A [P1 — PRODUCT KILL-RISK]: the similarity floor must actually filter
// irrelevant rules. The bug: Match() normalized every top candidate to score
// 1.0 (out[i].score /= best), so MatchFloor (0.60) NEVER cut — any rule sharing
// one trigram with the query was injected. THE moat depends on injecting ONLY
// relevant rules.
//
// These tests go THROUGH the real Match()/MatchAndGuard() path (not BloatGuard
// with hand-built scored{} candidates), so they exercise the actual floor.

// pgVault builds an index whose ONLY rule is bodied about PostgreSQL.
func pgVault() *Index {
	return BuildIndex([]Rule{{
		ID:         "use-postgres",
		Title:      "Use Postgres",
		Body:       "Always use PostgreSQL as the database, never MySQL.",
		Kind:       "guardrail",
		Weight:     0.95,
		Confidence: 0.95,
	}})
}

// F-A (a): an irrelevant query injects ZERO rules. A vault with one Postgres
// rule, queried "what's the weather today?", must NOT inject the Postgres rule.
// This is the product kill-risk: irrelevant rules in ~every request.
func TestFloorRejectsIrrelevantQuery(t *testing.T) {
	ix := pgVault()
	// Default floor (0.60). Go through the full match→guard path.
	res, err := ix.MatchAndGuard("what's the weather today?", GuardConfig{})
	if err == nil && len(res.Kept) > 0 {
		t.Fatalf("irrelevant query injected %d rule(s) (kill-risk); want ZERO. kept=%+v", len(res.Kept), res.Kept)
	}
	if err != ErrNoMatch {
		t.Fatalf("want ErrNoMatch for an irrelevant query, got err=%v kept=%+v", err, res.Kept)
	}
}

// F-A (b): a relevant query DOES inject the rule. The same Postgres vault,
// queried "which database should I use?", must inject the Postgres rule (the
// floor must not be so aggressive it kills genuine relevance).
func TestFloorAcceptsRelevantQuery(t *testing.T) {
	ix := pgVault()
	res, err := ix.MatchAndGuard("which database should I use?", GuardConfig{})
	if err != nil {
		t.Fatalf("relevant query must inject the rule, got err=%v", err)
	}
	if len(res.Kept) != 1 || res.Kept[0].ID != "use-postgres" {
		t.Fatalf("relevant query must inject the Postgres rule; kept=%+v", res.Kept)
	}
}

// F-A (replaces Test-36 via Match): the floor is reachable through Match(). A
// query that shares only an incidental trigram with the single rule must be
// floored out. This exercises Match()'s relevance score directly, not a
// hand-built scored{} bypass.
func TestFloorReachableThroughMatch(t *testing.T) {
	ix := pgVault()
	// "weather" shares no meaningful term with the Postgres rule. Even if a
	// stray trigram overlaps, the relevance floor must reject it.
	cands := ix.Match("weather forecast sunshine umbrella")
	res, err := BloatGuard(cands, GuardConfig{})
	if err != ErrNoMatch || len(res.Kept) > 0 {
		t.Fatalf("Match()-derived candidates for an irrelevant query must floor to ErrNoMatch; got err=%v kept=%+v", err, res.Kept)
	}
}

// Guards the moat at the multi-rule scale: a clearly-relevant query in a vault
// of mixed rules injects only the topically-matching rule(s), and an irrelevant
// query injects none — proving the floor cuts even when normalization makes the
// top candidate score 1.0.
func TestFloorMultiRuleMoat(t *testing.T) {
	ix := BuildIndex([]Rule{
		{ID: "pg", Title: "Use Postgres", Body: "Always use PostgreSQL as the database engine.", Weight: 0.9, Confidence: 0.9},
		{ID: "tabs", Title: "Use tabs", Body: "Indent source code with tabs, never spaces.", Weight: 0.8, Confidence: 0.8},
		{ID: "py", Title: "Python style", Body: "Use black formatting for Python files.", Weight: 0.7, Confidence: 0.7},
	})
	// Irrelevant: cooking. None of the three rules is about cooking.
	res, err := ix.MatchAndGuard("how long should I boil an egg?", GuardConfig{})
	if err == nil && len(res.Kept) > 0 {
		t.Fatalf("cooking query must inject nothing; kept=%+v", res.Kept)
	}
	// Relevant: database. Only the pg rule should clear the floor.
	res2, err2 := ix.MatchAndGuard("should I pick postgres or another database?", GuardConfig{})
	if err2 != nil {
		t.Fatalf("database query must inject; err=%v", err2)
	}
	got := make([]string, 0, len(res2.Kept))
	for _, r := range res2.Kept {
		got = append(got, r.ID)
	}
	if len(got) == 0 || got[0] != "pg" {
		t.Fatalf("database query must inject the pg rule first; got %v", strings.Join(got, ","))
	}
}

// P1-c [FLOOR FALSE-INJECT on a rule-unique IMPERATIVE word]: relStopwords listed
// generic verbs (use/get/pick) but EXCLUDED the imperative/directive class
// (always/never/avoid/prefer/remember/ensure/…) — exactly the words that FILL rule
// bodies. So an unrelated query whose only in-vocab word is an imperative ("always
// remember to call mom" vs an "Always use PostgreSQL…" rule) had df("always")=1,
// rel=1.0 ≥ floor → the Postgres rule injected into a query about phoning mom.
//
// After adding the imperative class to relStopwords, "always" carries no topical
// signal, the query covers no distinctive word ⇒ rel below floor ⇒ NO injection —
// against BOTH a 1-rule and a 3-rule Postgres vault. A genuinely DB-relevant query
// must still inject.
func TestFloorRejectsRuleUniqueImperativeWord(t *testing.T) {
	// 1-rule vault: the imperative "always" appears in the rule body.
	one := pgVault()
	res, err := one.MatchAndGuard("always remember to call mom", GuardConfig{})
	if err != ErrNoMatch || len(res.Kept) > 0 {
		t.Fatalf("1-rule vault: an imperative-only query must inject NOTHING; got err=%v kept=%+v", err, res.Kept)
	}

	// 3-rule vault (TestFloorMultiRuleMoat shape): the same imperative-only query
	// must still floor to nothing even with more rules to (falsely) match.
	three := BuildIndex([]Rule{
		{ID: "pg", Title: "Use Postgres", Body: "Always use PostgreSQL as the database engine.", Weight: 0.9, Confidence: 0.9},
		{ID: "tabs", Title: "Use tabs", Body: "Always indent source code with tabs, never spaces.", Weight: 0.8, Confidence: 0.8},
		{ID: "py", Title: "Python style", Body: "Always prefer black formatting; never use autopep8.", Weight: 0.7, Confidence: 0.7},
	})
	res3, err3 := three.MatchAndGuard("always remember to call mom", GuardConfig{})
	if err3 != ErrNoMatch || len(res3.Kept) > 0 {
		t.Fatalf("3-rule vault: an imperative-only query must inject NOTHING; got err=%v kept=%+v", err3, res3.Kept)
	}

	// Control: a genuinely relevant DB query (which ALSO opens with an imperative)
	// must still inject — the imperative stopwording must not kill real relevance.
	resOK, errOK := three.MatchAndGuard("always use the right database: postgres or mysql?", GuardConfig{})
	if errOK != nil {
		t.Fatalf("a genuinely relevant DB query must inject; err=%v", errOK)
	}
	if len(resOK.Kept) == 0 || resOK.Kept[0].ID != "pg" {
		gotIDs := make([]string, 0, len(resOK.Kept))
		for _, r := range resOK.Kept {
			gotIDs = append(gotIDs, r.ID)
		}
		t.Fatalf("relevant DB query must inject the pg rule first; got %v", strings.Join(gotIDs, ","))
	}
}
