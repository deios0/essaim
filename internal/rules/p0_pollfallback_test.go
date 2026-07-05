package rules

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// P0-4 [WATCHER SILENTLY DEAD ON WSL2 /mnt/c + NETWORK FS]: fsnotify delivers no
// events on WSL2 DrvFs and many network filesystems, and Store.loop rebuilt the
// index ONLY on fsnotify events — so on those platforms (30-40% of the target)
// the live index and the learning loop froze on the startup snapshot forever,
// with no poll fallback and no health signal. Rebuild() had zero callers.
//
// The fix adds an mtime-gated poll fallback: each poll tick cheaply scans the
// vault's max modtime and rebuilds only when it advanced past the last rebuild —
// catching changes fsnotify missed on ANY filesystem — and sets a sticky
// degraded flag the moment the poll has to cover for fsnotify.
func TestP0WatcherPollFallbackReindexes(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "r1.md"),
		"---\nid: r1\ntitle: R1\nstatus: live\nweight: 0.9\n---\nfirst rule about postgres")

	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if n := store.Index().Len(); n != 1 {
		t.Fatalf("want 1 rule initially, got %d", n)
	}
	if store.WatcherDegraded() {
		t.Fatal("watcher must not start degraded")
	}

	// Simulate an external edit fsnotify would MISS on WSL: add a rule directly
	// with a modtime clearly newer than the last rebuild.
	p2 := filepath.Join(dir, "r2.md")
	mustWrite(t, p2, "---\nid: r2\ntitle: R2\nstatus: live\nweight: 0.9\n---\nsecond rule about docker")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p2, future, future); err != nil {
		t.Fatal(err)
	}

	// The poll fallback must detect the change WITHOUT any fsnotify event.
	if !store.pollOnce() {
		t.Fatal("P0-4: pollOnce must detect the external change and rebuild")
	}
	if n := store.Index().Len(); n != 2 {
		t.Fatalf("P0-4: poll fallback must re-index; want 2 rules, got %d", n)
	}
	if !store.WatcherDegraded() {
		t.Fatal("P0-4: poll had to cover for fsnotify → WatcherDegraded must be true")
	}

	// Idempotent: a second poll with no vault change must not rebuild.
	if store.pollOnce() {
		t.Fatal("pollOnce with no change must not rebuild")
	}
}
