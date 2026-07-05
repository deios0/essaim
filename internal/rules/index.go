package rules

import (
	"math"
	"sort"
	"strings"
	"sync/atomic"
	"unicode"
)

// Index is an immutable, in-memory lexical index over a vault's rules. It is
// built off the request path and published via an atomic Pointer (build-then-
// swap); the hot path only ever does a Load() + a flat scan over precomputed
// term frequencies — no locks, no disk (spec §5.2).
//
// The match is a pure-Go BM25 over a trigram-augmented token stream (T0),
// augmented at build time with M5 SEMANTIC relevance: each rule's token bag is
// expanded with the curated CONCEPT terms its words imply (concepts.go), so a
// query using a general concept ("database") covers a rule that only names a
// specific instance ("postgresql") — no shared word required, no CGO, no ONNX,
// no model download. The dense-vector / ONNX path was rejected (see ADR
// 2026-06-27-m5-semantic-concept-expansion) for the purity reasons.
//
// The no-false-positive property is enforced by the RELEVANCE FLOOR below (the
// `rel` ratio that counts a query's uncovered content words — see `Match`), NOT
// by the concept table: the table adds common polysemous words to the vocabulary,
// so an off-topic query CAN incidentally hit one. The original "false-positive-free
// by construction" claim was wrong (a 45-query battery proved 11 FPs); the floor's
// OOV-content-word denominator is the fix. See the ADR's "Correction" section.
type Index struct {
	rules []Rule

	// Per-document term frequencies (token → count), parallel to rules.
	docTF []map[string]int
	// Document length in tokens, parallel to rules.
	docLen []int
	// Document frequency: token → number of docs containing it.
	df map[string]int
	// Average document length, for BM25 length normalization.
	avgLen float64
	// idfMax is the largest possible content-word IDF in this index — the IDF of a
	// term that appears in exactly one rule (df==1, the most distinctive a real
	// corpus word can be). It is the per-word penalty SCALE charged to an
	// out-of-vocabulary (df==0) query word in the relevance denominator (M5-FIX):
	// an OOV word is unaccounted content, so it is treated as "one more distinctive
	// content word the query carries that NO rule covers." Precomputed at build
	// time so the request path never recomputes it. Zero for an empty index.
	idfMax float64
}

// BM25 parameters (Robertson/Sparck-Jones defaults).
const (
	bm25K1 = 1.5
	bm25B  = 0.75
)

// relOOVPenalty is the per-out-of-vocabulary-content-word penalty charged into
// the relevance denominator (M5-FIX), as a fraction of idfMax (the IDF of a
// df==1 term — the most distinctive a real corpus word can be). It makes an
// off-topic multi-word query whose only in-vocab hit is an incidental
// concept-value clear LESS than the 0.60 floor, while a genuinely topical query
// (distinctive covered term + at most one trailing generic word) still clears it.
//
// BOUND PROOF (against the default 0.60 floor; rel = hitIDF/(qIDFTotal + β·n·idfMax),
// β = relOOVPenalty, n = #OOV content words). For a query with ONE covered
// distinctive concept value (idf ≈ idfMax) the ratio is 1/(1 + β·n):
//   - n=1 (semantic target "what database should I use for my APP?"): 1/(1+β) ≥ 0.60
//     ⟺ β ≤ 0.667. The target MUST clear ⇒ upper bound β ≤ 0.667.
//   - n=2 ("learn a new LANGUAGE": learn,new OOV): 1/(1+2β) < 0.60 ⟺ β > 0.333.
//   - the literal-word FP "commit to a RELATIONSHIP" (covered idf=1.281 < idfMax
//     in the realistic vault, n=1): 1.281/(1.281 + 1.792β) < 0.60 ⟺ β > 0.477.
//
// β = 0.55 sits inside (0.477, 0.667]: target fires, every 2+-OOV off-topic query
// AND the weaker-than-max literal-word FP floor out. The window is wide enough
// that the value is robust, not a hair-trigger tuned to one battery.
const relOOVPenalty = 0.55

// relStopwords are high-frequency function words that carry ~no topical signal.
// They are EXCLUDED from the absolute relevance score (rel) only — never from
// BM25 ranking. Without this, an irrelevant query whose only in-vocabulary word
// is a stopword ("what's the weather" sharing "the" with a Postgres rule body)
// scores rel=1.0 and defeats the floor (F-A). With it, such a query has no
// meaningful covered word ⇒ rel=0 ⇒ floored. A query that genuinely covers a
// distinctive term ("database", "postgres") is unaffected. ASCII, lowercase;
// pure and locale-independent.
var relStopwords = map[string]bool{
	"a": true, "an": true, "and": true, "the": true, "is": true, "are": true,
	"was": true, "were": true, "be": true, "been": true, "to": true, "of": true,
	"in": true, "on": true, "at": true, "for": true, "with": true, "as": true,
	"by": true, "or": true, "but": true, "if": true, "then": true, "else": true,
	"i": true, "me": true, "my": true, "we": true, "us": true, "you": true,
	"it": true, "its": true, "this": true, "that": true, "these": true,
	"those": true, "what": true, "which": true, "who": true, "how": true,
	"when": true, "where": true, "why": true, "do": true, "does": true,
	"did": true, "can": true, "could": true, "should": true, "would": true,
	"will": true, "shall": true, "may": true, "might": true, "must": true,
	"use": true, "using": true, "used": true, "s": true, "t": true, "not": true,
	"no": true, "yes": true, "today": true, "now": true, "here": true,
	"there": true, "some": true, "any": true, "another": true, "other": true,
	"pick": true, "choose": true, "get": true, "got": true, "have": true, "has": true,
	"had": true, "about": true, "from": true, "into": true, "over": true,
	"out": true, "up": true, "down": true, "so": true, "than": true,
	"too": true, "very": true, "just": true, "more": true, "most": true,
	"long": true, "much": true, "many": true, "good": true, "bad": true,
	// IMPERATIVE / GENERIC-DIRECTIVE class (P1-c). These are the words that FILL
	// rule bodies ("Always use X", "Never do Y", "Prefer Z", "Remember to …"), so
	// they carry directive force but ZERO topical signal. Without excluding them, a
	// query whose only in-vocab word is an imperative ("always remember to call
	// mom" vs an "Always use PostgreSQL…" rule) had df=1 ⇒ rel=1.0 ≥ floor ⇒ the
	// rule false-injected into an unrelated query. Excluding them from `rel` (NOT
	// from BM25 ranking) means such a query covers no distinctive term ⇒ rel below
	// floor ⇒ no injection; a genuinely topical query is unaffected.
	"always": true, "never": true, "avoid": true, "prefer": true,
	"preferred": true, "remember": true, "ensure": true, "make": true,
	"keep": true, "call": true, "don": true, "dont": true, "let": true,
	"need": true, "needs": true, "want": true, "wants": true, "try": true,
	"please": true, "also": true, "only": true, "every": true, "each": true,
	"all": true, "both": true, "either": true, "neither": true,
}

// BuildIndex constructs an immutable Index from the loaded rules. It is
// deterministic and allocates everything it needs up front so the request path
// never allocates index structures.
func BuildIndex(rs []Rule) *Index {
	ix := &Index{
		rules:  rs,
		docTF:  make([]map[string]int, len(rs)),
		docLen: make([]int, len(rs)),
		df:     make(map[string]int, len(rs)*8),
	}
	var total int
	for i, r := range rs {
		text := r.Title + " " + r.Body
		toks := tokenize(text)

		// M5 SEMANTIC AUGMENT (index-time precompute — the <15ms constraint): add
		// the curated CONCEPT terms implied by this rule's words (e.g. a rule that
		// mentions "postgresql"/"mysql" also indexes "database"/"sql"/"rdbms"). A
		// concept word is indexed exactly like a body word — its bare token feeds
		// `rel` coverage so a query using the GENERAL concept covers this SPECIFIC
		// rule, and its trigrams feed BM25 ranking — so a semantic match BECOMES a
		// lexical match and inherits the floor machinery. All of this happens here at
		// build time; the request path is unchanged. NOTE: the added concept VALUES
		// are common words ("language", "style", "container", …) that DO enter the
		// vocabulary, so an off-topic query can incidentally hit one — the no-FP
		// guarantee is the `rel` floor's OOV-content-word denominator in Match, NOT
		// this augment. A pure-noise query ("weather") still covers nothing ⇒ floored.
		for _, c := range conceptAugment(baseWords(text)) {
			toks = append(toks, c) // bare concept word → counts toward rel coverage
			for _, tg := range trigrams(c) {
				toks = append(toks, "_"+tg) // concept trigrams → BM25 ranking only
			}
		}

		tf := make(map[string]int, len(toks))
		for _, t := range toks {
			tf[t]++
		}
		ix.docTF[i] = tf
		ix.docLen[i] = len(toks)
		total += len(toks)
		for t := range tf {
			ix.df[t]++
		}
	}
	if len(rs) > 0 {
		ix.avgLen = float64(total) / float64(len(rs))
	}
	// idfMax = IDF of a df==1 term (the most distinctive a corpus word can be),
	// using the SAME BM25 IDF formula the matcher uses. It is the per-OOV-word
	// penalty scale (M5-FIX). Computed once here, off the request path. With one
	// rule this is small (the IDF scale is naturally compressed), which is correct:
	// the penalty self-scales to the vault's distinctiveness range.
	if N := float64(len(rs)); N > 0 {
		ix.idfMax = math.Log(1 + (N-1+0.5)/(1+0.5))
	}
	return ix
}

// Len reports the number of indexed rules. Zero ⇒ ErrIndexEmpty upstream.
func (ix *Index) Len() int {
	if ix == nil {
		return 0
	}
	return len(ix.rules)
}

// scored is a rule with its match scores, used internally by the matcher.
//
// score is the BM25 score normalized to [0,1] by dividing by the best
// candidate's score — it is for RANKING only (so the top result is always 1.0).
//
// rel is an ABSOLUTE relevance score in [0,1], independent of the other
// candidates: the fraction of distinct query WORDS (not trigrams) the rule's
// indexed text actually contains. It is what the similarity floor (§5.3.c) cuts
// on — the normalized `score` cannot be used for the floor because it pins the
// top candidate to 1.0 no matter how irrelevant, so the floor never fires (F-A:
// a "Postgres" rule was injected for "what's the weather"). A rule that shares
// only an incidental trigram with the query has rel≈0 and is floored out; a rule
// whose body genuinely covers the query terms has rel near 1 and is kept.
type scored struct {
	rule  Rule
	score float64
	rel   float64
	// relSet distinguishes a computed rel of 0 (genuinely irrelevant, from
	// Match()) from an unset rel (a hand-built candidate in a unit test that only
	// populated score). The floor cuts on rel only when relSet; otherwise it
	// falls back to score, preserving the BloatGuard unit tests that predate F-A.
	relSet bool
}

// Match runs a BM25 query over the last user message and returns rules scored
// above zero. It does NOT apply the bloat guard — that is BloatGuard's job (spec
// §5.3).
//
// Two scores are produced per candidate:
//   - score: BM25, normalized to [0,1] by dividing by the best score. RANKING
//     only — the top result is always 1.0.
//   - rel:   ABSOLUTE query-WORD coverage in [0,1] (fraction of distinct query
//     words the rule's text contains). The similarity floor (§5.3.c) cuts on
//     THIS, so a genuinely-irrelevant rule (only an incidental trigram overlaps)
//     is rejected and an on-topic rule is kept (F-A). Trigrams still feed BM25
//     ranking but never inflate rel, so a stray trigram can't lift an irrelevant
//     rule over the floor.
//
// Results are sorted by score DESC then id ASC (stable, deterministic).
func (ix *Index) Match(query string) []scored {
	if ix == nil || len(ix.rules) == 0 {
		return nil
	}
	qToks := tokenize(query)
	if len(qToks) == 0 {
		return nil
	}
	// Deduplicate query terms (BM25 is over the set of query terms).
	seen := make(map[string]bool, len(qToks))
	var terms []string
	for _, t := range qToks {
		if !seen[t] {
			seen[t] = true
			terms = append(terms, t)
		}
	}
	N := float64(len(ix.rules))

	// Distinct query WORDS (not trigrams) that EXIST in the corpus vocabulary —
	// the basis for the absolute relevance score. A trigram token is prefixed
	// with "_" (see tokenize); a word never is. Each in-vocab word is IDF-weighted
	// so a rare, distinctive term (e.g. "postgres", "database") dominates relevance
	// and common filler ("which", "should", "the") contributes little.
	//
	// M5-FIX — OOV content words COUNT IN THE DENOMINATOR. The original M5 code
	// SKIPPED query words with df==0 (out of vocabulary) entirely. That was the
	// concept-expansion false-positive: M5 added concept VALUES ("language", "ui",
	// "style", "code", "container", "web", "data", …) — common polysemous English
	// words — to the corpus vocabulary, so an off-topic multi-word query whose ONLY
	// in-vocab word is an incidental concept hit ("I want to learn a new LANGUAGE")
	// had its OOV words ("learn", "new") silently dropped from the denominator ⇒
	// rel = 1.0 ⇒ false inject. Counting OOV content words at a per-word penalty
	// (idfMax, the most distinctive a real corpus word can be — the harshest fair
	// charge) makes such a query yield rel = covered / (covered + Σ penalty) ≪ 1.0,
	// below the floor, WITHOUT a tuned probabilistic threshold: a multi-word
	// off-topic query with one incidental hit can never reach the floor, while a
	// genuinely-topical query (the distinctive covered term dominates the IDF mass,
	// few or no uncovered content words) still clears it. This ALSO kills the
	// pre-existing literal-word FPs (e.g. "commit to a relationship" → git): the
	// single incidental cover is diluted by the query's real, uncovered content.
	type qword struct {
		tok string
		idf float64
	}
	var qWords []qword
	var qIDFTotal float64 // Σ IDF over in-vocab content words (the covered-or-not pool)
	var oovPenalty float64
	for _, t := range terms {
		if strings.HasPrefix(t, "_") {
			continue // trigram — feeds BM25 ranking, never the relevance floor
		}
		if relStopwords[t] {
			continue // function word — no topical signal, excluded from rel
		}
		df := float64(ix.df[t])
		if df == 0 {
			// Out-of-vocabulary content word: real topical content the query carries
			// that NO rule covers. It is NOT silently dropped (the M5 bug); it charges
			// a per-word penalty into the denominator so an incidental single cover on
			// an otherwise off-topic query can't reach rel=1.0. The penalty is idfMax
			// (the IDF of a df==1 term) scaled by relOOVPenalty — see relOOVPenalty for
			// the bound proof that this keeps the semantic target firing.
			oovPenalty += relOOVPenalty * ix.idfMax
			continue
		}
		idf := math.Log(1 + (N-df+0.5)/(df+0.5))
		qWords = append(qWords, qword{tok: t, idf: idf})
		qIDFTotal += idf
	}
	// The relevance denominator: in-vocab content IDF mass PLUS the OOV penalty.
	// A query with no content words at all (only stopwords/trigrams) has a zero
	// denominator ⇒ rel stays 0 ⇒ floored (unchanged from before).
	relDenom := qIDFTotal + oovPenalty
	out := make([]scored, 0, len(ix.rules))
	var best float64
	for i, r := range ix.rules {
		var s float64
		tf := ix.docTF[i]
		dl := float64(ix.docLen[i])
		for _, t := range terms {
			f := float64(tf[t])
			if f == 0 {
				continue
			}
			df := float64(ix.df[t])
			// BM25 IDF with the standard +1 smoothing (always positive).
			idf := math.Log(1 + (N-df+0.5)/(df+0.5))
			norm := f * (bm25K1 + 1) / (f + bm25K1*(1-bm25B+bm25B*dl/ix.avgLen))
			s += idf * norm
		}
		if s > 0 {
			// Absolute relevance = IDF-weighted fraction of in-vocabulary query
			// words present in THIS rule's indexed text. Independent of other
			// candidates and of trigram overlap, so the floor cuts on real topical
			// match: a distinctive word ("postgres") covered ⇒ high rel; only
			// filler covered ⇒ low rel; nothing covered ⇒ rel 0.
			var hitIDF float64
			for _, w := range qWords {
				if tf[w.tok] > 0 {
					hitIDF += w.idf
				}
			}
			var rel float64
			if relDenom > 0 {
				rel = hitIDF / relDenom
			}
			out = append(out, scored{rule: r, score: s, rel: rel, relSet: true})
			if s > best {
				best = s
			}
		}
	}
	if best > 0 {
		for i := range out {
			out[i].score /= best
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].score != out[b].score {
			return out[a].score > out[b].score
		}
		return out[a].rule.ID < out[b].rule.ID
	})
	return out
}

// tokenize lowercases, splits on non-alphanumeric runes, and augments each word
// with its character trigrams. The trigram augmentation makes the match robust
// to morphology and partial overlaps (e.g. "postgres" vs "postgresql") without
// a stemmer, and is locale-independent (operates on runes). Pure, deterministic.
func tokenize(s string) []string {
	s = strings.ToLower(s)
	var words []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, cur.String())
			cur.Reset()
		}
	}
	for _, ch := range s {
		if isWordRune(ch) {
			cur.WriteRune(ch)
		} else {
			flush()
		}
	}
	flush()

	out := make([]string, 0, len(words)*3)
	for _, w := range words {
		out = append(out, w)
		for _, tg := range trigrams(w) {
			out = append(out, "_"+tg) // prefix so trigrams never collide with words
		}
	}
	return out
}

func isWordRune(ch rune) bool {
	if ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch >= 'A' && ch <= 'Z' {
		return true
	}
	// Non-ASCII: keep Unicode LETTERS, DIGITS, and COMBINING MARKS (Cyrillic,
	// accented Latin, CJK, and NFD-decomposed text — a base letter followed by a
	// combining mark, common on macOS, e.g. "niño" = n i n ◌̃ o) but NOT Unicode
	// punctuation. The old `ch >= 0x80` glued typographic punctuation — curly
	// quotes, em/en dashes, guillemets, ellipsis — onto the adjacent word, turning
	// a genuine on-topic term into a penalized out-of-vocabulary token, so a query
	// using smart quotes injected nothing (P1). Including IsMark keeps combining
	// marks attached to their base letter (gemini review) so decomposed words are
	// not split; punctuation still splits.
	return ch >= 0x80 && (unicode.IsLetter(ch) || unicode.IsDigit(ch) || unicode.IsMark(ch))
}

// trigrams returns the character trigrams of a word. Words shorter than 3 runes
// yield no trigrams (the whole word already carries the signal).
func trigrams(w string) []string {
	rs := []rune(w)
	if len(rs) < 3 {
		return nil
	}
	out := make([]string, 0, len(rs)-2)
	for i := 0; i+3 <= len(rs); i++ {
		out = append(out, string(rs[i:i+3]))
	}
	return out
}

// Pointer is the atomic, build-then-swap publication seam: the request path
// reads the current immutable Index via Load(); a rebuild Stores a fresh one.
// A nil Index Load is valid (no vault loaded yet) and means "no rules".
type Pointer struct {
	p atomic.Pointer[Index]
}

// Store publishes a new immutable index. Safe for concurrent use with Load.
func (sp *Pointer) Store(ix *Index) { sp.p.Store(ix) }

// Load returns the current immutable index (may be nil before the first Store).
func (sp *Pointer) Load() *Index { return sp.p.Load() }
