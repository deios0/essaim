package rules

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// NewStore on an empty dir → nil index (no vault = no rules), no error.
func TestStoreEmptyDirNoIndex(t *testing.T) {
	s, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore(empty): %v", err)
	}
	if s.Index().Len() != 0 {
		t.Fatalf("empty vault must yield 0 rules")
	}
}

// NewStore loads the initial index from a populated vault.
func TestStoreInitialLoad(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "r.md", "---\nid: r\ntitle: R\n---\nbody")
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if s.Index().Len() != 1 {
		t.Fatalf("want 1 rule, got %d", s.Index().Len())
	}
}

// Watch picks up a new rule file (debounced rebuild, atomic swap) and the new
// index is observed via the atomic pointer.
func TestWatchPicksUpNewRule(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.md", "---\nid: a\ntitle: A\n---\nbody a")
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	s.debounce = 30 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Watch(ctx); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer s.Close()

	writeFile(t, dir, "b.md", "---\nid: b\ntitle: B\n---\nbody b")

	deadline := time.After(3 * time.Second)
	for {
		if s.Index().Len() == 2 {
			return // observed the swap
		}
		select {
		case <-deadline:
			t.Fatalf("watch did not pick up the new rule; index len=%d", s.Index().Len())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// build-then-swap race test (design closure §5): concurrent readers Loading the
// pointer while rebuilds Store new indexes must never observe a torn/partial
// index. Run under -race to catch data races on the atomic pointer.
func TestBuildThenSwapNoTornRead(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.md", "---\nid: a\ntitle: A\n---\nbody")
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers: hammer rebuild (build-then-swap).
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = s.rebuild()
				}
			}
		}()
	}
	// Readers: hammer the atomic Load + a Match (the hot-path read).
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					ix := s.Index()
					if ix != nil {
						_ = ix.Match("a query")
						_ = ix.Len()
					}
				}
			}
		}()
	}
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// P1-4: rebuild on a transient FS error (the vault dir briefly missing/unreadable
// — editor atomic save-via-rename, OneDrive/AV lock, a WSL/Windows hiccup) must
// degrade to STALE, never to EMPTY: the last-good index stays published and the
// store is marked degraded. The prior code returned (nil,nil) from LoadVault for
// a missing dir and atomically published an empty index with degraded=false,
// silently wiping every live rule. (This test previously asserted Len()==0 —
// codifying the bug; it now asserts the safe behavior.)
func TestRebuildMissingDirStaysSafe(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.md", "---\nid: a\ntitle: A\n---\nbody")
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if s.Index().Len() != 1 {
		t.Fatalf("precondition: 1 rule")
	}
	if s.Degraded() {
		t.Fatalf("a healthy initial load must NOT be degraded")
	}
	_ = os.RemoveAll(dir)
	if err := s.rebuild(); err != nil {
		t.Fatalf("rebuild on missing dir must not error: %v", err)
	}
	// Missing dir → KEEP the last-good index (1 rule), never wipe to empty.
	if s.Index() == nil {
		t.Fatalf("index must never become nil after a swap")
	}
	if s.Index().Len() != 1 {
		t.Fatalf("P1-4: a missing dir must KEEP the last-good index (1 rule), got %d", s.Index().Len())
	}
	if !s.Degraded() {
		t.Fatalf("P1-4: keeping a stale index on a missing dir must mark the store degraded")
	}
	_ = filepath.Clean(dir)
}

// P1-4: a rebuild that loads ZERO rules while the previous index was NON-EMPTY
// (even if the dir still exists but was momentarily unreadable / mid-rename) must
// keep the last-good index and degrade — never silently wipe live rules to empty.
func TestRebuildEmptyAfterNonEmptyKeepsLastGood(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.md", "---\nid: a\ntitle: A\n---\nbody")
	writeFile(t, dir, "b.md", "---\nid: b\ntitle: B\n---\nbody")
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if s.Index().Len() != 2 {
		t.Fatalf("precondition: 2 rules, got %d", s.Index().Len())
	}
	// Remove every rule file (the dir still exists) → LoadVault returns 0 rules.
	if err := os.Remove(filepath.Join(dir, "a.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "b.md")); err != nil {
		t.Fatal(err)
	}
	if err := s.rebuild(); err != nil {
		t.Fatalf("rebuild must not error: %v", err)
	}
	// 0-rules-after-non-empty is treated as a suspicious transient → keep last-good.
	if s.Index().Len() != 2 {
		t.Fatalf("P1-4: an empty load after a non-empty index must KEEP last-good (2), got %d", s.Index().Len())
	}
	if !s.Degraded() {
		t.Fatalf("P1-4: keeping a stale index on an empty load must mark degraded")
	}
}

// A LEGITIMATELY empty vault (empty from the very first load) is NOT degraded —
// 0 rules is the honest state, not a transient wipe. This guards against the
// P1-4 fix over-degrading a genuinely empty vault.
func TestInitialEmptyVaultNotDegraded(t *testing.T) {
	dir := t.TempDir() // exists, no .md files
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if s.Index().Len() != 0 {
		t.Fatalf("an empty vault must yield 0 rules")
	}
	if s.Degraded() {
		t.Fatalf("a genuinely-empty vault (empty from first load) must NOT be degraded")
	}
}
