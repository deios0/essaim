package server

import (
	"strings"
	"testing"
)

// P1: a huge last-user message must not blow the intercept budget. The match
// query is capped to head+tail so a distinctive term at EITHER end survives (the
// moat stays ON), only the bulk middle is dropped. The cap slices on byte/rune
// boundaries without materializing the whole string.
func TestCapMatchQueryKeepsBothEnds(t *testing.T) {
	filler := strings.Repeat("filler words and pasted context lines\n", 100000) // ~3.7MB
	q := "REMEMBER the postgres rule up front. " + filler + " so, which mysql thing at the end?"
	capped := capMatchQuery(q)
	if len(capped) > maxMatchQueryBytes+1 {
		t.Fatalf("capped query = %d bytes, want <= %d", len(capped), maxMatchQueryBytes+1)
	}
	if !strings.Contains(capped, "postgres") {
		t.Fatalf("cap must keep the HEAD (initial instruction); 'postgres' not found")
	}
	if !strings.Contains(capped, "mysql") {
		t.Fatalf("cap must keep the TAIL (final question); 'mysql' not found")
	}
	if len(capped) >= len(q) {
		t.Fatalf("cap must shrink a huge query (bulk middle dropped)")
	}
	// Short queries pass through untouched.
	if got := capMatchQuery("hi"); got != "hi" {
		t.Fatalf("short query must pass through; got %q", got)
	}
	// Multibyte safety: a query of Cyrillic just over the byte budget must slice on
	// rune boundaries (valid UTF-8 out, no replacement chars from a mid-rune cut).
	big := strings.Repeat("привет ", maxMatchQueryBytes) // >> budget, 2-byte runes
	if c := capMatchQuery(big); !utf8ValidNoRepl(c) {
		t.Fatalf("cap produced invalid/mid-rune UTF-8")
	}
}

func utf8ValidNoRepl(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
