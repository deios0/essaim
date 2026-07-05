package extract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SECURITY (gemini review): a credential-bearing /remember sigil must be REJECTED
// on EVERY send — including a RESEND (IDE resends conversation history). The
// multi-sigil rewrite added the credential sigil to the seen-set, so a resend hit
// the duplicate-skip, produced a "skipped" (not "rejected") result, and Process
// fell THROUGH to T1 with the raw credential text — leaking the secret. The fix
// checks credentials BEFORE the seen-set and never records a credential sigil.
func TestCredentialSigilRejectedOnResend(t *testing.T) {
	dir := t.TempDir()
	e := New(dir, Config{})
	line := "/remember my aws secret access key is AKIAIOSFODNN7EXAMPLE wjalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY"
	ex := Exchange{UserText: line, NewUserLines: []string{line}}

	// First send: rejected, T1 skipped.
	r1 := e.Process(ex)
	if r1.Status != "rejected" {
		t.Fatalf("first send of a credential sigil must be rejected; got %+v", r1)
	}
	// Resend (same line, now in the seen-set): must STILL be rejected and NOT fall
	// through to T1.
	r2 := e.Process(ex)
	if r2.Status != "rejected" {
		t.Fatalf("SECURITY: resent credential sigil must STILL be rejected (no T1 leak); got %+v", r2)
	}
	if r2.Tier == "T1" {
		t.Fatalf("SECURITY: resent credential sigil reached T1 — credential leak; got %+v", r2)
	}
	// And nothing credential-bearing was ever written to the vault.
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, _ error) error {
		if info == nil || info.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), "AKIAIOSFODNN7EXAMPLE") {
			t.Fatalf("SECURITY: credential persisted to %s", p)
		}
		return nil
	})
}
