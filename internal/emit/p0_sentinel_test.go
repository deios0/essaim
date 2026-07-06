package emit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"essaim/internal/rules"
)

// P0-2 [EMIT SILENT USER-CONTENT DELETION]: extractFencedRegion /
// replaceFencedRegion used strings.Index to find the FIRST ESSAIM_BEGIN anywhere
// in the file and the FIRST ESSAIM_END after it — unanchored and unpaired. A user
// whose native file contains the begin sentinel INLINE in prose (documenting
// essaim, a pasted example, a quoted marker) makes the region detector span from
// that inline marker to the real block's END, silently deleting every byte of
// user content in between on the next emit.
//
// The fix only recognizes a managed region whose BEGIN is line-anchored and
// alone on its line, paired with the next line-anchored solo END — exactly what
// WrapBlock always writes — and skips inline/quoted occurrences.

func writeBlock(body string) string { return rules.WrapBlock(body) }

// A file with an INLINE begin sentinel in user prose, then a real managed block
// appended after it, must survive a re-emit with ALL user content intact.
func TestP0EmitInlineSentinelNoDataLoss(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CLAUDE.md")

	userTop := "# My project rules\n\n" +
		"We document the marker `" + rules.ESSAIM_BEGIN + "` inline as an example here.\n\n" +
		"IMPORTANT user paragraph that must never be deleted.\n"
	// Simulate a first emit that appended a real managed block after the user text.
	block1 := writeBlock("- rule one")
	initial := userTop + "\n" + block1 + "\n"
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	// Re-emit a new block: must replace ONLY the real appended block, preserving
	// all user content (including the inline-marker line and the IMPORTANT para).
	block2 := writeBlock("- rule one\n- rule two")
	if err := writeRenameFencedWithBackup(path, block2); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	s := string(got)

	if !strings.Contains(s, "IMPORTANT user paragraph that must never be deleted.") {
		t.Fatalf("P0-2: user content deleted by re-emit; file now:\n%s", s)
	}
	if !strings.Contains(s, "inline as an example here.") {
		t.Fatalf("P0-2: user inline-marker prose line deleted; file now:\n%s", s)
	}
	if !strings.Contains(s, "- rule two") {
		t.Fatalf("re-emit did not update the managed block; file now:\n%s", s)
	}
	// Exactly ONE real managed BEGIN…END pair should remain in the block region
	// (the inline one is defanged in the block body but the user's prose copy is
	// left byte-exact). The managed region must be the appended one, not the
	// inline user marker.
	if n := strings.Count(s, "\n"+rules.ESSAIM_END); n != 1 {
		t.Fatalf("expected exactly one line-anchored managed END, got %d; file:\n%s", n, s)
	}
}

// A re-emit must be idempotent and must not append a second block when a real
// managed region already exists (even with an inline sentinel earlier).
func TestP0EmitIdempotentWithInlineSentinel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	user := "intro mentioning " + rules.ESSAIM_END + " inline, not a real fence.\n\n"
	block := writeBlock("- only rule")
	if err := os.WriteFile(path, []byte(user+block+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeRenameFencedWithBackup(path, block); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if n := strings.Count(string(got), rules.ESSAIM_BEGIN); n != 1 {
		t.Fatalf("idempotent re-emit must keep exactly one managed BEGIN; got %d:\n%s", n, string(got))
	}
	if !strings.Contains(string(got), "intro mentioning") {
		t.Fatalf("user intro line lost:\n%s", string(got))
	}
}
