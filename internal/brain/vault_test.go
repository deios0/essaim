package brain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"oikos/internal/rules"
)

// WriteVault mirrors the pulled zone rules into <vault>/_brain/ as live .md files
// that the existing emit path picks up. It REPLACES the prior mirror (a removed
// zone rule disappears), and the files are injectable (status: live).
func TestWriteVaultMirrorsRulesAsLive(t *testing.T) {
	dir := t.TempDir()
	got := []Rule{
		{ID: "a1", Title: "Git hygiene", Body: "sync via git push/pull only"},
		{ID: "b2", Title: "Use Postgres", Body: "always postgres, never mysql"},
	}
	if err := WriteVault(dir, got); err != nil {
		t.Fatalf("WriteVault: %v", err)
	}
	// The mirror lands under _brain/ and loads back as injectable rules.
	rs, err := rules.LoadVault(dir)
	if err != nil {
		t.Fatalf("LoadVault: %v", err)
	}
	live := rules.InjectableRules(rs)
	if len(live) != 2 {
		t.Fatalf("want 2 injectable brain rules, got %d (of %d loaded)", len(live), len(rs))
	}
	// Files live under _brain/.
	entries, _ := os.ReadDir(filepath.Join(dir, "_brain"))
	if len(entries) != 2 {
		t.Fatalf("_brain/ has %d files, want 2", len(entries))
	}
}

// A second pull with FEWER rules removes the stale mirror file (no orphan).
func TestWriteVaultReplacesStaleMirror(t *testing.T) {
	dir := t.TempDir()
	_ = WriteVault(dir, []Rule{{ID: "a1", Body: "one"}, {ID: "b2", Body: "two"}})
	if err := WriteVault(dir, []Rule{{ID: "a1", Body: "one only now"}}); err != nil {
		t.Fatalf("WriteVault 2: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "_brain"))
	if len(entries) != 1 {
		t.Fatalf("stale mirror not pruned: _brain/ has %d files, want 1", len(entries))
	}
	b, _ := os.ReadFile(filepath.Join(dir, "_brain", entries[0].Name()))
	if !strings.Contains(string(b), "one only now") {
		t.Errorf("mirror not refreshed: %q", string(b))
	}
}

// A zone rule with no title must not surface its UUID as the title — derive a
// short title from the body instead (the Brain /api/rules response has body but
// often no title).
func TestWriteVaultDerivesTitleFromBodyWhenMissing(t *testing.T) {
	dir := t.TempDir()
	if err := WriteVault(dir, []Rule{{ID: "2b624f08-afe6-471c", Body: "Git hygiene: sync via git push/pull only, never rsync .git."}}); err != nil {
		t.Fatalf("WriteVault: %v", err)
	}
	rs, _ := rules.LoadVault(dir)
	if len(rs) != 1 {
		t.Fatalf("want 1 rule, got %d", len(rs))
	}
	title := rs[0].Title
	if strings.Contains(title, "2b624f08") {
		t.Fatalf("title must not be the UUID; got %q", title)
	}
	if !strings.Contains(strings.ToLower(title), "git hygiene") {
		t.Fatalf("title should be derived from the body's opening; got %q", title)
	}
}
