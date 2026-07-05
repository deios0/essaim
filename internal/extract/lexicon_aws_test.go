package extract

import "testing"

// Pre-public hardening (2026-06-28): the markerless AWS *secret* access key gap.
//
// A 40-char base64 AWS secret access key carries no `secret[:=]` separator and no
// armor, so the keyed/marker patterns in credentialPattern miss it (review
// follow-up #1, lexicon.go gap). The p1d revert
// (docs/decisions/2026-06-24-p1d-privatekeybody-reverted.md) FORBIDS a blind
// length+entropy gate because it both drops legit rules (SSH public keys, PEM
// cert bodies, base64 blobs) AND misses real 44-char WireGuard keys. The
// sanctioned alternatives are refuse-on-intent or SPECIFIC-FORMAT. The AWS
// secret access key has a specific format (exactly 40 chars over [A-Za-z0-9/+]),
// so we detect THAT shape and gate it to AWS context (`aws` + one of
// secret/access/key within a short window). We REDACT just the token to
// [REDACTED]; we never drop the whole rule.
//
// These tests are the FP-safety gate. They must hold before the pattern ships.

// The real AWS secret (40-char base64) WITH aws context IS redacted — the whole
// canonical AWS example string and the bare-token-with-context form. The token
// alone is redacted to [REDACTED]; the surrounding prose survives.
func TestAWSSecretKeyRedactedWithContext(t *testing.T) {
	cases := []struct {
		name, in, token string
	}{
		{
			name:  "canonical aws example sentence",
			in:    "for testing use the aws secret access key wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY in the demo",
			token: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		},
		{
			name:  "labelled secret with slashes",
			in:    "AWS_SECRET_ACCESS_KEY je7MtGbClwBF/2Zp9Utk/h3yCo8nvbEXAMPLEKEY rotate it",
			token: "je7MtGbClwBF/2Zp9Utk/h3yCo8nvbEXAMPLEKEY",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !ContainsCredential(c.in) {
				t.Errorf("ContainsCredential(%q) = false, want true (AWS secret w/ context)", c.in)
			}
			out := RedactCredentials(c.in)
			if contains(out, c.token) {
				t.Errorf("RedactCredentials left the AWS secret token behind:\n%s", out)
			}
			if !contains(out, "[REDACTED]") {
				t.Errorf("RedactCredentials must mark the token [REDACTED]; got %q", out)
			}
			// We redact the TOKEN, not the rule: surrounding words survive.
			if !contains(out, "aws") && !contains(out, "AWS") {
				t.Errorf("redaction dropped the surrounding prose (should redact token only): %q", out)
			}
		})
	}
}

// The AWS access-key-ID (AKIA…) and the keyed/separator forms still work — the
// new token pass must not regress the existing detectors.
func TestAWSAccessKeyIDAndKeyedFormsStillRedact(t *testing.T) {
	positives := []string{
		"AKIAIOSFODNN7EXAMPLE", // access key ID (AKIA pattern)
		"aws_secret_access_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", // keyed form (separator)
		"secret: 'wJalrXUtnFEMIabcdefghij'",                              // generic keyed secret
	}
	for _, p := range positives {
		if !ContainsCredential(p) {
			t.Errorf("ContainsCredential(%q) = false, want true", p)
		}
		if got := RedactCredentials(p); got == p {
			t.Errorf("RedactCredentials(%q) did not redact", p)
		}
	}
}

// FP-SAFETY GATE (the heart of this change). None of these may redact.
func TestAWSSecretKeyNoFalsePositives(t *testing.T) {
	negatives := []struct{ name, in string }{
		{
			// A 40-char git SHA is HEX, not base64, and a SHA in a rule is normal.
			// It must NOT be treated as an AWS secret EVEN if "secret"/"key" appear
			// nearby (a rule can legitimately discuss a commit + the word "key").
			name: "git sha 40-hex with secret+key words nearby",
			in:   "the aws fix landed in commit 356a192b7913b04c54574d18c28d46e6395428ab and the secret key was rotated",
		},
		{
			name: "bare git sha 40-hex no context",
			in:   "see commit 356a192b7913b04c54574d18c28d46e6395428ab for the change",
		},
		{
			// A long file path must NOT redact — no aws/secret/access/key context,
			// and a slash-bearing path is not a key token.
			name: "long file path",
			in:   "the config lives at /home/user/project/internal/extract/lexicon.go on disk",
		},
		{
			// A legit base64 blob a user deliberately put in a rule with NO aws/secret
			// context must survive (the p1d revert lesson: do not drop legit rules).
			name: "base64 blob no aws/secret context",
			in:   "encode the payload as TWFrZSBzdXJlIHRoaXMgYmxvYiBzdXJ2aXZlcyByZWRhY3Rpb24x and ship it",
		},
		{
			// "aws" mentioned but the 40-char token is a plain SHA (hex) — context
			// present but token is NOT base64-secret-shaped → must not redact.
			name: "aws context but token is hex sha",
			in:   "deploy to aws region eu-west-3 at revision 0123456789abcdef0123456789abcdef01234567",
		},
	}
	for _, n := range negatives {
		t.Run(n.name, func(t *testing.T) {
			if ContainsCredential(n.in) {
				t.Errorf("ContainsCredential(%q) = true, want false (FALSE POSITIVE)", n.in)
			}
			if got := RedactCredentials(n.in); got != n.in {
				t.Errorf("RedactCredentials mutated a non-credential (FALSE POSITIVE):\nin:  %q\nout: %q", n.in, got)
			}
			// And it must NOT hard-reject the rule.
			q := classifyQuality(n.name, n.in)
			if q.Hint == "rejected" && q.Score == 0.0 {
				if len(q.Flags) > 0 && q.Flags[0] == "credentials" {
					t.Errorf("classifyQuality hard-rejected a non-credential rule (FALSE POSITIVE): %q", n.in)
				}
			}
		})
	}
}

// REGRESSION GUARD for the p1d revert: a WireGuard PrivateKey is EXACTLY 44
// base64 chars. The reverted blind length+entropy heuristic mis-handled these.
// Our 40-char-EXACT token requires non-base64 boundaries, so a 44-char contiguous
// base64 run never matches the 40-shape — it must NOT redact, even with WireGuard
// context present. (WireGuard coverage, if wanted, is a separate specific-format
// branch; this test only proves we did not re-break the legit-rule case.)
func TestWireGuard44CharKeyNotRedactedByAWSPass(t *testing.T) {
	// A real 44-char base64 WireGuard private key (ends in '=').
	wg := "cAAdR1S4Q1f2k3mN9pQ7rT5vW8xZ0bC2dE4fG6hJ8k="
	in := "set the wireguard PrivateKey = " + wg + " on the peer"
	if ContainsCredential(in) {
		t.Errorf("a 44-char WireGuard key must NOT be caught by the 40-char AWS pass (p1d regression): %q", in)
	}
	if got := RedactCredentials(in); contains(got, "[REDACTED]") && !contains(got, wg) {
		t.Errorf("the AWS pass redacted a 44-char WireGuard key (p1d regression):\n%s", got)
	}
}

// A 40-char base64 token WITHOUT aws context must NOT redact even if it is
// base64-shaped (the context gate is what keeps FPs near zero). With the full
// aws + `secret access key` intent context, the SAME token IS redacted. This
// proves the gate is the discriminator. (Context gate tightened 2026-06-28,
// review defect D1: the gate now requires the ordered `secret access key`
// phrase, NOT a bare "secret"/"access"/"key", so "the aws secret <token>" alone
// no longer qualifies — that loosened form let "cache key"/"awstats" rules
// through.)
func TestAWSSecretKeyContextGate(t *testing.T) {
	token := "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	noCtx := "store the value " + token + " somewhere safe"
	if ContainsCredential(noCtx) {
		t.Errorf("a 40-char base64 token with NO aws/secret context must NOT redact: %q", noCtx)
	}
	bareSecretOnly := "the aws secret " + token + " must be handled"
	if ContainsCredential(bareSecretOnly) {
		t.Errorf("a bare 'aws secret' (no 'access key' phrase) must NOT redact under the tightened gate: %q", bareSecretOnly)
	}
	withCtx := "the aws secret access key " + token + " must be redacted"
	if !ContainsCredential(withCtx) {
		t.Errorf("the SAME token WITH the full aws secret access key context MUST redact: %q", withCtx)
	}
}
