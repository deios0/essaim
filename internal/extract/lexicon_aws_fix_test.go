package extract

import (
	"strings"
	"testing"
)

// Pre-public hardening follow-up (2026-06-28): dev-protocol review found three
// defects in the markerless AWS secret-key path (hasAWSContext + the AWS-secret
// redaction). Each is reproduced here as a permanent regression test.
//
//   D1 (BLOCKER, the p1d regression): hasAWSContext used bare strings.Contains
//      for "aws" and "key"/"access"/"secret", so "awstats", "gateways",
//      "keystore", "cache key" tripped it. A 40-char token in a benign rule got
//      [REDACTED] and classifyQuality hard-rejected the whole rule.
//   D2 (BLOCKER, reachable panic): hasAWSContext sliced a separately-lowered
//      full string with indices into the ORIGINAL text. Some runes shrink in
//      bytes when lowercased (U+212A Kelvin -> "k"), so lo > len(lowered) panics.
//   D3 (P1, real-secret false negative): the boundary-consuming regex skipped
//      the second of two adjacent secrets.

// D1 — the review's EXACT reproduction case. A benign rule that mentions
// "awstats" and "cache key" next to a 40-char base64-ish token must NOT be
// redacted and must NOT be hard-rejected by classifyQuality.
func TestD1_AWStatsCacheKeyNotRedactedNorRejected(t *testing.T) {
	in := "For awstats, the cache key AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AbAB must stay stable"

	if ContainsCredential(in) {
		t.Errorf("ContainsCredential(%q) = true, want false (D1 false positive: awstats/cache key)", in)
	}
	if got := RedactCredentials(in); got != in {
		t.Errorf("RedactCredentials mutated a benign rule (D1 false positive):\nin:  %q\nout: %q", in, got)
	}
	q := classifyQuality("cache key stability", in)
	if q.Hint == "rejected" && q.Score == 0.0 && len(q.Flags) > 0 && q.Flags[0] == "credentials" {
		t.Errorf("classifyQuality hard-rejected a benign rule (D1 regression — p1d false-positive rule drop): %q", in)
	}
}

// D1 battery — the other substring traps that bare strings.Contains let through.
// "gateways" and "keystore" both embed the literal substrings the old gate keyed
// on; none of these benign rules may redact or be rejected.
func TestD1_WordBoundaryTraps(t *testing.T) {
	cases := []struct{ name, in string }{
		{"gateways substring", "the gateways route traffic; token AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AbAB is benign"},
		{"keystore substring", "load the keystore entry AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AbAB into memory"},
		{"awstats + access word", "awstats grants access to AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AbAB reports"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if ContainsCredential(c.in) {
				t.Errorf("ContainsCredential(%q) = true, want false (D1 substring trap)", c.in)
			}
			if got := RedactCredentials(c.in); got != c.in {
				t.Errorf("RedactCredentials mutated a benign rule (D1 substring trap):\nin:  %q\nout: %q", c.in, got)
			}
			q := classifyQuality(c.name, c.in)
			if q.Hint == "rejected" && q.Score == 0.0 && len(q.Flags) > 0 && q.Flags[0] == "credentials" {
				t.Errorf("classifyQuality hard-rejected a benign rule (D1 substring trap): %q", c.in)
			}
		})
	}
}

// D2 — a multibyte rune (U+212A Kelvin) positioned so that text indices overrun
// the separately-lowered string. The OLD code panics ("slice bounds out of
// range"); the fix must NOT panic and must still behave correctly (the rule has
// no AWS secret, so nothing is redacted).
func TestD2_MultibyteRuneNoPanic(t *testing.T) {
	// U+212A KELVIN SIGN: 3 bytes, lowercases to ascii "k" (1 byte). A run of
	// them makes strings.ToLower(text) much shorter than text, so any lo/hi
	// derived from text overshoots len(lowered).
	prefix := strings.Repeat("K", 60) // 180 bytes -> 60 bytes lowered
	in := prefix + " the cache key AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AbAB stays put"

	// Must not panic on any of the three readers.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("D2 panic (slice bounds): %v", r)
		}
	}()
	got := ContainsCredential(in)
	_ = RedactCredentials(in)
	_ = ContainsPrivateKeyMarker(in)

	// No AWS secret present (benign cache key) → must be false / unchanged.
	if got {
		t.Errorf("ContainsCredential(multibyte-prefixed benign rule) = true, want false")
	}
	if out := RedactCredentials(in); out != in {
		t.Errorf("RedactCredentials mutated a benign multibyte rule:\nin:  %q\nout: %q", in, out)
	}
}

// D2b — a multibyte rune adjacent to a REAL aws secret must still redact (the
// fix must preserve correctness, not just dodge the panic). The window is taken
// from the original text and lowered locally, so multibyte context still works.
func TestD2_MultibyteRuneStillRedactsRealSecret(t *testing.T) {
	prefix := strings.Repeat("K", 8)
	token := "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	in := prefix + " aws secret access key " + token + " rotate it"

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("D2b panic: %v", r)
		}
	}()
	if !ContainsCredential(in) {
		t.Errorf("ContainsCredential(multibyte + real aws secret) = false, want true")
	}
	out := RedactCredentials(in)
	if strings.Contains(out, token) {
		t.Errorf("real aws secret survived redaction with multibyte prefix:\n%s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected [REDACTED] marker; got %q", out)
	}
}

// D3 — two adjacent 40-char secrets, both with AWS context, must BOTH redact.
// The old boundary-consuming regex matched the trailing boundary of secret #1,
// which was also the leading boundary of secret #2, so #2 was skipped.
func TestD3_AdjacentSecretsBothRedacted(t *testing.T) {
	s1 := "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	s2 := "je7MtGbClwBF/2Zp9Utk/h3yCo8nvbEXAMPLEKEY"
	in := "aws secret access key " + s1 + " " + s2

	if !ContainsCredential(in) {
		t.Errorf("ContainsCredential(two adjacent aws secrets) = false, want true")
	}
	out := RedactCredentials(in)
	if strings.Contains(out, s1) {
		t.Errorf("D3: first adjacent secret survived:\n%s", out)
	}
	if strings.Contains(out, s2) {
		t.Errorf("D3: second adjacent secret survived (the boundary-skip bug):\n%s", out)
	}
	// Both should be marked.
	if n := strings.Count(out, "[REDACTED]"); n != 2 {
		t.Errorf("D3: expected 2 [REDACTED] markers, got %d:\n%s", n, out)
	}
}

// FP BATTERY — the permanent gate for the tightened AWS secret-key path. Every
// token the dev-protocol review enumerated, with its redact/not-redact verdict
// AND a "rule is never dropped" assertion for the negatives. A negative MUST:
//   - not be flagged by ContainsCredential,
//   - be returned byte-for-byte unchanged by RedactCredentials, and
//   - NOT be hard-rejected by classifyQuality (Score 0.0 / Flags=[credentials]).
//
// A positive MUST be redacted (token gone, [REDACTED] present) and detected.
func TestAWSSecretKeyFPBattery(t *testing.T) {
	const tok = "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AbAB" // 40-char base64-ish, NOT hex
	const realSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	const realSecret2 = "je7MtGbClwBF/2Zp9Utk/h3yCo8nvbEXAMPLEKEY"

	type tc struct {
		name   string
		in     string
		redact bool // true => must redact; false => must survive untouched + not be dropped
	}
	cases := []tc{
		// ---- NEGATIVES (must survive, must not drop the rule) ----
		{"awstats + cache key (the p1d regression)", "For awstats, the cache key " + tok + " must stay stable", false},
		{"gateways substring", "the gateways route traffic; token " + tok + " is benign", false},
		{"keystore substring", "load the keystore entry " + tok + " into memory", false},
		{"cache key phrase", "persist the cache key " + tok + " between runs", false},
		{"40-char git SHA (hex)", "fix landed in commit 356a192b7913b04c54574d18c28d46e6395428ab today", false},
		{"40-char git SHA with aws+secret access key words", "aws fix in commit 356a192b7913b04c54574d18c28d46e6395428ab; secret access key rotated", false},
		{"44-char WireGuard key", "set wireguard PrivateKey = cAAdR1S4Q1f2k3mN9pQ7rT5vW8xZ0bC2dE4fG6hJ8k= on peer", false},
		{"context-free base64 blob", "encode the payload as TWFrZSBzdXJlIHRoaXMgYmxvYiBzdXJ2aXZlcyByZWRhY3Rpb24x and ship", false},
		{"long file path", "config at /home/user/project/internal/extract/lexicon.go on disk", false},
		{"multibyte rune prefix (no panic)", strings.Repeat("K", 40) + " the cache key " + tok + " stays put", false},

		// ---- POSITIVES (must redact) ----
		{"real aws secret access key (40 base64)", "aws_secret_access_key = " + realSecret, true},
		{"two adjacent secrets with context", "aws secret access key " + realSecret + " " + realSecret2, true},
		{"AKIA access key id", "credentials: AKIAIOSFODNN7EXAMPLE in the file", true},
		{"keyed aws_secret_access_key form", "aws_secret_access_key=" + realSecret, true},
		{"canonical aws secret sentence", "for testing use the aws secret access key " + realSecret + " in the demo", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("PANIC on %q: %v", c.in, r)
				}
			}()
			got := ContainsCredential(c.in)
			out := RedactCredentials(c.in)
			if c.redact {
				if !got {
					t.Errorf("ContainsCredential=false, want true (positive): %q", c.in)
				}
				if out == c.in || !strings.Contains(out, "[REDACTED]") {
					t.Errorf("expected redaction, got %q", out)
				}
				return
			}
			// Negative: never detected, never mutated, never dropped.
			if got {
				t.Errorf("FALSE POSITIVE — ContainsCredential=true: %q", c.in)
			}
			if out != c.in {
				t.Errorf("FALSE POSITIVE — RedactCredentials mutated rule:\nin:  %q\nout: %q", c.in, out)
			}
			q := classifyQuality(c.name, c.in)
			if q.Score == 0.0 && q.Hint == "rejected" && len(q.Flags) > 0 && q.Flags[0] == "credentials" {
				t.Errorf("FALSE POSITIVE — classifyQuality DROPPED the rule (the p1d failure): %q", c.in)
			}
		})
	}
}

// Belt-and-braces: assert the new pattern NEVER hard-rejects a benign rule via
// the credentials flag for the full negative battery, in addition to the
// per-case check above (the explicit "no rule is ever dropped" guarantee).
func TestAWSSecretKeyNeverDropsBenignRule(t *testing.T) {
	const tok = "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AbAB"
	benign := []string{
		"For awstats, the cache key " + tok + " must stay stable",
		"the gateways route traffic; token " + tok + " is benign",
		"load the keystore entry " + tok + " into memory",
		"see commit 356a192b7913b04c54574d18c28d46e6395428ab for the change",
		"set wireguard PrivateKey = cAAdR1S4Q1f2k3mN9pQ7rT5vW8xZ0bC2dE4fG6hJ8k= on peer",
	}
	for _, in := range benign {
		q := classifyQuality("benign rule", in)
		for _, f := range q.Flags {
			if f == "credentials" {
				t.Errorf("benign rule wrongly flagged 'credentials' (rule dropped): %q", in)
			}
		}
	}
}
