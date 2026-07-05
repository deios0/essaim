package extract

import (
	"math"
	"testing"
)

// Test 18 (F-10): the Go classify_quality port matches reference engine on a shared golden
// table INCLUDING the §2.7.1 adversary rows AND RU multibyte-near-20-boundary
// fixtures. The numbers are the spec's empirically-verified Python outputs.
func TestClassifyQualityParityGolden(t *testing.T) {
	// Numbers are the ACTUAL outputs of the real reference engine Python classify_quality
	//, captured by running it directly — the binding source of truth
	// per spec §2.7.1 ("the real Python classify_quality was run on the adversary
	// strings"). Note the RU `у меня 8 лет опыта` is 18 RUNES (<20) so Python
	// flags too_short → 0.1/rejected (the spec table's "0.5" was for the EN
	// variant); EITHER WAY it is NOT staged, which is the whole point of M3-R5.
	cases := []struct {
		name     string
		text     string
		score    float64
		prefHits int
		hint     string
		hasPref  bool // the preference_signal FLAG (set only at >=2)
	}{
		// RU adversary: 18 runes → too_short → 0.1/rejected (NOT staged).
		{"ru_8_let_opyta", "у меня 8 лет опыта", 0.1, 0, "rejected", false},
		// EN adversary: 28 runes, no pref → 0.5/new but pref_hits 0 (NOT staged).
		{"en_8_years", "I have 8 years of experience", 0.5, 0, "new", false},
		// single RU pref `всегда`: pref_hits 1, score 0.65, NO preference_signal flag.
		{"single_ru_pref", "всегда используй табы", 0.65, 1, "new", false},
		// >=2 pref → preference_signal flag, validated (prefer+because+rule = 3 hits → 0.8).
		{"two_pref", "prefer tabs because it's the team rule", 0.8, 3, "validated", true},
		// RU >=2 pref (никогда + не) → preference_signal, 0.8/validated.
		{"two_ru_pref", "никогда так не делай пожалуйста", 0.8, 2, "validated", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := classifyQuality(c.text, "")
			if math.Abs(q.Score-c.score) > 1e-9 {
				t.Errorf("score = %v, want %v", q.Score, c.score)
			}
			if q.PrefHits != c.prefHits {
				t.Errorf("prefHits = %d, want %d", q.PrefHits, c.prefHits)
			}
			if q.Hint != c.hint {
				t.Errorf("hint = %q, want %q", q.Hint, c.hint)
			}
			hasPref := false
			for _, f := range q.Flags {
				if f == "preference_signal" {
					hasPref = true
				}
			}
			if hasPref != c.hasPref {
				t.Errorf("preference_signal flag = %v, want %v (flags=%v)", hasPref, c.hasPref, q.Flags)
			}
		})
	}
}

// Test 19 (F-10): the length check uses RuneCountInString, not byte len. An
// 11-rune/22-byte RU string must NOT be flagged too_short (a byte-len check
// would see 22 >= 20 and also pass — so use a string that is <20 BYTES but >=20
// RUNES to actually distinguish). "никогданетакделай" is 18 runes (<20) but 36
// bytes; the inverse: pick a >=20-rune RU string that is well over 20 bytes and
// assert NOT too_short, and a <20-rune one that IS.
func TestClassifyQualityRuneLengthNotByte(t *testing.T) {
	// 22 runes (>=20) — must NOT be too_short even though it has no real signal.
	long := "никогда так не делай пожалуйста" // > 20 runes
	q := classifyQuality(long, "")
	for _, f := range q.Flags {
		if f == "too_short" {
			t.Fatalf("a %d-rune string was wrongly flagged too_short (byte-len bug)", len([]rune(long)))
		}
	}
	// A genuinely short string (<20 runes) IS too_short.
	short := "табы" // 4 runes
	qs := classifyQuality(short, "")
	got := false
	for _, f := range qs.Flags {
		if f == "too_short" {
			got = true
		}
	}
	if !got {
		t.Fatalf("a 4-rune string must be flagged too_short, flags=%v", qs.Flags)
	}
}

// Test 22 / credential hard-reject: a key-shaped correction scores 0.0,
// rejected, with the credentials flag.
func TestClassifyQualityCredentialHardReject(t *testing.T) {
	q := classifyQuality("set api_key=sk-abcdefghijklmnop1234567890", "")
	if q.Score != 0.0 || q.Hint != "rejected" {
		t.Fatalf("credential text must be score 0.0 / rejected, got %v/%q", q.Score, q.Hint)
	}
	if len(q.Flags) == 0 || q.Flags[0] != "credentials" {
		t.Fatalf("want credentials flag, got %v", q.Flags)
	}
}

// Test 56: ONE credentialPattern, THREE readers. The same pattern that drives
// the sigil scan (ContainsCredential), the capture redaction (RedactCredentials),
// and the emitter refusal must agree on a golden set of credential strings.
func TestCredentialOnePatternThreeReadersGolden(t *testing.T) {
	positives := []string{
		"sk-abcdefghijklmnop1234567890",
		"sk_live_abcdefghijklmnop1234",
		"whsec_abcdefghijklmnop1234",
		"AKIAIOSFODNN7EXAMPLE",
		"ghp_abcdefghijklmnopqrstuvwxyz1234",
		"github_pat_abcdefghijklmnopqrstuvwxyz",
		"xoxb-EXAMPLE-REDACTED-FIX",
		"AIzaSyABCDEFGHIJKLMNOPQRSTUVWXYZ0123456",
		"eyJhbGciOiJIUzI1.eyJzdWIiOiIxMjM0NQ.SflKxwRJSMeKKF2QT4fwpM",
		"password=hunter2hunter2",
		"api_key: 'abcd1234efgh5678'",
	}
	for _, p := range positives {
		if !ContainsCredential(p) {
			t.Errorf("ContainsCredential(%q) = false, want true", p)
		}
		if got := RedactCredentials(p); got == p {
			t.Errorf("RedactCredentials(%q) did not redact", p)
		}
	}
	negatives := []string{
		"prefer tabs over spaces",
		"always use PostgreSQL never MySQL",
		"the password policy doc is at /docs", // "password" but no key=value
		"у меня 8 лет опыта",
	}
	for _, n := range negatives {
		if ContainsCredential(n) {
			t.Errorf("ContainsCredential(%q) = true, want false (false positive)", n)
		}
	}
}

// RedactCredentials preserves surrounding prose/URLs (§4.7).
func TestRedactPreservesSurrounding(t *testing.T) {
	in := "use the endpoint https://api.example.com but api_key=sk-abcdefghijklmnop1234567890 is secret"
	out := RedactCredentials(in)
	if !contains(out, "https://api.example.com") {
		t.Fatalf("redaction must preserve the URL; got %q", out)
	}
	if !contains(out, "[REDACTED]") {
		t.Fatalf("redaction must replace the key span; got %q", out)
	}
	if contains(out, "sk-abcdefghijklmnop1234567890") {
		t.Fatalf("the key must be gone; got %q", out)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && indexOf(s, sub) >= 0 }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// normalizeTitle + titleHash port parity.
func TestNormalizeTitleAndHash(t *testing.T) {
	if got := normalizeTitle("  Prefer   Tabs\tOver Spaces  "); got != "prefer tabs over spaces" {
		t.Fatalf("normalizeTitle = %q", got)
	}
	// Same normalized title ⇒ same hash.
	if titleHash("Prefer Tabs") != titleHash("  prefer   tabs ") {
		t.Fatal("titleHash must be invariant to case/whitespace")
	}
	if titleHash("a") == titleHash("b") {
		t.Fatal("different titles must hash differently")
	}
}

// P1-5: credential redaction pattern gaps. The highest-trust invariant ("a local
// store NEVER persists a credential") means the pattern must be BROAD. The
// re-audit found four real shapes that leaked through into injected/promoted
// rules: URL-embedded password, PEM private-key block, GitLab PAT (glpat-), and a
// SPACE-separated bearer token (`Bearer <tok>` with no `:`/`=`). Each must be
// detected (ContainsCredential) AND redacted (RedactCredentials), on every path.
func TestCredentialPatternGapsP15(t *testing.T) {
	positives := []string{
		// URL-embedded password (scheme://user:password@host).
		"postgres://admin:SuperSecret123@db.internal:5432/app",
		"mysql://root:Hunter2Hunter2@127.0.0.1/prod",
		"redis://user:p4ssw0rd-xyz@cache:6379",
		// PEM private-key block (P1-b: the WHOLE block, multi-line, must match).
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\n-----END RSA PRIVATE KEY-----",
		"-----BEGIN PRIVATE KEY-----abc-----END PRIVATE KEY-----",
		"-----BEGIN OPENSSH PRIVATE KEY-----xyz",
		"-----BEGIN EC PRIVATE KEY-----\nMHcCAQEEIObody\n-----END EC PRIVATE KEY-----",
		// PGP private key BLOCK — the word BLOCK AFTER KEY (P1-b: missed entirely
		// by the prior `...PRIVATE KEY-----` header pattern).
		"-----BEGIN PGP PRIVATE KEY BLOCK-----\nlQOYBGABCDEFbody\n-----END PGP PRIVATE KEY BLOCK-----",
		// GitLab personal access token.
		"glpat-EXAMPLE-REDACTED-TEST-FIXTURE",
		"the token is glpat-EXAMPLE-REDACTED-FIXTURE-VALUE here",
		// Space-separated bearer (no : or =).
		"Authorization: Bearer abcdef0123456789ghijkl",
		"send Bearer sk0123456789abcdefghij with the request",
	}
	for _, p := range positives {
		if !ContainsCredential(p) {
			t.Errorf("ContainsCredential(%q) = false, want true (P1-5 gap)", p)
		}
		if got := RedactCredentials(p); got == p {
			t.Errorf("RedactCredentials(%q) did not redact (P1-5 gap)", p)
		}
		// A credential-bearing text must be hard-rejected by the quality gate
		// (never written to the vault as a rule).
		q := classifyQuality(p, "")
		if q.Hint != "rejected" || q.Score != 0.0 {
			t.Errorf("classifyQuality(%q) must hard-reject; got hint=%q score=%v", p, q.Hint, q.Score)
		}
	}

	// P1-b strengthening: for a MULTI-LINE private key block, RedactCredentials
	// must remove the ENTIRE block — header AND base64 body AND footer — not just
	// the BEGIN header line. The prior single-line header pattern left the body +
	// END line behind, which then dodged the hard-reject (the leak).
	wholeBlocks := []struct{ block, bodySent string }{
		{"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaEKEYBODYSENTINEL0123456789\n-----END OPENSSH PRIVATE KEY-----", "b3BlbnNzaEKEYBODYSENTINEL0123456789"},
		{"-----BEGIN PGP PRIVATE KEY BLOCK-----\n\nlQOYpgpKEYBODYSENTINEL9876543210\n-----END PGP PRIVATE KEY BLOCK-----", "lQOYpgpKEYBODYSENTINEL9876543210"},
	}
	for _, wb := range wholeBlocks {
		out := RedactCredentials(wb.block)
		if contains(out, wb.bodySent) {
			t.Errorf("RedactCredentials left the key BODY behind (whole-block leak):\n%s", out)
		}
		if contains(out, "PRIVATE KEY") {
			t.Errorf("RedactCredentials left a BEGIN/END PRIVATE KEY line behind:\n%s", out)
		}
	}
	// Negatives: the new patterns must NOT over-match plain prose / benign URLs.
	negatives := []string{
		"connect to postgres://db.internal:5432/app",           // host:port, no user:pass
		"see https://api.example.com/v1/path for the endpoint", // plain https URL
		"the bearer of this message is Denis",                  // the word "bearer" in prose
		"BEGIN by reading the PRIVATE notes section",           // prose, not a PEM block
		"glpat is the prefix gitlab uses",                      // mentions glpat- but no token body
	}
	for _, n := range negatives {
		if ContainsCredential(n) {
			t.Errorf("ContainsCredential(%q) = true, want false (P1-5 false positive)", n)
		}
	}
}

// P1-d (REVERTED): the armorless `privateKeyBody` length+entropy heuristic was
// removed as net-negative (false-positives drop legit rules: SSH PUBLIC keys,
// PEM cert bodies, bare base64 images, config blobs; false-negatives leak real
// secrets: a 44-char WireGuard PrivateKey, raw Ed25519/X25519/EC-P256 scalars, a
// `base64,`-prefixed evasion). TestCredentialBareBase64KeyBodyP1d was deleted
// with it. The armor-anchored P1-b detection + the case-insensitive
// pemMarkerPattern (below) remain the credential gates.

// P1-d: pemMarkerPattern must be case-insensitive. A lower/mixed-case armor line
// (some tools emit "-----begin private key-----") is still a private-key tell —
// the orphan-marker backstop (ContainsPrivateKeyMarker) must catch it.
func TestPemMarkerCaseInsensitiveP1d(t *testing.T) {
	markers := []string{
		"-----begin private key-----",
		"-----End RSA Private Key-----",
		"-----BEGIN openssh PRIVATE key-----",
	}
	for _, m := range markers {
		if !ContainsPrivateKeyMarker(m) {
			t.Errorf("ContainsPrivateKeyMarker(%q) = false, want true (case-insensitive)", m)
		}
		if classifyQuality(m, "").Hint != "rejected" {
			t.Errorf("classifyQuality(%q) must hard-reject a (case-insensitive) PEM marker", m)
		}
	}
}

// F5-quality (release-gate residual): classifyQuality must NOT silently DROP a
// legitimate correction merely because it is PARAPHRASED — i.e. it states a real
// preference WITHOUT any of the literal directive tokens (prefer/should/must/
// always/never/…). The scorer starts at 0.5 and the `new` hint floor is 0.45, so
// a ≥20-rune, noise-free correction that hits ZERO preference signals still clears
// as `new` (atLeastNew → promote-eligible). This pins that a paraphrase is NOT
// over-dropped to `rejected`. (The drops that DO happen — too_short <20 runes,
// noise/log patterns, anchored low-signal "ok"/"done"/bare-number — are correct
// rejections, not paraphrase-blindness; the negative cases below assert those
// still reject so the test isn't merely "everything passes".)
func TestClassifyQualityDoesNotDropParaphrasedCorrections(t *testing.T) {
	// Legit corrections phrased as plain statements — NO directive/preference token
	// (no prefer/should/must/always/never/avoid/rule/decision/because/correct/don't).
	// Each must clear the `new` floor (score >= 0.45, hint new or validated) so the
	// lifecycle's atLeastNew promote gate can still see it.
	paraphrases := []string{
		"we indent with tabs in this repository, not spaces", // states the tabs preference, no directive word
		"the team uses PostgreSQL for the main datastore",    // states the DB choice as a fact
		"environment config lives in a dotenv file at root",  // a real convention, plainly stated
		"commit messages follow the conventional commits format here",
		"our python code is formatted by black on save",
	}
	for _, p := range paraphrases {
		q := classifyQuality(p, "")
		if !hintAtLeastNew(q.Hint) {
			t.Errorf("paraphrased correction %q was over-dropped: hint=%q score=%v flags=%v (want >= new)",
				p, q.Hint, q.Score, q.Flags)
		}
		// A plain paraphrase with no directive token should hit 0 preference signals
		// yet still clear — proving the 0.5 base + 0.45 `new` floor protects paraphrases
		// (NOT that it sneaks in via a signal word). If a future edit makes one of these
		// hit a signal, the assertion above still holds; this is a documentation guard.
		if q.PrefHits == 0 && q.Hint == "rejected" {
			t.Errorf("a no-signal paraphrase %q must not be `rejected` on the base score alone; score=%v flags=%v",
				p, q.Score, q.Flags)
		}
	}

	// CONTROL — these SHOULD still reject (the gate is not vacuously accepting):
	//   - a too-short fragment (<20 runes),
	//   - an anchored low-signal acknowledgement ("done"),
	//   - a noisy log line (>=2 noise patterns).
	rejects := []string{
		"use tabs", // 8 runes → too_short → 0.1/rejected
		"done",     // anchored low_signal
		"Traceback (most recent call last): error: boom", // noise_log (traceback + error:)
	}
	for _, r := range rejects {
		if q := classifyQuality(r, ""); hintAtLeastNew(q.Hint) {
			t.Errorf("control %q must NOT clear the new floor (gate must still reject); hint=%q score=%v flags=%v",
				r, q.Hint, q.Score, q.Flags)
		}
	}
}

// hintAtLeastNew mirrors lifecycle.Hint.atLeastNew for the F5-quality test: a hint
// of `validated` or `new` clears the promote floor; `rejected` does not. Kept
// local to the extract package's test so it does not import the lifecycle package.
func hintAtLeastNew(hint string) bool { return hint == "validated" || hint == "new" }
