package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	syncpkg "essaim/internal/sync"
)

func gitOrSkip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

func TestRunSyncRequiresRemote(t *testing.T) {
	// No --remote and no ESSAIM_SYNC_REMOTE → a clear error.
	t.Setenv("ESSAIM_SYNC_REMOTE", "")
	os.Unsetenv("ESSAIM_SYNC_REMOTE")
	var out bytes.Buffer
	if err := runSync([]string{"--vault", t.TempDir()}, &out); err == nil {
		t.Fatal("runSync without a remote must error")
	}
}

func TestRunSyncRequiresVault(t *testing.T) {
	t.Setenv("ESSAIM_VAULT", "")
	os.Unsetenv("ESSAIM_VAULT")
	t.Setenv("ESSAIM_CONFIG", filepath.Join(t.TempDir(), "config.json")) // empty config
	var out bytes.Buffer
	if err := runSync([]string{"--remote", "git@example.com:x/y.git"}, &out); err == nil {
		t.Fatal("runSync without a vault must error")
	}
}

// TestRunSyncEndToEnd drives the CLI against a local bare remote and confirms the
// vault is published.
func TestRunSyncEndToEnd(t *testing.T) {
	gitOrSkip(t)

	// Bare remote acting as the user's BYO git storage.
	remote := t.TempDir()
	runGit(t, remote, "init", "--bare", "-b", "main")

	// A local vault with one rule.
	vault := t.TempDir()
	if err := syncpkg.WriteVaultRecords(vault, []syncpkg.Record{
		{Identity: "use-tabs", Title: "Use tabs", Body: "Use tabs, not spaces.", Status: "active", Lamport: 1},
	}); err != nil {
		t.Fatalf("seed vault: %v", err)
	}

	var out bytes.Buffer
	if err := runSync([]string{"--remote", remote, "--vault", vault}, &out); err != nil {
		t.Fatalf("runSync: %v", err)
	}
	if !strings.Contains(out.String(), "synced 1 rules") {
		t.Fatalf("sync output should report 1 rule synced, got:\n%s", out.String())
	}

	// A fresh clone of the remote must contain the rule.
	verify := t.TempDir()
	runGit(t, verify, "clone", remote, ".")
	if _, err := os.Stat(filepath.Join(verify, "use-tabs.md")); err != nil {
		t.Fatalf("remote should contain use-tabs.md after sync: %v", err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
