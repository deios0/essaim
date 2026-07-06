package capture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"essaim/internal/extract"
)

// P1-b [CREDENTIAL LEAK — TOP PRIORITY]: a "white" local binary must NEVER
// persist a secret. The leak path: capture.Redact() runs the credentialPattern
// over each flattened message IN PLACE; a PEM private key was matched only by its
// single-line BEGIN header, so Redact wiped the header to [REDACTED] but the
// base64 KEY BODY + the END line SURVIVED. The redacted text then no longer
// matched credentialPattern, so the extractor's classifyQuality did NOT
// hard-reject it, and the surviving body was written to an _inbox/<id>.md draft.
//
// This test drives the REAL seam end-to-end:
//
//	capture.Capture{...} → Redact() → ToExchange() → Extractor.Process() → writeDraft
//
// and asserts, for BOTH a full multi-line OpenSSH private key AND a PGP private
// key block:
//  1. the base64 KEY BODY string is absent from EVERY persisted output (no .md),
//  2. NO draft file is written anywhere in the vault,
//  3. ContainsCredential is true on the WHOLE block (header+body+footer).
func TestP1bPrivateKeyNeverPersistedThroughRealSeam(t *testing.T) {
	// A realistic multi-line OpenSSH private key: BEGIN header, a base64 body,
	// END footer. The base64 body is a unique sentinel we assert never escapes.
	const openSSHBody = "b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAA"
	openSSHKey := "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
		openSSHBody + "\n" +
		"AAAAC3NzaC1lZDI1NTE5AAAAIE4Tg9p1QzBexampleKeyBodyLine2plusmore\n" +
		"-----END OPENSSH PRIVATE KEY-----"

	// A PGP private key block — note the word BLOCK AFTER KEY (the prior pattern
	// missed this entirely).
	const pgpBody = "lQOYBGABCDEFGHIJKLMNOPpgpprivatekeybodybase64sentinel1234567890"
	pgpKey := "-----BEGIN PGP PRIVATE KEY BLOCK-----\n" +
		"\n" +
		pgpBody + "\n" +
		"-----END PGP PRIVATE KEY BLOCK-----"

	cases := []struct {
		name     string
		key      string
		bodySent string
	}{
		{"openssh", openSSHKey, openSSHBody},
		{"pgp", pgpKey, pgpBody},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// (3) ContainsCredential must be true on the WHOLE block.
			if !extract.ContainsCredential(tc.key) {
				t.Fatalf("ContainsCredential(whole %s block) = false, want true", tc.name)
			}

			vault := t.TempDir()
			e := extract.New(vault, extract.Config{})

			// Build the capture exactly as the server tap does: the user pasted the
			// key into a correction, with a preference-shaped wrapper so the T1 gate
			// would otherwise STAGE it (this is what makes the leak reachable — a
			// "prefer/always" body clears the score+pref gate).
			userMsg := "always prefer this deploy key, you must remember it because the CEO said so:\n" + tc.key
			c := Capture{
				OriginalMessages: []ChatMessage{
					{Role: "user", Content: userMsg},
				},
				AssistantText: "Understood, I'll use that key. " + tc.key,
			}

			// REAL SEAM step 1: Redact in place (as capture_tap does before enqueue).
			c.Redact()

			// After Redact, the secret body must already be gone from the in-memory
			// capture (the whole-block match removes header+body+footer).
			for i, m := range c.OriginalMessages {
				if strings.Contains(m.Content, tc.bodySent) {
					t.Fatalf("after Redact, OriginalMessages[%d] still contains the key body sentinel:\n%s", i, m.Content)
				}
			}
			if strings.Contains(c.AssistantText, tc.bodySent) {
				t.Fatalf("after Redact, AssistantText still contains the key body sentinel:\n%s", c.AssistantText)
			}

			// REAL SEAM step 2: the server tap drops a key-bearing capture
			// (ViolatesHardInvariant) — it is NEVER enqueued, NEVER learned from. This
			// is the whole-message-drop that closes the leak even though Redact left a
			// learnable "always prefer this deploy key" preference behind. In the real
			// flow Process is therefore never reached for this capture.
			if !c.ViolatesHardInvariant() {
				t.Fatalf("a private-key-bearing capture must be whole-message-dropped (ViolatesHardInvariant=true)")
			}

			// DEFENSE IN DEPTH: even if a buggy caller SKIPPED Redact and enqueued the
			// raw, un-redacted exchange, the extractor must HARD-REJECT it (the whole-
			// block credentialPattern / marker hard-reject in classifyQuality) and
			// write NOTHING. Feed the un-redacted key straight to a fresh extractor.
			raw := Capture{OriginalMessages: []ChatMessage{{Role: "user", Content: userMsg}}, AssistantText: "ok"}
			res := e.Process(raw.ToExchange())
			if res.WrotePath != "" {
				doc, _ := os.ReadFile(res.WrotePath)
				t.Fatalf("un-redacted private-key exchange must be hard-rejected (write nothing); wrote %s:\n%s", res.WrotePath, doc)
			}

			// (1)+(2): assert NO .md file exists anywhere in the vault AND the body
			// sentinel appears in NONE of the persisted bytes (both seam + defense).
			var mdFiles []string
			_ = filepath.WalkDir(vault, func(p string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil
				}
				if strings.HasSuffix(p, ".md") {
					mdFiles = append(mdFiles, p)
				}
				b, _ := os.ReadFile(p)
				if strings.Contains(string(b), tc.bodySent) {
					t.Fatalf("key body sentinel leaked into persisted file %s:\n%s", p, b)
				}
				return nil
			})
			if len(mdFiles) != 0 {
				t.Fatalf("no draft .md must be written for a private-key exchange; found %v", mdFiles)
			}
		})
	}
}

// P1-b (unit, belt): RedactCredentials must remove the WHOLE multi-line private
// key block (header + base64 body + footer), not just the BEGIN header line. The
// prior per-line pattern left the body+END line behind, which then dodged the
// hard-reject. This pins whole-block redaction directly.
func TestP1bRedactRemovesWholePrivateKeyBlock(t *testing.T) {
	const body = "MIIEpQIBAAKCAQEAbase64keybodyuniquesentinel0123456789abcdef"
	in := "here is the key:\n-----BEGIN RSA PRIVATE KEY-----\n" + body +
		"\nmoreBase64BodyLineTwo+slashes/==\n-----END RSA PRIVATE KEY-----\nthanks"
	out := extract.RedactCredentials(in)
	if strings.Contains(out, body) {
		t.Fatalf("RedactCredentials left the key body behind:\n%s", out)
	}
	if strings.Contains(out, "-----END RSA PRIVATE KEY-----") {
		t.Fatalf("RedactCredentials left the END footer line behind:\n%s", out)
	}
	if strings.Contains(out, "-----BEGIN RSA PRIVATE KEY-----") {
		t.Fatalf("RedactCredentials left the BEGIN header line behind:\n%s", out)
	}
	// Surrounding prose must survive.
	if !strings.Contains(out, "here is the key:") || !strings.Contains(out, "thanks") {
		t.Fatalf("redaction must preserve surrounding prose, got:\n%s", out)
	}
}
