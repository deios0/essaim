package emit

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"essaim/internal/rules"
)

// symlinkOrSkip creates newname -> oldname, skipping the test on a host that can't
// make symlinks (e.g. a non-privileged Windows session). The rest of the suite is
// POSIX-first; these tests assert the symlink write-through contract where symlinks
// exist.
func symlinkOrSkip(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unsupported on this host: %v", err)
		}
		t.Fatalf("symlink: %v", err)
	}
}

// P2-5 (i)+(iii): emitting to a SYMLINKED native file (the CLAUDE.md -> AGENTS.md
// single-source setup) must PRESERVE the symlink and write the block THROUGH it to
// the real target — it must NOT replace the link with a detached regular file
// (which silently diverges the two names).
func TestEmitThroughSymlinkPreservesLinkAndWritesTarget(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "AGENTS.md")
	link := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(real, []byte("# real target\n\nuser content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, real, link) // CLAUDE.md -> AGENTS.md

	e := New(rules.GuardConfig{EagerBytes: 4096}, []Tool{{Name: "claude-code", NativeFile: link}})
	e.SetDebounce(0)
	if _, err := e.EmitNow(liveIndex(t)); err != nil {
		t.Fatalf("emit through symlink: %v", err)
	}

	// (i) The logical path is STILL a symlink (not clobbered by a regular file).
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("emit must PRESERVE the symlink at %s, but it is now a regular file", link)
	}

	// (iii) The block landed in the REAL target (write-through), and the target's
	// user content is preserved.
	got, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, rules.ESSAIM_BEGIN) || !strings.Contains(s, "Always use PostgreSQL") {
		t.Fatalf("the block must be written THROUGH the symlink into the real target:\n%s", s)
	}
	if !strings.Contains(s, "user content") {
		t.Fatalf("the real target's user content must be preserved:\n%s", s)
	}

	// Reading through the link must observe the same bytes (single source of truth).
	viaLink, err := os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if string(viaLink) != s {
		t.Fatalf("reading through the link must equal the real target (single source):\nlink:\n%s\nreal:\n%s", viaLink, s)
	}
}

// P2-5 (ii): the .essaim.bak must land at the LOGICAL (symlink) path, because
// wire/unwire snapshot and restore at that path. A backup written at the RESOLVED
// real path would be orphaned (unwire would never find it → stale/leftover backup).
func TestEmitThroughSymlinkBackupAtLogicalPath(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "AGENTS.md")
	link := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(real, []byte("# real target\n\nuser content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, real, link)

	e := New(rules.GuardConfig{EagerBytes: 4096}, []Tool{{Name: "cc", NativeFile: link}})
	e.SetDebounce(0)
	if _, err := e.EmitNow(liveIndex(t)); err != nil {
		t.Fatalf("emit: %v", err)
	}

	logicalBak := link + ".essaim.bak"
	realBak := real + ".essaim.bak"
	if _, err := os.Stat(logicalBak); err != nil {
		t.Fatalf("the backup must be at the LOGICAL (symlink) path %s where unwire looks: %v", logicalBak, err)
	}
	if _, err := os.Stat(realBak); err == nil {
		t.Fatalf("no orphaned backup may be written at the RESOLVED real path %s", realBak)
	}
	// The backup must be the PRISTINE original (pre-block) content.
	bak, _ := os.ReadFile(logicalBak)
	if strings.Contains(string(bak), rules.ESSAIM_BEGIN) {
		t.Fatalf("the backup must be the pristine pre-block original, not a with-block copy:\n%s", bak)
	}
	if !strings.Contains(string(bak), "user content") {
		t.Fatalf("the backup must hold the original user content:\n%s", bak)
	}
}

// P2-5 (iv): a DANGLING symlink (points at a not-yet-existing target) must be
// PRESERVED — emit writes to its readlink target (creating it), it must NOT be
// replaced by a regular file at the link path.
// A DANGLING symlink (points at a missing target) is written as a normal file at
// the LOGICAL path — replacing the already-broken link — so the block, the .bak,
// and a later unwire all live at one consistent path. Writing THROUGH to the
// readlink target instead created a block-only file that unwire would orphan
// while deleting the link (codex integral review). Replacing a broken link loses
// nothing.
func TestEmitThroughDanglingSymlinkWritesLogicalPathUnwireSafe(t *testing.T) {
	dir := t.TempDir()
	realMissing := filepath.Join(dir, "AGENTS.md") // does NOT exist
	link := filepath.Join(dir, "CLAUDE.md")
	symlinkOrSkip(t, realMissing, link) // dangling: CLAUDE.md -> (missing) AGENTS.md

	e := New(rules.GuardConfig{EagerBytes: 4096}, []Tool{{Name: "cc", NativeFile: link}})
	e.SetDebounce(0)
	if _, err := e.EmitNow(liveIndex(t)); err != nil {
		t.Fatalf("emit through dangling symlink: %v", err)
	}

	// The logical path now holds the block as a regular file (broken link replaced).
	got, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("emit must write the block at the logical path: %v", err)
	}
	if !strings.Contains(string(got), rules.ESSAIM_BEGIN) {
		t.Fatalf("the block must be written at the logical path:\n%s", got)
	}
	// The block must NOT have been materialized at the (missing) readlink target —
	// that orphan path is exactly the unwire hazard we avoid.
	if _, err := os.Lstat(realMissing); !os.IsNotExist(err) {
		t.Fatalf("the readlink target must NOT be created (unwire-safety); Lstat err=%v", err)
	}
}
