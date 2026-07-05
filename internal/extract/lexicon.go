// Package extract implements the M3 layered learning loop: T0 sigil
// (deterministic), T1 heuristic (zero-token, ported from the reference engine's
// lexicon + quality scorer), and T2 cheap async LLM gate (opt-in, default OFF).
// It consumes a captured exchange (the pre-injection messages + the assistant
// text), classifies it false-positive-averse, and writes a `draft` rule to
// _inbox/ (or, on a sigil, an `active` rule to remembered/<date>/). A local
// store NEVER persists a credential — the credentialPattern hard-rejects on
// every path (sigil scan, capture redaction, emitter refusal).
//
// All of this runs OFF the request hot path (locked invariant 2): the capture
// tap is observation-only and never alters the verbatim client stream.
package extract

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode/utf8"
)

// credentialPattern is the byte-for-byte port of the reference engine
// CREDENTIAL_PATTERN, RE2-safe (no backrefs; (?i) is fine over
// the JWT alternation). ONE pattern, THREE readers — the sigil scan (§2.3), the
// capture redaction (§4.7), and the NativeFileEmitter refusal (§5.4) all share
// it. A local store NEVER persists a key, even one the user explicitly typed
// after /remember.
var credentialPattern = regexp.MustCompile(`(?i)(?:` +
	`sk-[A-Za-z0-9_-]{16,}` +
	`|(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{16,}` +
	`|whsec_[A-Za-z0-9]{16,}` +
	`|AKIA[0-9A-Z]{16}` +
	`|gh[pousr]_[A-Za-z0-9]{20,}` +
	`|github_pat_[A-Za-z0-9_]{20,}` +
	`|glpat-[A-Za-z0-9_-]{18,}` + // GitLab personal access token (P1-5)
	`|xox[baprs]-[A-Za-z0-9-]{10,}` +
	`|AIza[0-9A-Za-z_-]{35}` +
	// Google OAuth ACCESS token (P2-4): the distinctive `ya29.` prefix + a long
	// base64url body. The {20,} floor rejects prose like `ya29.1` / `figure
	// ya29.1` (short, numeric) while matching a real access token (hundreds of
	// chars). Anchored on the prefix, so `ya29` without the dot never matches.
	`|ya29\.[0-9A-Za-z_-]{20,}` +
	// Google REFRESH token (P2-4): the `1//` prefix + a long base64url body. The
	// {20,} floor is what keeps a fraction like `1//2`, `1//3`, or a path fragment
	// `1//D2` from matching — a real refresh token is a long high-entropy run.
	`|1//[0-9A-Za-z_-]{20,}` +
	`|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}` +
	// PEM/PGP private-key block (P1-b): match the WHOLE block — header + base64
	// BODY + END footer — not just the single BEGIN header line. The prior
	// header-only pattern left the key body + END line behind on redaction, so the
	// redacted text no longer matched and dodged the hard-reject (the leak). The
	// `[\s\S]*?` is the non-greedy "any char incl. newline" span (RE2 has no `s`
	// flag / no `.` newline-dot; `[\s\S]` is the RE2-safe equivalent). `[^-]*`
	// after BEGIN/END covers OPENSSH/RSA/EC/DSA and the PGP form `PRIVATE KEY
	// BLOCK` (the word BLOCK after KEY). The non-greedy body stops at the FIRST
	// END marker, so two adjacent keys each match. A lone BEGIN with no matching
	// END is still caught by the header alternative below (defense in depth).
	`|-----BEGIN[^-]*PRIVATE KEY[^-]*-----[\s\S]*?-----END[^-]*PRIVATE KEY[^-]*-----` +
	// Lone/truncated PEM header (no END present, e.g. a clipped paste): the header
	// alone is still a credential marker → match + hard-reject.
	`|-----BEGIN[^-]*PRIVATE KEY[^-]*-----` +
	// URL-embedded credential (P1-5): scheme://user:password@host. Requires the
	// user:pass@ shape, so a plain host:port URL (no '@') never matches.
	`|[a-z][a-z0-9+.-]*://[^/\s:@]+:[^/\s@]+@` +
	// Space-separated bearer token (P1-5): `Bearer <token>` with NO ':'/'=' —
	// the keyed form below requires a separator and misses this shape.
	`|bearer\s+[A-Za-z0-9_\-./+=]{16,}` +
	`|(?:api[_-]?key|password|passwd|token|secret|bearer)\s*[:=]\s*['"]?[A-Za-z0-9_\-./+=]{8,}` +
	`)`)

// base64Run matches a MAXIMAL run of the base64 alphabet [A-Za-z0-9/+]. We scan
// these runs (FindAllStringIndex) and length-gate each to EXACTLY 40 ourselves,
// rather than baking a 40-char boundary into the regex. A boundary-consuming
// regex (`(?:^|[^…])([…]{40})(?:[^…]|$)`) consumes the delimiter BETWEEN two
// adjacent 40-char tokens as part of match #1, so the scanner restarts PAST the
// start of match #2 and silently skips the second secret (review defect D3:
// `aws … SECRET1 SECRET2` redacted only SECRET1). Matching the maximal run and
// gating length per run sees BOTH (and a 41+-char run is one run of length≠40,
// so it is correctly rejected, never clipped to a spurious 40). The EXACT-40
// shape is the SPECIFIC-FORMAT branch the p1d revert sanctions (a known wire
// shape, not a blind length+entropy gate) — see the revert note above and
// docs/decisions/2026-06-24-p1d-privatekeybody-reverted.md. On its own a 40-char
// base64 run is ambiguous (a git SHA, a base64 blob, a path fragment all fit), so
// it is NEVER redacted on shape alone: the readers additionally require (a) the
// run is not pure lowercase hex (excludes git SHAs) and (b) AWS context — a
// word-boundary `aws` AND a tight `secret … access … key` intent phrase within a
// short window of the token.
var base64Run = regexp.MustCompile(`[A-Za-z0-9/+]+`)

// hex40 matches a pure lowercase-hex 40-char string — a git SHA-1. An AWS secret
// access key is base64 and (effectively always) carries a char outside [0-9a-f];
// excluding pure hex keeps a 40-char commit SHA from ever being read as a secret.
var hex40 = regexp.MustCompile(`^[0-9a-f]{40}$`)

// awsContextWindow is how many bytes on each side of a candidate token we scan
// for the AWS context markers. Short by design: the markers must be ADJACENT to
// the token (same clause), not merely somewhere in a long rule, so an unrelated
// 40-char base64 blob in a rule that happens to mention AWS elsewhere is safe.
const awsContextWindow = 48

// awsWordPattern matches the WORD `aws` at a non-alphanumeric boundary — NOT a
// bare substring. The prior bare strings.Contains(win, "aws") fired on "awstats"
// and any token embedding the three letters (review defect D1: a benign "For
// awstats, the cache key <40chars> …" rule was redacted AND hard-rejected). We
// use an explicit `(?:^|[^a-z0-9])aws(?:[^a-z0-9]|$)` boundary rather than RE2
// `\b`: RE2 `\b` treats `_` as a word char, so `\baws\b` would REJECT the very
// common `AWS_SECRET_ACCESS_KEY` form (no boundary between `AWS` and `_`). The
// non-alphanumeric boundary treats `_` (and space/`-`/`=`) as a boundary, so
// `AWS_…` qualifies, while a LETTER or DIGIT adjacent to `aws` (awstats, lawsuit)
// does not. (?i) lowercases; the window is already lowered by hasAWSContext.
var awsWordPattern = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])aws(?:[^a-z0-9]|$)`)

// awsIntentPattern matches the TIGHT AWS secret-key intent phrase
// `secret access key` (the three words in order, separated by a single
// `_`/space/`-`), covering `aws_secret_access_key`, `AWS_SECRET_ACCESS_KEY`,
// `secret access key`, and `secret-access-key`. The prior gate accepted the BARE
// word "key" / "access" / "secret", so "the cache key …", "grant access to …",
// or any rule with an isolated "secret" qualified (review defect D1). Requiring
// the full ordered phrase keeps false positives near zero while still matching
// every real labelling of an AWS secret access key.
var awsIntentPattern = regexp.MustCompile(`(?i)secret[ _-]access[ _-]key`)

// hasAWSContext reports whether the original-text window [lo:hi] carries AWS
// secret-key context: BOTH a word-boundary `aws` AND the tight intent phrase
// `secret access key` (any of `_`/space/`-` between the words). It slices the
// ORIGINAL text (lo/hi clamped to [0,len(text)]) and lowercases ONLY that window
// for the comparison — it must NOT index a separately-lowered full string with
// text-derived indices, because lowercasing can SHRINK byte length (e.g. U+212A
// Kelvin → "k"), making a full-lowered string shorter than text and panicking on
// `lowered[lo:hi]` when lo>len(lowered) (review defect D2, a reachable panic on
// the relay goroutine with no recover).
func hasAWSContext(text string, lo, hi int) bool {
	if lo < 0 {
		lo = 0
	}
	if lo > len(text) {
		lo = len(text)
	}
	if hi > len(text) {
		hi = len(text)
	}
	if hi < lo {
		hi = lo
	}
	win := strings.ToLower(text[lo:hi])
	return awsWordPattern.MatchString(win) && awsIntentPattern.MatchString(win)
}

// awsSecretRun reports whether the base64 run text[ts:te] is an AWS secret access
// key for redaction purposes: EXACTLY 40 chars, not a pure-hex git SHA, and
// adjacent to AWS secret-key context. Shared by both readers so ContainsCredential
// and RedactCredentials never disagree. The context window is derived from the
// ORIGINAL text (clamped, then locally lowered inside hasAWSContext) — never an
// index into a separately-lowered full string (defect D2).
//
// Chain-aware lookbehind (defect D3): two AWS secrets can be listed back-to-back
// (`aws secret access key SECRET1 SECRET2`). The intent phrase sits just before
// SECRET1, ~42 bytes before SECRET2 — outside the short fixed window measured
// from SECRET2 alone. So for the lookbehind we walk PAST any immediately-
// preceding chain of `whitespace + 40-char-base64-token` groups (each itself a
// secret-shaped token) and start the AWS-context window from THERE. This reaches
// the phrase for every token in a contiguous secret list, while a benign 40-char
// blob sitting after unrelated prose (no preceding secret chain) is unaffected —
// the window stays short and FP-safe.
func awsSecretRun(text string, ts, te int) bool {
	if te-ts != 40 {
		return false // not the AWS secret wire shape (exactly 40 base64 chars)
	}
	token := text[ts:te]
	if hex40.MatchString(strings.ToLower(token)) {
		return false // a git SHA, never a secret
	}
	ctxStart := chainStart(text, ts) - awsContextWindow
	return hasAWSContext(text, ctxStart, te+awsContextWindow)
}

// chainStart walks backward from byte offset ts over a contiguous chain of
// `<whitespace><exactly-40-char base64 token>` groups and returns the offset at
// the START of the earliest such token (or ts itself if none precede). Only
// whitespace may separate the tokens — prose, commas, or any other byte stop the
// walk, so the chain stays a tight "list of secrets", not a scan across the rule.
func chainStart(text string, ts int) int {
	pos := ts
	for {
		// Consume preceding ASCII whitespace.
		i := pos
		for i > 0 && isASCIISpace(text[i-1]) {
			i--
		}
		if i == pos || i == 0 {
			return pos // no whitespace gap, or hit start → stop
		}
		// Consume a preceding base64 run; require it be EXACTLY 40 chars to count
		// as a chained secret token.
		j := i
		for j > 0 && isBase64Byte(text[j-1]) {
			j--
		}
		if i-j != 40 {
			return pos // the preceding run is not a 40-char secret token → stop
		}
		pos = j // walked past one chained secret; keep going
	}
}

func isASCIISpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f' || b == '\v'
}

func isBase64Byte(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') ||
		(b >= '0' && b <= '9') || b == '/' || b == '+'
}

// redactAWSSecretKeys replaces every 40-char base64 token that is (a) not a pure
// hex git SHA and (b) surrounded by AWS secret-key context with [REDACTED],
// leaving all other bytes — prose, the markers, non-matching tokens — intact. It
// is a TARGETED, FORMAT-SPECIFIC, CONTEXT-GATED pass: it closes the markerless AWS
// secret gap (review follow-up #1) without re-introducing the reverted blind
// length+entropy heuristic. It redacts only the offending token, never the rule.
//
// It scans MAXIMAL base64 runs (base64Run) and length-gates each run to exactly
// 40, instead of a boundary-consuming 40-char regex. The boundary-consuming form
// ate the delimiter between two adjacent secrets, skipping the second (defect
// D3); scanning runs sees every run independently, so two adjacent secrets BOTH
// redact.
func redactAWSSecretKeys(text string) string {
	var b strings.Builder
	last := 0
	for _, m := range base64Run.FindAllStringIndex(text, -1) {
		ts, te := m[0], m[1]
		if !awsSecretRun(text, ts, te) {
			continue
		}
		b.WriteString(text[last:ts])
		b.WriteString("[REDACTED]")
		last = te
	}
	if last == 0 {
		return text // no redaction → return the original unchanged (no realloc tells)
	}
	b.WriteString(text[last:])
	return b.String()
}

// containsAWSSecretKey reports whether text carries a context-gated AWS secret
// access key (the detector half of redactAWSSecretKeys, sharing the exact same
// run-scan + length gate + hex-exclusion + context gate so ContainsCredential and
// RedactCredentials never disagree).
func containsAWSSecretKey(text string) bool {
	for _, m := range base64Run.FindAllStringIndex(text, -1) {
		if awsSecretRun(text, m[0], m[1]) {
			return true
		}
	}
	return false
}

// pemMarkerPattern matches EITHER a BEGIN or an END private-key marker line
// (P1-b defense in depth). The whole-block credentialPattern removes a complete
// key in one shot, but if an upstream redaction was partial (e.g. it wiped only
// the BEGIN header, leaving an orphan `-----END ... PRIVATE KEY-----`), the
// remaining marker is still a tell that a key body was present. classifyQuality /
// runT0 hard-reject on this so a private-key-bearing exchange is NEVER written to
// a draft, regardless of redaction order.
//
// The (?i) flag (P1-d) makes the marker match case-INSENSITIVELY: some tools
// emit lower/mixed-case armor (`-----begin private key-----`), which is still a
// private-key tell and must trip the backstop.
var pemMarkerPattern = regexp.MustCompile(`(?i)-----(?:BEGIN|END)[^-]*PRIVATE KEY[^-]*-----`)

// NOTE (P1-d reverted): a `privateKeyBody` length+entropy heuristic once lived
// here to catch an ARMORLESS base64 DER key body (no `-----BEGIN…-----`). It was
// removed — corpus review showed it is net-negative: base64 of ANY binary/JSON
// mixes upper+lower+digit BY CONSTRUCTION, so a blind length+entropy gate cannot
// tell a key from an SSH PUBLIC key, a PEM certificate body, a bare base64 image,
// or a config blob (false POSITIVES that silently drop the user's legit rule),
// while a WireGuard PrivateKey (exactly 44 base64 chars), a raw Ed25519/X25519/
// EC-P256 scalar (~44), or a `base64,`-prefixed evasion slip straight through
// (false NEGATIVES). Markerless coverage, if revisited, must be REFUSE-ON-INTENT
// (key-intent words adjacent to a base64 blob) or SPECIFIC-FORMAT (e.g. the
// WireGuard `PrivateKey = ` 44-char context), never a blind length+entropy gate.
// See docs/decisions/2026-06-24-p1d-privatekeybody-reverted.md.
//
// The ONE markerless secret now covered (pre-public hardening, 2026-06-28) is the
// AWS secret access key, via the sanctioned SPECIFIC-FORMAT path: a maximal base64
// run (base64Run) of EXACTLY 40 chars that is NOT a pure-hex git SHA AND is
// adjacent to AWS context (a WORD-boundary `aws` AND the tight `secret access key`
// intent phrase). See redactAWSSecretKeys / containsAWSSecretKey above. It redacts
// only the token, never the rule, and is FP-proofed in lexicon_aws_test.go +
// lexicon_aws_fix_test.go (awstats/gateways/keystore/"cache key", git SHA, long
// path, context-free base64 blob, a 44-char WireGuard key, and a multibyte-prefix
// rune all survive untouched / never panic).

// ContainsPrivateKeyMarker reports whether text carries ANY private-key BEGIN/END
// marker (the hard-reject backstop for a partially-redacted key).
func ContainsPrivateKeyMarker(text string) bool {
	return pemMarkerPattern.MatchString(text)
}

// ContainsCredential reports whether text contains a credential-shaped span
// (the shared hard-reject predicate). Exported so the capture redactor and the
// emitter refusal use the SAME pattern (Test 56). credentialPattern already
// matches a whole PEM/PGP private-key block AND a lone/truncated BEGIN header
// (P1-b); the orphan-END backstop (a partial redaction that wiped only BEGIN) is
// applied separately by callers via ContainsPrivateKeyMarker / pemMarkerPattern.
// (An armorless/markerless DER body is NOT length-heuristically gated; see the
// P1-d revert note above.)
func ContainsCredential(text string) bool {
	return credentialPattern.MatchString(text) || containsAWSSecretKey(text)
}

// RedactCredentials replaces every credential-shaped span in text with
// [REDACTED]. Operates on a FLATTENED content string only (never raw JSON —
// mutating a JSON envelope can break the parse, §4.7). Surrounding prose / URLs
// / field names are preserved.
func RedactCredentials(text string) string {
	out := credentialPattern.ReplaceAllString(text, "[REDACTED]")
	return redactAWSSecretKeys(out)
}

// noisePatterns ports reference engine NOISE_PATTERNS, re.search semantics
// (unanchored). The (?i) inline flag is preserved per-pattern as in Python.
var noisePatterns = mustCompileAll([]string{
	`(?i)\btraceback\b`,
	`(?i)\bexception\b`,
	`(?i)\berror:\s`,
	`(?i)\bjournalctl\b`,
	`(?i)\bstack trace\b`,
	`(?i)\bsession duration\b`,
	`(?i)\btoken usage\b`,
	`(?i)\bnpm err!\b`,
	`(?i)\bwarning:\s`,
	`(?i)\bconnection refused\b`,
	`(?i)\bfailed to refresh\b`,
	`(?i)\bfailed to connect\b`,
	`(?i)\buvicorn\[`,
	`(?i)Fix this error in \w+\.\s*Error:`,
	`(?i)\bHTTP/\d`,
	`(?i)\bexit code \d`,
})

// lowSignalPatterns ports reference engine LOW_SIGNAL_PATTERNS. Python
// applies them with re.match (anchored at START), so each is anchored with ^.
// The ones already carrying ^ keep it; the (?i)^ok$ etc. are anchored as-is.
var lowSignalPatterns = mustCompileAll([]string{
	`(?i)^ok$`, `(?i)^done$`, `(?i)^fixed$`,
	`^\d+$`, `(?i)^n/?a$`, `(?i)^yes$`, `(?i)^no$`,
})

// preferenceSignals ports reference engine PREFERENCE_SIGNALS, EN+RU,
// re.search semantics (unanchored).
//
// PORT NOTE (Unicode word boundaries): Python's `re` `\b` is Unicode-aware, so
// `\bвсегда\b` matches a standalone Cyrillic word. Go's RE2 `\b` is ASCII-ONLY —
// it does NOT see a Cyrillic letter as a word char, so `\bвсегда\b` would match
// at EVERY position adjacent to Cyrillic (or fail). The three Cyrillic patterns
// are therefore re-expressed with an explicit Unicode boundary `(?:^|\P{L})…
// (?:\P{L}|$)` (start-or-non-letter … non-letter-or-end), which reproduces the
// Python `\b` semantics for whole-word Cyrillic matching. The ASCII (EN) patterns
// keep `\b` verbatim (RE2 `\b` is correct for ASCII). Verified against the real
// Python on the §2.7.1 fixtures (всегда→1 hit; никогда+не→2 hits).
var preferenceSignals = mustCompileAll([]string{
	`(?i)\bprefer`, `(?i)\bshould\b`, `(?i)\bmust\b`,
	`(?i)\bdecision\b`, `(?i)\bbecause\b`,
	`(?i)\bdenis\b`, `(?i)\bceo\b`,
	`(?i)\brule\b`, `(?i)\bprinciple\b`,
	`(?i)\bavoid\b`, `(?i)\bnever\b`, `(?i)\balways\b`,
	`(?i)\bcorrect`, `(?i)\bdon't\b`,
	`(?i)(?:^|\P{L})не\s`,
	`(?i)(?:^|\P{L})всегда(?:\P{L}|$)`,
	`(?i)(?:^|\P{L})никогда(?:\P{L}|$)`,
})

func mustCompileAll(pats []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(pats))
	for i, p := range pats {
		out[i] = regexp.MustCompile(p)
	}
	return out
}

// countSearch counts how many of pats match text anywhere (re.search semantics).
func countSearch(pats []*regexp.Regexp, text string) int {
	n := 0
	for _, p := range pats {
		if p.MatchString(text) {
			n++
		}
	}
	return n
}

// anyMatchAnchored reports whether any of pats matches text (the patterns are
// already ^-anchored, mirroring Python re.match).
func anyMatchAnchored(pats []*regexp.Regexp, text string) bool {
	for _, p := range pats {
		if p.MatchString(text) {
			return true
		}
	}
	return false
}

// Quality is the result of classifyQuality (port of reference engine classify_quality
//).
type Quality struct {
	Score    float64
	Flags    []string
	PrefHits int    // the COUNT (the T1 gate keys on this, not the flag — BR-A6)
	Hint     string // "validated" | "new" | "rejected"
}

// classifyQuality ports reference engine classify_quality exactly:
// additive/clamp scoring with a credential HARD-REJECT, length/noise/low-signal
// penalties and a preference bonus. The length check uses utf8.RuneCountInString
// (M3-R10) — Python len counts runes, so a 22-byte/11-rune RU string must NOT
// falsely flag too_short.
func classifyQuality(title, content string) Quality {
	text := strings.TrimSpace(title + " " + content)
	score := 0.5
	var flags []string

	// Hard reject: credentials. The marker check is P1-b defense
	// in depth: even if an upstream redaction wiped a key's BEGIN header (so the
	// whole-block pattern no longer matches), a residual BEGIN/END PRIVATE KEY
	// marker still means a key body was present → never persist it. (The P1-d
	// armorless-body length+entropy heuristic was reverted as net-negative — see
	// the revert note at pemMarkerPattern.)
	if credentialPattern.MatchString(text) || pemMarkerPattern.MatchString(text) || containsAWSSecretKey(text) {
		return Quality{Score: 0.0, Flags: []string{"credentials"}, PrefHits: 0, Hint: "rejected"}
	}

	// Length checks — RUNE count, not byte len (M3-R10 / F-10).
	n := utf8.RuneCountInString(text)
	if n < 20 {
		flags = append(flags, "too_short")
		score -= 0.4
	} else if n > 4000 {
		flags = append(flags, "too_long")
		score -= 0.2
	}

	// Noise patterns (re.search).
	noise := countSearch(noisePatterns, text)
	if noise >= 2 {
		flags = append(flags, "noise_log")
		score -= 0.6
	} else if noise == 1 {
		flags = append(flags, "noise_partial")
		score -= 0.3
	}

	// Low signal (re.match — anchored, over the trimmed TITLE).
	if anyMatchAnchored(lowSignalPatterns, strings.TrimSpace(title)) {
		flags = append(flags, "low_signal")
		score -= 0.4
	}

	// Preference/rule signals.
	prefHits := countSearch(preferenceSignals, text)
	if prefHits >= 2 {
		flags = append(flags, "preference_signal")
		score += 0.3
	} else if prefHits == 1 {
		score += 0.15
	}

	// Clamp.
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}

	var hint string
	switch {
	case score >= 0.75:
		hint = "validated"
	case score >= 0.45:
		hint = "new"
	default:
		hint = "rejected"
	}
	return Quality{Score: round3(score), Flags: flags, PrefHits: prefHits, Hint: hint}
}

// round3 rounds to 3 decimals, matching Python round(score, 3).
func round3(f float64) float64 {
	// round half away from zero to mirror Python's round closely enough for
	// the golden table (all fixtures are .x5 / .x clean values).
	scaled := f * 1000
	if scaled >= 0 {
		scaled = float64(int64(scaled + 0.5))
	} else {
		scaled = float64(int64(scaled - 0.5))
	}
	return scaled / 1000
}

// normalizeTitle ports reference engine normalize_title: strip → lowercase
// → collapse whitespace.
func normalizeTitle(title string) string {
	return collapseWS(strings.ToLower(strings.TrimSpace(title)))
}

var wsRun = regexp.MustCompile(`\s+`)

func collapseWS(s string) string { return wsRun.ReplaceAllString(s, " ") }

// titleHash is the dedup primary key: sha256 of the normalized title (port of
// compute_title_hash).
func titleHash(title string) string {
	sum := sha256.Sum256([]byte(normalizeTitle(title)))
	return hex.EncodeToString(sum[:])
}
