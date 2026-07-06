package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fsync-nit (durability review): the counts.json sidecar atomicWrite
// must fsync the temp BEFORE the rename, consistent with the ede42a4 vault
// hardening. Without it an interrupted write could leave a renamed zero-length
// .counts.json, silently resetting every reinforce count across a crash — which
// would make the durable-count promote (P0-2) unreliable, the very thing the
// sidecar exists to guarantee.
//
// This test observes the order via the syncFile seam: it records that the temp
// file was fsynced, and that at the moment of the sync the FINAL path did NOT yet
// exist (i.e. the sync happened on the temp BEFORE the rename). It would fail if
// someone removed the tmp.Sync() (the order/observation never fires).
func TestCountsSidecarFsyncsTempBeforeRename(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "_inbox", ".counts.json")

	var synced bool
	var finalExistedAtSync bool
	orig := syncFile
	syncFile = func(f *os.File) error {
		synced = true
		// At the moment we fsync, the temp is named ".essaim-*.tmp", and the FINAL
		// path must NOT exist yet (the rename happens only after the sync).
		if _, err := os.Stat(final); err == nil {
			finalExistedAtSync = true
		}
		return f.Sync()
	}
	defer func() { syncFile = orig }()

	h := newHotStoreAt(dir)
	// A reinforce triggers a persist() → atomicWrite → syncFile on the temp.
	h.reinforce("hash-a", false, true, time.Unix(1000, 0))

	if !synced {
		t.Fatal("the counts sidecar write must fsync the temp before rename (durability) — syncFile was never called")
	}
	if finalExistedAtSync {
		t.Fatal("the fsync must happen on the TEMP before the rename (the final path must not exist yet at sync time)")
	}
	// The persisted file must exist and be non-empty after the write completes.
	info, err := os.Stat(final)
	if err != nil {
		t.Fatalf("the counts sidecar must exist after a reinforce: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("the persisted counts sidecar must not be zero-length")
	}
}

// FIX 2 (review fix), through the real persist() seam: the first sidecar write
// must create the _inbox/ dir AND drop an _inbox/.gitignore, so a git-tracked
// vault never accidentally commits the .counts.json sidecar.
func TestCountsSidecarPersistWritesInboxGitignore(t *testing.T) {
	dir := t.TempDir()
	h := newHotStoreAt(dir)
	h.reinforce("hash-a", false, true, time.Unix(1000, 0)) // triggers persist()

	gi := filepath.Join(dir, "_inbox", ".gitignore")
	body, err := os.ReadFile(gi)
	if err != nil {
		t.Fatalf("sidecar persist must create _inbox/.gitignore: %v", err)
	}
	if !strings.Contains(string(body), "*") || !strings.Contains(string(body), "!.gitignore") {
		t.Fatalf("_inbox/.gitignore must ignore-all-but-self, got:\n%s", body)
	}
}

// A durability round-trip: the persisted count survives reload into a brand-new
// store over the same vault (the cross-restart guarantee the fsync protects).
func TestCountsSidecarRoundTripsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	h1 := newHotStoreAt(dir)
	h1.reinforce("hash-b", true /*validated*/, true /*atLeastNew*/, time.Unix(2000, 0))
	h1.reinforce("hash-b", false, true, time.Unix(3000, 0)) // count → 2

	// A brand-new store over the same vault reloads the persisted sidecar.
	h2 := newHotStoreAt(dir)
	if got := h2.count("hash-b"); got != 2 {
		t.Fatalf("reloaded count = %d, want 2 (persisted across reload)", got)
	}
	if !h2.latestHintAtLeastNew("hash-b") {
		t.Fatal("the persisted latest-hint flag must round-trip")
	}
}
