package sync

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"oikos/internal/rules"
)

// Options configures one `oikos sync` run. Everything is local + BYO-storage:
// the only network egress is to the user's OWN git RemoteURL — there is no oikos
// server, no phone-home. The whole feature is OPTIONAL and $0.
type Options struct {
	// VaultDir is the local Markdown vault (the source of truth) to sync.
	VaultDir string
	// RemoteURL is the user's git remote (https://…, git@…, or a local path).
	RemoteURL string
	// Branch is the remote branch (default "main").
	Branch string
	// WorkRoot is where the transient checkout is created (default: a temp dir
	// removed after the run). Injectable for tests.
	WorkRoot string
	// Message is the commit message (default "oikos sync: rule vault").
	Message string
}

// Result reports what a Sync did.
type Result struct {
	// Merged is the number of rules in the user's published (local active) set.
	Merged int
	// Quarantined is the number of incoming REMOTE rules placed in _inbox/ as
	// drafts awaiting the local user's explicit review/accept (P0 quarantine).
	Quarantined int
	// Pushed reports whether a commit was pushed (false on a no-divergence sync).
	Pushed bool
}

// Sync pushes/pulls the vault to the user's git remote and merges deterministically.
//
// Flow (all local + the user's own remote — no oikos infra):
//  1. clone/fetch RemoteURL into a transient checkout
//  2. load the remote vault + the local vault as Records
//  3. Merge them (deterministic LWW, NO rule lost — see merge.go)
//  4. write the merged union back to BOTH the local vault and the checkout
//  5. commit + push the checkout (skipped cleanly if nothing changed)
//
// This is the SCAFFOLD, not the paid Team-Sync: the merge is a per-record
// last-writer-wins on a Lamport clock, with a clean seam (Merge's contract) for
// a future CRDT/Team-Sync layer to slot in without touching the transport.
func Sync(opt Options) (Result, error) {
	if strings.TrimSpace(opt.VaultDir) == "" {
		return Result{}, errors.New("oikos sync: a vault directory is required (set one in /setup or OIKOS_VAULT)")
	}
	if strings.TrimSpace(opt.RemoteURL) == "" {
		return Result{}, errors.New("oikos sync: a git remote is required (e.g. `oikos sync --remote git@github.com:you/rules.git`)")
	}
	branch := opt.Branch
	if branch == "" {
		branch = "main"
	}
	msg := opt.Message
	if msg == "" {
		msg = "oikos sync: rule vault"
	}

	// P1 — git argument / transport injection: validate the user-supplied remote
	// URL + branch BEFORE any git is shelled out. A crafted `--upload-pack=…`,
	// `-`-prefixed, or `ext::`/`file://` remote (or a `-`/whitespace branch) is
	// rejected here so it can never reach the git CLI. We validate FIRST so a
	// rejected remote never even creates a checkout dir.
	if err := validateRemoteURL(opt.RemoteURL); err != nil {
		return Result{}, err
	}
	if err := validateBranch(branch); err != nil {
		return Result{}, err
	}

	// Transient checkout dir. When WorkRoot is unset we create (and clean up) a
	// temp dir; an injected WorkRoot is left in place for inspection/tests.
	workRoot := opt.WorkRoot
	cleanup := func() {}
	if workRoot == "" {
		tmp, err := os.MkdirTemp("", "oikos-sync-*")
		if err != nil {
			return Result{}, err
		}
		workRoot = tmp
		cleanup = func() { _ = os.RemoveAll(tmp) }
	}
	defer cleanup()
	checkout := filepath.Join(workRoot, "checkout")

	g := gitRunner{}

	// 1. Get a checkout of the remote branch. A fresh clone is simplest and
	//    correct for a small rule vault; an empty remote yields an empty checkout.
	hadRemoteContent, err := g.cloneOrInit(opt.RemoteURL, branch, checkout)
	if err != nil {
		return Result{}, err
	}

	// 2. Load both sides SEPARATELY. The local vault is the user's own ACCEPTED
	//    rules (the source of truth for what they publish); the remote vault is
	//    UNTRUSTED inbound (a teammate's push, possibly malicious/compromised).
	var remoteRecs []Record
	if hadRemoteContent {
		remoteRecs, err = LoadVaultRecords(checkout)
		if err != nil {
			return Result{}, err
		}
	}
	localRecs, err := LoadVaultRecords(opt.VaultDir)
	if err != nil {
		return Result{}, err
	}

	// 3. P1 CREDENTIAL GATE (push side): refuse to sync a LOCAL rule whose body
	//    trips the credential check — a secret must never be committed/pushed to a
	//    remote in the clear. We fail the WHOLE sync (loud, before any push) so the
	//    user fixes the leak rather than shipping it.
	if leaky, ok := firstCredentialBearing(localRecs); ok {
		return Result{}, fmt.Errorf(
			"oikos sync: refusing to push rule %q — its content trips the credential gate "+
				"(a secret must never be synced in the clear; remove the credential and retry)",
			leaky.Key())
	}

	// 4. P0 QUARANTINE ON PULL: every incoming REMOTE rule lands in the vault's
	//    _inbox/ as a status:draft (clamped to a non-privileged tier), NEVER in the
	//    active/injectable set. The local user explicitly accepts a draft later via
	//    the M3 promote mechanism. This walls the in-team supply-chain injection:
	//    a pull can never auto-apply a remote rule into the live index. The ACTIVE
	//    vault is left UNTOUCHED by the pull — local rules are unaffected.
	//
	//    Defense in depth: a credential-bearing REMOTE rule is quarantined with its
	//    secret REDACTED and FLAGGED (credential_redacted: true) by quarantineRemote
	//    (P2-5) — never written to the active set, and never persisted plaintext in
	//    _inbox/.
	quarantined := quarantineIncoming(remoteRecs, localRecs)
	if len(quarantined) > 0 {
		inbox, ierr := rules.EnsureInboxDir(opt.VaultDir)
		if ierr != nil {
			return Result{}, ierr
		}
		if werr := WriteVaultRecords(inbox, quarantined); werr != nil {
			return Result{}, werr
		}
	}

	// 5. The PUSH carries the deterministic union of the user's LOCAL rules layered
	//    over what is already on the remote (Merge — same lossless, LWW contract as
	//    before). This is NON-DESTRUCTIVE: a teammate's remote rule the user has not
	//    accepted is PRESERVED on the remote (a cold pull never deletes the remote's
	//    rules), while the user's own edits are published so teammates can pull them.
	//    Crucially the union is written ONLY to the pushed checkout — NEVER to the
	//    local active vault (the quarantine wall in step 4 already handled inbound),
	//    so re-publishing the union cannot auto-apply a remote rule onto THIS box.
	//
	//    P1 CREDENTIAL GATE (push set, strict): the credential gate is applied to
	//    the ENTIRE published union, not just the user's own rules — a credential
	//    that arrived in a remote rule is DROPPED from the push so this machine never
	//    re-commits a secret under the user's git identity. (Local credential-bearing
	//    rules already hard-failed the whole sync in step 3; this drops the
	//    remote-origin case, which is quarantined locally regardless.)
	toPublish := dropCredentialBearing(Merge(localRecs, remoteRecs))
	if err := replaceVaultFiles(checkout, toPublish); err != nil {
		return Result{}, err
	}

	// 6. Commit + push only if the checkout actually changed (no empty commits).
	changed, err := g.commitIfDirty(checkout, branch, msg)
	if err != nil {
		return Result{}, err
	}
	pushed := false
	if changed {
		if err := g.push(checkout, branch); err != nil {
			return Result{}, err
		}
		pushed = true
	}

	// Merged reports the size of the published union; the quarantined inbound is
	// reported separately so it is never conflated with "applied" (active) rules.
	return Result{Merged: len(toPublish), Quarantined: len(quarantined), Pushed: pushed}, nil
}

// quarantineIncoming returns the REMOTE records that should be quarantined as
// drafts in _inbox/, each clamped to draft status + a non-privileged tier. A
// remote record is quarantined when it is NEW relative to the local active set
// OR differs in content from the local copy (a remote EDIT of a local rule is
// still inbound-to-review, never an auto-apply). A remote rule whose content is
// byte-identical to an already-accepted local rule is dropped (nothing to
// review). The local active set is the trust anchor and is never modified here.
func quarantineIncoming(remote, local []Record) []Record {
	localByKey := make(map[string]Record, len(local))
	for _, r := range local {
		localByKey[r.Key()] = r
	}
	var out []Record
	for _, r := range remote {
		if cur, ok := localByKey[r.Key()]; ok && cur.ContentID() == r.ContentID() {
			// Already accepted locally with identical content — nothing to review.
			continue
		}
		out = append(out, quarantineRemote(r))
	}
	return out
}

// replaceVaultFiles makes the checkout's tracked `.md` rule set exactly `recs`:
// it removes existing top-level `.md` files (so a rule deleted everywhere does
// not resurrect) then writes the merged union. Non-`.md` files (README, .git)
// are left untouched.
func replaceVaultFiles(dir string, recs []Record) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return WriteVaultRecords(dir, recs)
}

// gitRunner wraps the git CLI. Splitting it out keeps the git invocations in one
// place and makes the egress surface obvious (only `git` to the user's remote).
type gitRunner struct{}

func (gitRunner) run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// Isolate from the user's global/system git config so behavior is
	// deterministic; supply a default identity so commits never fail on a box
	// with no configured user.name/email.
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=oikos",
		"GIT_AUTHOR_EMAIL=oikos@localhost",
		"GIT_COMMITTER_NAME=oikos",
		"GIT_COMMITTER_EMAIL=oikos@localhost",
		"GIT_TERMINAL_PROMPT=0", // never block on an interactive credential prompt
	)
	// P2: GIT_TERMINAL_PROMPT only stops GIT's own prompts — an SSH remote can still
	// block the daemon on an interactive host-key or passphrase prompt. Set
	// non-interactive ssh options, but ONLY if the user has not set their own
	// GIT_SSH_COMMAND (which may carry `-i <key>`, ProxyCommand, a custom port, …) —
	// clobbering it would break their private-remote auth (codex review). We compose
	// onto a user command when present, else use a bare non-interactive ssh.
	sshOpts := "-o BatchMode=yes -o StrictHostKeyChecking=accept-new"
	if userSSH := os.Getenv("GIT_SSH_COMMAND"); userSSH != "" {
		cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND="+userSSH+" "+sshOpts)
	} else {
		cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND=ssh "+sshOpts)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String(), nil
}

// cloneOrInit clones the remote branch into checkout. If the remote has no
// commits yet (cold start), it initializes an empty checkout wired to the remote
// and returns hadRemoteContent=false.
func (g gitRunner) cloneOrInit(remoteURL, branch, checkout string) (bool, error) {
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		return false, err
	}
	// Try a normal clone of the branch first. The `--` end-of-options separator
	// (defense in depth atop validateRemoteURL/validateBranch) guarantees the
	// remote URL and "." positional can never be reparsed as git options even if a
	// future edit weakens the validators. `--branch=` uses the `=` form so the
	// branch value can never be split off as a bare option-looking token.
	if _, err := g.run(checkout, "clone", "--branch="+branch, "--", remoteURL, "."); err == nil {
		return true, nil
	}
	// The branch (or any commit) may not exist yet — set up an empty repo bound
	// to the remote so the first commit can be pushed to create it.
	if _, err := g.run(checkout, "init", "-b", branch); err != nil {
		return false, err
	}
	if _, err := g.run(checkout, "remote", "add", "origin", "--", remoteURL); err != nil {
		return false, err
	}
	return false, nil
}

// commitIfDirty stages all changes and commits when the working tree is dirty.
// It returns changed=false (no commit) when there is nothing to commit, so a
// no-divergence sync produces no empty commit and no push.
func (g gitRunner) commitIfDirty(checkout, branch, msg string) (bool, error) {
	if _, err := g.run(checkout, "add", "-A"); err != nil {
		return false, err
	}
	// `git status --porcelain` prints nothing on a clean tree.
	status, err := g.run(checkout, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(status) == "" {
		return false, nil
	}
	if _, err := g.run(checkout, "commit", "-m", msg); err != nil {
		return false, err
	}
	return true, nil
}

// push publishes the branch to origin. The `--` end-of-options separator ensures
// the (already-validated) branch name can never be reparsed as a push option.
func (g gitRunner) push(checkout, branch string) error {
	_, err := g.run(checkout, "push", "origin", "--", branch)
	return err
}
