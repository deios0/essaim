package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"oikos/internal/extract"
	"oikos/internal/rules"
)

// P2-5: a credential-bearing REMOTE rule must NEVER land in the local _inbox/ in
// plaintext. The git.go comment claimed quarantine "redacts or flags" the secret,
// but quarantineRemote only clamped status/kind + set RemoteOrigin — the secret
// stayed verbatim in the draft body. Fix: quarantine redacts the credential from
// the durable content AND stamps a clear flag, reusing the extract redaction
// helpers so a remote secret is never persisted in the clear.
func TestQuarantineRedactsAndFlagsCredential(t *testing.T) {
	secret := "sk-proj-abcdefABCDEF1234567890abcdefABCDEF1234567890Tz"
	in := Record{
		Identity: "leaky",
		Title:    "Use this API key",
		Body:     "Always call the vendor API with key " + secret + " for auth.",
		Kind:     "guardrail",
		Status:   "live",
	}
	// Precondition: the input really carries a credential.
	if !extract.ContainsCredential(in.Body) {
		t.Fatal("test fixture must carry a credential")
	}

	q := quarantineRemote(in)

	// The redacted record must no longer contain the secret anywhere durable.
	if strings.Contains(q.Body, secret) || strings.Contains(q.Title, secret) {
		t.Fatalf("quarantine must redact the credential from the body/title; body=%q title=%q", q.Body, q.Title)
	}
	if extract.ContainsCredential(q.Body) || extract.ContainsCredential(q.Title) {
		t.Fatalf("quarantined record must not trip the credential gate after redaction; body=%q", q.Body)
	}
	// And it must be flagged so a human reviewer sees a secret was stripped.
	if !q.CredentialFlagged {
		t.Fatal("quarantine must flag a credential-bearing remote rule")
	}
	// Standard quarantine invariants still hold.
	if q.Status != rules.StatusDraft || !q.RemoteOrigin {
		t.Fatalf("quarantine invariants broken: status=%q remoteOrigin=%v", q.Status, q.RemoteOrigin)
	}
}

// End-to-end through the real write path: the plaintext secret must not appear on
// disk, and the flag + [REDACTED] marker must be present in the written draft.
func TestQuarantineCredentialNeverWrittenPlaintext(t *testing.T) {
	secret := "ya29.a0AfB_byC1234567890abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOP-_"
	q := quarantineRemote(Record{
		Identity: "leaky2",
		Title:    "creds",
		Body:     "the token is " + secret + " keep it",
		Kind:     "rule",
		Status:   "live",
	})
	doc := RenderRecord(q)
	if strings.Contains(doc, secret) {
		t.Fatalf("the rendered draft must NEVER contain the plaintext secret:\n%s", doc)
	}
	if !strings.Contains(doc, "[REDACTED]") {
		t.Fatalf("the redacted body must carry a [REDACTED] marker:\n%s", doc)
	}
	if !strings.Contains(doc, "credential_redacted: true") {
		t.Fatalf("the draft must carry a credential_redacted flag:\n%s", doc)
	}

	// Write it to disk the way the sync does and confirm the file is clean.
	dir := t.TempDir()
	if err := WriteVaultRecords(dir, []Record{q}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected one draft file, got %d", len(entries))
	}
	raw, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if strings.Contains(string(raw), secret) {
		t.Fatalf("on-disk draft leaked the plaintext secret:\n%s", raw)
	}
}

// A clean (non-credential) remote rule is quarantined UNCHANGED (no spurious
// flag, body preserved verbatim) — the redaction only touches leaky rules.
func TestQuarantineCleanRuleUnchanged(t *testing.T) {
	in := Record{
		Identity: "clean",
		Title:    "Prefer Postgres",
		Body:     "Use PostgreSQL, never MySQL, for the main store.",
		Kind:     "rule",
		Status:   "live",
	}
	q := quarantineRemote(in)
	if q.CredentialFlagged {
		t.Fatal("a clean rule must not be credential-flagged")
	}
	if q.Body != in.Body || q.Title != in.Title {
		t.Fatalf("a clean rule's content must be preserved verbatim; body=%q title=%q", q.Body, q.Title)
	}
	if strings.Contains(RenderRecord(q), "credential_redacted") {
		t.Fatal("a clean rule's draft must not carry the credential_redacted flag")
	}
}
