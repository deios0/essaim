package rules

import (
	"os"
	"path/filepath"
	"testing"
)

// FIX 2 (review fix): the first time the daemon writes into the vault's _inbox/
// (where the .counts.json sidecar + unreviewed draft rules live), it must drop an
// _inbox/.gitignore so a user whose vault is a git repo never accidentally commits
// .counts.json or unreviewed drafts. EnsureInboxDir creates the dir AND the
// .gitignore on first use.
func TestEnsureInboxDirWritesGitignore(t *testing.T) {
	vault := t.TempDir()

	dir, err := EnsureInboxDir(vault)
	if err != nil {
		t.Fatalf("EnsureInboxDir: %v", err)
	}
	if want := filepath.Join(vault, InboxDir); dir != want {
		t.Fatalf("inbox dir = %q, want %q", dir, want)
	}

	// The dir must exist.
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("_inbox/ dir not created: err=%v", err)
	}

	// The .gitignore must exist with the expected content.
	giPath := filepath.Join(dir, ".gitignore")
	got, err := os.ReadFile(giPath)
	if err != nil {
		t.Fatalf("_inbox/.gitignore not created: %v", err)
	}
	if string(got) != inboxGitignore {
		t.Fatalf("_inbox/.gitignore content mismatch:\n got: %q\nwant: %q", got, inboxGitignore)
	}

	// The content must ignore everything in _inbox/ EXCEPT the .gitignore itself,
	// so .counts.json and any draft *.md are covered.
	if !contains(string(got), "*") || !contains(string(got), "!.gitignore") {
		t.Fatalf("_inbox/.gitignore must ignore-all-but-self, got:\n%s", got)
	}
}

// EnsureInboxDir must be idempotent and must NOT clobber a user-edited .gitignore
// on a second call (only writes the file when it does not already exist).
func TestEnsureInboxDirIdempotentNoClobber(t *testing.T) {
	vault := t.TempDir()

	if _, err := EnsureInboxDir(vault); err != nil {
		t.Fatalf("first EnsureInboxDir: %v", err)
	}
	giPath := filepath.Join(vault, InboxDir, ".gitignore")

	// Simulate the operator customizing the .gitignore.
	custom := "# operator-edited\n*\n!.gitignore\n!keepme\n"
	if err := os.WriteFile(giPath, []byte(custom), 0o644); err != nil {
		t.Fatalf("write custom gitignore: %v", err)
	}

	// A second call must NOT overwrite the operator's edits.
	if _, err := EnsureInboxDir(vault); err != nil {
		t.Fatalf("second EnsureInboxDir: %v", err)
	}
	got, err := os.ReadFile(giPath)
	if err != nil {
		t.Fatalf("read gitignore after 2nd call: %v", err)
	}
	if string(got) != custom {
		t.Fatalf("EnsureInboxDir clobbered an existing .gitignore:\n got: %q\nwant: %q", got, custom)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
