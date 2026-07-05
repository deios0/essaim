package sync

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitAvailable reports whether a real git binary is on PATH.
func gitAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available; skipping git-transport integration test")
	}
}

// initBareRemote creates a bare git repo to act as the user's BYO remote.
func initBareRemote(t *testing.T) string {
	t.Helper()
	remote := t.TempDir()
	run(t, remote, "git", "init", "--bare", "-b", "main")
	return remote
}

// run executes a command in dir and fails the test on error.
func run(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed in %s: %v\n%s", name, strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

// seedRemoteVault publishes an initial vault to the bare remote (so the first
// pull has content), via a throwaway clone.
func seedRemoteVault(t *testing.T, remote string, recs []Record) {
	t.Helper()
	work := t.TempDir()
	run(t, work, "git", "clone", remote, ".")
	if err := WriteVaultRecords(work, recs); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	run(t, work, "git", "add", "-A")
	run(t, work, "git", "commit", "-m", "seed")
	run(t, work, "git", "push", "origin", "main")
}

// TestSyncPullQuarantinesRemoteAndPublishesLocal is the headline git-transport
// scenario under the SECURITY CONTRACT (security review P0): a local vault and a
// remote vault diverge; `Sync`
//   - PUBLISHES the user's own local rules to the remote (so teammates can pull),
//   - QUARANTINES every incoming remote rule in _inbox/ as a status:draft (never
//     auto-applied into the active/injectable vault),
//   - leaves the ACTIVE vault's local rules UNTOUCHED.
//
// The old behavior (remote rules merged straight into the live vault) was the
// in-team supply-chain injection this fix closes.
func TestSyncPullQuarantinesRemoteAndPublishesLocal(t *testing.T) {
	gitAvailable(t)

	remote := initBareRemote(t)
	// Remote starts with rules {a@1, c@1} — a is also edited locally, c is remote-only.
	seedRemoteVault(t, remote, []Record{
		{Identity: "a", Title: "A", Body: "remote a", Status: "active", Lamport: 1},
		{Identity: "c", Title: "C", Body: "remote c", Status: "active", Lamport: 1},
	})

	// Local vault has {a@5 (newer edit), b@1 (local-only)}.
	localVault := t.TempDir()
	if err := WriteVaultRecords(localVault, []Record{
		{Identity: "a", Title: "A", Body: "local a NEWER", Status: "active", Lamport: 5},
		{Identity: "b", Title: "B", Body: "local b", Status: "active", Lamport: 1},
	}); err != nil {
		t.Fatalf("local seed: %v", err)
	}

	opt := Options{
		VaultDir:  localVault,
		RemoteURL: remote,
		WorkRoot:  t.TempDir(),
		Message:   "oikos sync",
	}
	res, err := Sync(opt)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.Pushed != true {
		t.Fatalf("expected a push (local rules published), got result %+v", res)
	}

	// The ACTIVE local vault is UNTOUCHED: still exactly the user's {a (local edit),
	// b}. The remote a and remote-only c did NOT auto-apply into the active set.
	local, err := LoadVaultRecords(localVault)
	if err != nil {
		t.Fatalf("reload local: %v", err)
	}
	if len(local) != 2 {
		t.Fatalf("active vault must stay the user's 2 local rules, got %d: %+v", len(local), keys(local))
	}
	a, _ := find(local, "a")
	if !strings.Contains(a.Body, "local a NEWER") {
		t.Fatalf("the local edit of a must be preserved untouched, got %q", a.Body)
	}
	if _, ok := find(local, "c"); ok {
		t.Fatal("remote-only rule c must NOT auto-apply into the active vault")
	}

	// The incoming remote rules (the remote edit of a AND remote-only c) are
	// QUARANTINED as drafts in _inbox/, awaiting explicit accept.
	q, err := LoadVaultRecords(filepath.Join(localVault, "_inbox"))
	if err != nil {
		t.Fatalf("load inbox: %v", err)
	}
	if _, ok := find(q, "c"); !ok {
		t.Fatal("remote-only rule c must be quarantined in _inbox/ as a draft")
	}
	qa, ok := find(q, "a")
	if !ok {
		t.Fatal("the remote EDIT of a must be quarantined (inbound-to-review), not auto-merged")
	}
	if qa.Status != "draft" {
		t.Fatalf("quarantined remote a must be a draft, got status %q", qa.Status)
	}
	if res.Quarantined != 2 {
		t.Fatalf("expected 2 quarantined inbound rules (remote a + c), got %d", res.Quarantined)
	}

	// A SECOND machine that clones the remote sees the NON-DESTRUCTIVE published
	// union: the user's local edits layered over the remote's rules ({a (local edit
	// wins by Lamport), b (local-only), c (remote preserved)}). The push never
	// deletes a teammate's rule, and never pushes a quarantined draft (the _inbox/
	// is gitignored and is not part of the published set). No draft reaches the
	// remote.
	verify := t.TempDir()
	run(t, verify, "git", "clone", remote, ".")
	remoteRecs, err := LoadVaultRecords(verify)
	if err != nil {
		t.Fatalf("load pushed remote: %v", err)
	}
	if len(remoteRecs) != 3 {
		t.Fatalf("remote should carry the non-destructive union {a,b,c}, got %d: %+v", len(remoteRecs), keys(remoteRecs))
	}
	for _, r := range remoteRecs {
		if r.Status == "draft" {
			t.Fatalf("a quarantined draft must never be pushed to the remote: %+v", r)
		}
	}
	pa, _ := find(remoteRecs, "a")
	if !strings.Contains(pa.Body, "local a NEWER") {
		t.Fatalf("the pushed a must be the user's local edit (Lamport-5 winner), got %q", pa.Body)
	}
	if _, ok := find(remoteRecs, "c"); !ok {
		t.Fatal("the remote-only rule c must be PRESERVED on the remote (push is non-destructive)")
	}
}

// TestSyncFirstPushToEmptyRemote covers the cold-start case: the remote has no
// commits yet; Sync must publish the local vault without crashing on an empty
// pull.
func TestSyncFirstPushToEmptyRemote(t *testing.T) {
	gitAvailable(t)

	remote := initBareRemote(t) // empty, no commits
	localVault := t.TempDir()
	if err := WriteVaultRecords(localVault, []Record{
		{Identity: "only", Title: "Only", Body: "the only rule", Status: "active", Lamport: 1},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := Sync(Options{
		VaultDir:  localVault,
		RemoteURL: remote,
		WorkRoot:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Sync to empty remote: %v", err)
	}
	if !res.Pushed {
		t.Fatalf("first sync should push, got %+v", res)
	}
	verify := t.TempDir()
	run(t, verify, "git", "clone", remote, ".")
	if _, err := os.Stat(filepath.Join(verify, "only.md")); err != nil {
		t.Fatalf("remote should contain only.md after first push: %v", err)
	}
}

// TestSyncNoChangeDoesNotPush asserts a no-op sync (local == remote) does not
// push (clean seam: nothing to do, no empty commit).
func TestSyncNoChangeDoesNotPush(t *testing.T) {
	gitAvailable(t)

	remote := initBareRemote(t)
	recs := []Record{{Identity: "a", Title: "A", Body: "a", Status: "active", Lamport: 1}}
	seedRemoteVault(t, remote, recs)

	localVault := t.TempDir()
	if err := WriteVaultRecords(localVault, recs); err != nil {
		t.Fatalf("seed local: %v", err)
	}

	res, err := Sync(Options{VaultDir: localVault, RemoteURL: remote, WorkRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.Pushed {
		t.Fatalf("a no-divergence sync must not push (no empty commit), got %+v", res)
	}
}

// TestSyncRequiresVaultAndRemote asserts the guard rails on missing config.
func TestSyncRequiresVaultAndRemote(t *testing.T) {
	if _, err := Sync(Options{RemoteURL: "x"}); err == nil {
		t.Fatal("Sync without a VaultDir must error")
	}
	if _, err := Sync(Options{VaultDir: "/x"}); err == nil {
		t.Fatal("Sync without a RemoteURL must error")
	}
}
