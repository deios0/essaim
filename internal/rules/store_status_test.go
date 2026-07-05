package rules

import (
	"os"
	"path/filepath"
	"testing"
)

// Test 3 (BL-1): BuildIndex (via the Store) indexes only {active,live}, but the
// Store's full slice (for the lifecycle sweep) still contains the draft too.
func TestBuildIndexFiltersNonLiveKeepsFullSliceForSweep(t *testing.T) {
	dir := t.TempDir()
	// One live rule at the vault root.
	mustWrite(t, filepath.Join(dir, "live.md"),
		"---\nid: live\ntitle: Live Rule\nstatus: live\nweight: 0.9\n---\nA live rule body.")
	// One draft in _inbox/.
	inbox := filepath.Join(dir, "_inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(inbox, "draft.md"),
		"---\nid: draft\ntitle: Draft Rule\nstatus: draft\nweight: 0.9\n---\nA draft rule body.")

	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	// The published index sees ONLY the live rule.
	if n := store.Index().Len(); n != 1 {
		t.Fatalf("published index Len = %d, want 1 (live only; draft filtered)", n)
	}
	// The full slice (for the sweep) sees BOTH.
	all := store.AllRules()
	if len(all) != 2 {
		t.Fatalf("AllRules = %d, want 2 (live + draft for the sweep)", len(all))
	}
	var sawDraft bool
	for _, r := range all {
		if r.ID == "draft" {
			sawDraft = true
		}
	}
	if !sawDraft {
		t.Fatal("the lifecycle sweep must still see the draft in AllRules")
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
