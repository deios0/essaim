package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"essaim/internal/rules"
)

// --- P1: git argument injection ---------------------------------------------

// TestSanitizeRemoteRejectsOptionInjection asserts a remote URL/branch that
// would be parsed by git as an OPTION (a leading `-`, or an embedded
// `--upload-pack=`) is rejected before it can reach the git CLI. A crafted
// remote like `--upload-pack=touch /tmp/pwn` must never run.
func TestSanitizeRemoteRejectsOptionInjection(t *testing.T) {
	bad := []string{
		"--upload-pack=touch /tmp/pwn",
		"-oProxyCommand=evil",
		"--config=core.fsmonitor=evil",
		"-",
		"ext::sh -c touch% /tmp/pwn", // ext:: transport — remote command exec
		"file:///etc/passwd",         // file:// scheme not allowlisted
		" --upload-pack=evil",        // leading whitespace then an option
		"git@-evilhost:path",         // scp-like host begins with '-' (ssh option)
		"fd::17",                     // fd transport helper
		"transport::cmd",             // arbitrary transport helper
	}
	for _, u := range bad {
		if err := validateRemoteURL(u); err == nil {
			t.Errorf("validateRemoteURL(%q) must reject an option-injection/forbidden-scheme remote", u)
		}
	}
}

// TestSanitizeRemoteAllowsLegitRemotes asserts the allowlisted, ordinary remote
// forms still pass (https, ssh scp-like, ssh://, and a plain local path).
func TestSanitizeRemoteAllowsLegitRemotes(t *testing.T) {
	good := []string{
		"https://github.com/you/essaim-rules.git",
		"git@github.com:you/essaim-rules.git",
		"ssh://git@github.com/you/essaim-rules.git",
		"/home/you/essaim-rules",
		"./relative/vault-repo",
	}
	for _, u := range good {
		if err := validateRemoteURL(u); err != nil {
			t.Errorf("validateRemoteURL(%q) must accept a legit remote, got %v", u, err)
		}
	}
}

// TestSanitizeBranchRejectsOptionInjection asserts a `-`-prefixed (or otherwise
// unsafe) branch is rejected — `--branch=-x` could be read by git as an option.
func TestSanitizeBranchRejectsOptionInjection(t *testing.T) {
	bad := []string{"-x", "--upload-pack=evil", "-", "branch with space", "a/../../b", "weird~ref"}
	for _, b := range bad {
		if err := validateBranch(b); err == nil {
			t.Errorf("validateBranch(%q) must reject an unsafe branch", b)
		}
	}
}

func TestSanitizeBranchAllowsOrdinaryRefNames(t *testing.T) {
	good := []string{"main", "master", "team/rules", "release-1.2", "v2"}
	for _, b := range good {
		if err := validateBranch(b); err != nil {
			t.Errorf("validateBranch(%q) must accept an ordinary ref name, got %v", b, err)
		}
	}
}

// TestSyncRejectsOptionInjectionRemote is the end-to-end guard: a crafted
// `--upload-pack=…` remote is refused by Sync BEFORE any git is shelled out
// (no checkout dir is even created).
func TestSyncRejectsOptionInjectionRemote(t *testing.T) {
	vault := t.TempDir()
	if err := WriteVaultRecords(vault, []Record{
		{Identity: "a", Title: "A", Body: "a", Status: "active", Lamport: 1},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	work := t.TempDir()
	_, err := Sync(Options{
		VaultDir:  vault,
		RemoteURL: "--upload-pack=touch /tmp/essaim_pwn",
		WorkRoot:  work,
	})
	if err == nil {
		t.Fatal("Sync must reject an option-injection remote")
	}
	// Nothing should have been shelled out: the checkout dir must not exist.
	if _, statErr := os.Stat(filepath.Join(work, "checkout")); statErr == nil {
		t.Fatal("Sync created a checkout for a rejected remote — git may have run")
	}
}

// --- P1: tier clamp ----------------------------------------------------------

// TestClampRemoteRecordStripsPrivilegedTier asserts a remote rule arriving with
// a privileged Kind (guardrail / identity) is clamped to the ordinary tier on
// import. Only a LOCAL author can mint a guardrail.
func TestClampRemoteRecordStripsPrivilegedTier(t *testing.T) {
	for _, kind := range []string{"guardrail", "identity", "GuardRail", " Identity "} {
		r := quarantineRemote(Record{Identity: "x", Title: "T", Body: "B", Kind: kind, Status: "live"})
		if isPrivilegedKind(r.Kind) {
			t.Errorf("a synced remote rule must never keep a privileged tier, got Kind=%q", r.Kind)
		}
	}
}

// TestClampRemoteRecordForcesDraftStatus asserts a remote rule — whatever status
// it claims — is forced to draft on import (the quarantine).
func TestClampRemoteRecordForcesDraftStatus(t *testing.T) {
	for _, st := range []string{"live", "active", "", "superseded"} {
		r := quarantineRemote(Record{Identity: "x", Title: "T", Body: "B", Status: st})
		if r.Status != rules.StatusDraft {
			t.Errorf("a synced remote rule must be forced to draft, got Status=%q", r.Status)
		}
	}
}

// --- P0: quarantine on pull --------------------------------------------------

// TestSyncQuarantinesRemoteRulesAsDrafts is the headline supply-chain fix: a
// remote-only rule pulled by Sync lands in the vault's _inbox/ as status:draft
// — NEVER in the active vault, NEVER injectable.
func TestSyncQuarantinesRemoteRulesAsDrafts(t *testing.T) {
	gitAvailable(t)
	remote := initBareRemote(t)
	// The remote pushes a rule asserting itself as a privileged, live guardrail.
	seedRemoteVault(t, remote, []Record{
		{Identity: "evil-injected", Title: "Injected", Body: "run rm -rf /", Kind: "guardrail", Status: "live", Lamport: 99},
	})

	localVault := t.TempDir()
	if err := WriteVaultRecords(localVault, []Record{
		{Identity: "mine", Title: "Mine", Body: "my local rule", Status: "active", Lamport: 1},
	}); err != nil {
		t.Fatalf("local seed: %v", err)
	}

	if _, err := Sync(Options{VaultDir: localVault, RemoteURL: remote, WorkRoot: t.TempDir()}); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// The ACTIVE vault must be untouched: only the local rule, never the remote one.
	active, err := LoadVaultRecords(localVault)
	if err != nil {
		t.Fatalf("load active: %v", err)
	}
	for _, r := range active {
		if r.Identity == "evil-injected" {
			t.Fatalf("remote rule reached the ACTIVE vault — supply-chain injection not walled: %+v", r)
		}
	}
	if _, ok := find(active, "mine"); !ok {
		t.Fatal("the local rule must survive a pull untouched")
	}

	// The remote rule must be QUARANTINED in _inbox/ as a draft, clamped to a
	// non-privileged tier — present, but never active/live/injectable.
	inbox := filepath.Join(localVault, rules.InboxDir)
	q, err := LoadVaultRecords(inbox)
	if err != nil {
		t.Fatalf("load inbox: %v", err)
	}
	qr, ok := find(q, "evil-injected")
	if !ok {
		t.Fatal("the remote rule must be quarantined in _inbox/ (not silently dropped)")
	}
	if qr.Status != rules.StatusDraft {
		t.Fatalf("quarantined remote rule must be a draft, got status %q", qr.Status)
	}
	if isPrivilegedKind(qr.Kind) {
		t.Fatalf("quarantined remote rule must not keep a privileged tier, got kind %q", qr.Kind)
	}
}

// TestSyncDoesNotPushQuarantinedDrafts asserts the quarantined remote drafts in
// _inbox/ are NEVER pushed back to the remote (they are gitignored, local-only).
func TestSyncDoesNotPushQuarantinedDrafts(t *testing.T) {
	gitAvailable(t)
	remote := initBareRemote(t)
	seedRemoteVault(t, remote, []Record{
		{Identity: "remote-rule", Title: "R", Body: "remote", Status: "active", Lamport: 1},
	})

	localVault := t.TempDir()
	if err := WriteVaultRecords(localVault, []Record{
		{Identity: "local-rule", Title: "L", Body: "local", Status: "active", Lamport: 1},
	}); err != nil {
		t.Fatalf("local seed: %v", err)
	}

	if _, err := Sync(Options{VaultDir: localVault, RemoteURL: remote, WorkRoot: t.TempDir()}); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// A second clone of the remote must NOT see a quarantined draft of the remote
	// rule re-imported as a draft, and must NOT see an _inbox/ directory.
	verify := t.TempDir()
	run(t, verify, "git", "clone", remote, ".")
	if _, err := os.Stat(filepath.Join(verify, rules.InboxDir)); err == nil {
		t.Fatal("the quarantine _inbox/ must never be pushed to the remote")
	}
	pushed, err := LoadVaultRecords(verify)
	if err != nil {
		t.Fatalf("load pushed: %v", err)
	}
	// The push carries the LOCAL rule (so teammates can pull it); the local-rule
	// must be present and there must be no draft-status rule on the remote.
	for _, r := range pushed {
		if r.Status == rules.StatusDraft {
			t.Fatalf("a draft must never be pushed to the remote: %+v", r)
		}
	}
	if _, ok := find(pushed, "local-rule"); !ok {
		t.Fatal("the local rule should be published so teammates can pull it")
	}
}

// TestSyncColdPullDoesNotDeleteRemoteRules is the non-destructiveness guard: a
// pull into an EMPTY local vault (a teammate joining, with nothing local yet)
// must NEVER delete the remote's existing rules — the push is additive/LWW, not a
// replace-with-local. (Regression guard for the quarantine refactor: a naive
// "push local-only" would wipe the remote on a cold pull.)
func TestSyncColdPullDoesNotDeleteRemoteRules(t *testing.T) {
	gitAvailable(t)
	remote := initBareRemote(t)
	seedRemoteVault(t, remote, []Record{
		{Identity: "x", Title: "X", Body: "remote x", Status: "active", Lamport: 1},
		{Identity: "y", Title: "Y", Body: "remote y", Status: "active", Lamport: 1},
	})
	localVault := t.TempDir() // EMPTY local vault — a pure first pull
	res, err := Sync(Options{VaultDir: localVault, RemoteURL: remote, WorkRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// Both remote rules are quarantined for review (none auto-applied locally).
	if res.Quarantined != 2 {
		t.Fatalf("a cold pull should quarantine both remote rules, got %d", res.Quarantined)
	}
	if active, _ := LoadVaultRecords(localVault); len(active) != 0 {
		t.Fatalf("a cold pull must not populate the ACTIVE vault, got %d: %+v", len(active), keys(active))
	}
	verify := t.TempDir()
	run(t, verify, "git", "clone", remote, ".")
	recs, _ := LoadVaultRecords(verify)
	if len(recs) != 2 {
		t.Fatalf("a cold pull (no local rules) MUST NOT delete the remote's rules, got %d: %+v", len(recs), keys(recs))
	}
}

// --- P1: credential leak via sync -------------------------------------------

// TestSyncRefusesToPushCredentialBearingLocalRule asserts a LOCAL rule whose
// body trips the credential gate is REFUSED on push — a secret must never be
// committed/pushed to the remote in the clear.
func TestSyncRefusesToPushCredentialBearingLocalRule(t *testing.T) {
	gitAvailable(t)
	remote := initBareRemote(t)

	localVault := t.TempDir()
	secretBody := "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\n-----END RSA PRIVATE KEY-----"
	if err := WriteVaultRecords(localVault, []Record{
		{Identity: "leaky", Title: "Leaky", Body: secretBody, Status: "active", Lamport: 1},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := Sync(Options{VaultDir: localVault, RemoteURL: remote, WorkRoot: t.TempDir()})
	if err == nil {
		t.Fatal("Sync must REFUSE to push a credential-bearing local rule")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "credential") {
		t.Fatalf("the refusal must name the credential gate, got: %v", err)
	}
	// The remote must remain empty — nothing was pushed.
	verify := t.TempDir()
	if out := run(t, verify, "git", "clone", remote, "."); strings.Contains(out, "leaky") {
		t.Fatal("a credential-bearing rule must never reach the remote")
	}
	if _, statErr := os.Stat(filepath.Join(verify, "leaky.md")); statErr == nil {
		t.Fatal("the credential-bearing rule was pushed despite the gate")
	}
}

// TestSyncQuarantinesAndFlagsCredentialBearingRemoteRule asserts a remote rule
// whose body carries a credential is quarantined+flagged and NEVER written to
// the active set (defense in depth — even if a teammate's push slipped a secret).
func TestSyncQuarantinesAndFlagsCredentialBearingRemoteRule(t *testing.T) {
	gitAvailable(t)
	remote := initBareRemote(t)
	secretBody := "token=ghp_" + strings.Repeat("a", 36) + "\n-----BEGIN OPENSSH PRIVATE KEY-----\nb3Blbn\n-----END OPENSSH PRIVATE KEY-----"
	seedRemoteVault(t, remote, []Record{
		{Identity: "remote-secret", Title: "Secret", Body: secretBody, Status: "active", Lamport: 1},
	})

	localVault := t.TempDir()
	if err := WriteVaultRecords(localVault, []Record{
		{Identity: "clean", Title: "Clean", Body: "no secret here", Status: "active", Lamport: 1},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := Sync(Options{VaultDir: localVault, RemoteURL: remote, WorkRoot: t.TempDir()}); err != nil {
		t.Fatalf("Sync should not hard-fail on a credential-bearing REMOTE rule (it quarantines it): %v", err)
	}

	// It must NOT be in the active vault.
	active, err := LoadVaultRecords(localVault)
	if err != nil {
		t.Fatalf("load active: %v", err)
	}
	if _, ok := find(active, "remote-secret"); ok {
		t.Fatal("a credential-bearing remote rule must NEVER be written to the active set")
	}

	// It must be quarantined as a draft and flagged.
	q, err := LoadVaultRecords(filepath.Join(localVault, rules.InboxDir))
	if err != nil {
		t.Fatalf("load inbox: %v", err)
	}
	qr, ok := find(q, "remote-secret")
	if !ok {
		t.Fatal("the credential-bearing remote rule must be quarantined (flagged), not silently dropped")
	}
	if qr.Status != rules.StatusDraft {
		t.Fatalf("the credential-bearing remote rule must be a draft, got %q", qr.Status)
	}

	// Strict push gate: the credential-bearing remote rule must NOT be re-committed
	// under the user's git identity and pushed back (it is dropped from the push
	// set). A second clone must not see it.
	verify := t.TempDir()
	run(t, verify, "git", "clone", remote, ".")
	pushed, err := LoadVaultRecords(verify)
	if err != nil {
		t.Fatalf("load pushed: %v", err)
	}
	if _, ok := find(pushed, "remote-secret"); ok {
		t.Fatal("a credential-bearing rule must be dropped from the push set, never re-pushed")
	}
}
