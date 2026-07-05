package rules

import "testing"

// P1: typographic punctuation must SPLIT tokens, not glue onto words. A query
// using smart quotes / em-dash around a distinctive term must still cover the
// rule (the old ch>=0x80 rule made "“postgres”" one OOV token → zero injection).
func TestUnicodePunctuationSplitsTokens(t *testing.T) {
	ix := pgVault()
	// Smart-quoted + em-dashed on-topic query.
	q := "which “database” should I use—postgres or mysql?"
	res, err := ix.MatchAndGuard(q, GuardConfig{})
	if err != nil || len(res.Kept) == 0 || res.Kept[0].ID != "use-postgres" {
		t.Fatalf("smart-quoted on-topic query must still inject the pg rule; err=%v kept=%+v", err, res.Kept)
	}
	// Cyrillic letters must still be kept as word runes (not split).
	toks := tokenize("привет мир")
	var hasCyr bool
	for _, tk := range toks {
		if tk == "привет" || tk == "мир" {
			hasCyr = true
		}
	}
	if !hasCyr {
		t.Fatalf("Cyrillic words must survive tokenization; got %v", toks)
	}
	// A curly quote alone must NOT become part of a token.
	for _, tk := range tokenize("“x”") {
		if tk[0] != '_' && tk != "x" {
			t.Fatalf("punctuation glued onto token: %q", tk)
		}
	}
}
