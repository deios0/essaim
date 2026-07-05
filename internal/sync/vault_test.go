package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMD(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// TestLoadVaultRecordsParsesSyncMeta asserts a rule file's sync metadata
// (cid/lamport/updated_at) is read from frontmatter when present.
func TestLoadVaultRecordsParsesSyncMeta(t *testing.T) {
	dir := t.TempDir()
	writeMD(t, dir, "r1.md", `---
id: no-force-push
title: No force push
lamport: 7
updated_at: 2026-06-24T10:00:00Z
status: active
kind: guardrail
---
Never force-push main.
`)
	recs, err := LoadVaultRecords(dir)
	if err != nil {
		t.Fatalf("LoadVaultRecords: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	r := recs[0]
	if r.Identity != "no-force-push" {
		t.Fatalf("identity = %q, want no-force-push", r.Identity)
	}
	if r.Lamport != 7 {
		t.Fatalf("lamport = %d, want 7", r.Lamport)
	}
	if r.UpdatedAt != "2026-06-24T10:00:00Z" {
		t.Fatalf("updated_at = %q, want the frontmatter value", r.UpdatedAt)
	}
	if !strings.Contains(r.Body, "Never force-push main.") {
		t.Fatalf("body not parsed: %q", r.Body)
	}
}

// TestLoadVaultRecordsDefaultsIdentityToFileStem asserts a rule with no `id`
// frontmatter still gets a stable Identity (the filename stem), so it has a
// stable merge key.
func TestLoadVaultRecordsDefaultsIdentityToFileStem(t *testing.T) {
	dir := t.TempDir()
	writeMD(t, dir, "tabs-not-spaces.md", "Use tabs, not spaces.\n")
	recs, err := LoadVaultRecords(dir)
	if err != nil {
		t.Fatalf("LoadVaultRecords: %v", err)
	}
	if len(recs) != 1 || recs[0].Identity != "tabs-not-spaces" {
		t.Fatalf("identity should default to the file stem, got %+v", recs)
	}
}

// TestRenderRecordRoundTrips asserts a Record renders to frontmatter+body that
// LoadVaultRecords reads back identically (content + sync meta preserved).
func TestRenderRecordRoundTrips(t *testing.T) {
	dir := t.TempDir()
	orig := Record{
		Identity:  "my-rule",
		Title:     "My rule",
		Body:      "Body line one.\nBody line two.",
		Kind:      "rule",
		Status:    "active",
		Lamport:   12,
		UpdatedAt: "2026-06-24T12:00:00Z",
	}
	writeMD(t, dir, "my-rule.md", RenderRecord(orig))

	recs, err := LoadVaultRecords(dir)
	if err != nil {
		t.Fatalf("LoadVaultRecords: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	got := recs[0]
	if got.Identity != orig.Identity || got.Lamport != orig.Lamport ||
		got.UpdatedAt != orig.UpdatedAt || got.Kind != orig.Kind || got.Status != orig.Status {
		t.Fatalf("round-trip meta mismatch:\n got=%+v\n want=%+v", got, orig)
	}
	if strings.TrimSpace(got.Body) != strings.TrimSpace(orig.Body) {
		t.Fatalf("round-trip body mismatch:\n got=%q\n want=%q", got.Body, orig.Body)
	}
	// The content address must survive the round-trip (durable content unchanged).
	if got.ContentID() != orig.ContentID() {
		t.Fatalf("ContentID changed across the round-trip: %q -> %q", orig.ContentID(), got.ContentID())
	}
}

// TestWriteVaultRecordsThenLoadConverges is the end-to-end vault path: render a
// merged set to a dir, load it back, and confirm the set matches (one file per
// rule, stable).
func TestWriteVaultRecordsThenLoadConverges(t *testing.T) {
	dir := t.TempDir()
	recs := []Record{
		{Identity: "a", Title: "A", Body: "a", Status: "active", Lamport: 1},
		{Identity: "b", Title: "B", Body: "b", Status: "active", Lamport: 2},
	}
	if err := WriteVaultRecords(dir, recs); err != nil {
		t.Fatalf("WriteVaultRecords: %v", err)
	}
	back, err := LoadVaultRecords(dir)
	if err != nil {
		t.Fatalf("LoadVaultRecords: %v", err)
	}
	if len(back) != 2 {
		t.Fatalf("expected 2 records back, got %d", len(back))
	}
}

// TestTwoDivergentVaultsMergeNoRuleLost is the headline Item-F scenario: two
// divergent vaults on disk → a deterministic merge → no rule lost, conflicts
// resolved by the logical clock.
func TestTwoDivergentVaultsMergeNoRuleLost(t *testing.T) {
	localDir := t.TempDir()
	remoteDir := t.TempDir()

	// Shared rule "a" edited on both sides; remote has the higher Lamport.
	writeMD(t, localDir, "a.md", "---\nid: a\nlamport: 1\nstatus: active\n---\nlocal edit of a\n")
	writeMD(t, remoteDir, "a.md", "---\nid: a\nlamport: 4\nstatus: active\n---\nremote edit of a\n")
	// Disjoint rules: local-only "b", remote-only "c".
	writeMD(t, localDir, "b.md", "---\nid: b\nlamport: 1\nstatus: active\n---\nlocal only b\n")
	writeMD(t, remoteDir, "c.md", "---\nid: c\nlamport: 1\nstatus: active\n---\nremote only c\n")

	local, err := LoadVaultRecords(localDir)
	if err != nil {
		t.Fatalf("load local: %v", err)
	}
	remote, err := LoadVaultRecords(remoteDir)
	if err != nil {
		t.Fatalf("load remote: %v", err)
	}

	merged := Merge(local, remote)
	if len(merged) != 3 {
		t.Fatalf("merge must keep all 3 distinct rules, got %d: %+v", len(merged), merged)
	}
	a, ok := find(merged, "a")
	if !ok {
		t.Fatal("conflicted rule a was lost")
	}
	if !strings.Contains(a.Body, "remote edit of a") {
		t.Fatalf("higher-Lamport remote edit of a must win, got body %q", a.Body)
	}
	if _, ok := find(merged, "b"); !ok {
		t.Fatal("local-only rule b was lost")
	}
	if _, ok := find(merged, "c"); !ok {
		t.Fatal("remote-only rule c was lost")
	}
}
