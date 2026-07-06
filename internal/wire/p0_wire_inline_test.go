package wire

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"essaim/internal/rules"
)

// Codex BLOCKER: stripManagedBlock used unanchored strings.Index. On the diverged
// unwire path, a user's INLINE begin sentinel before the real block would splice
// and destroy content. With the shared line-anchored ManagedRegion it must not.
func TestP0UnwireInlineSentinelNoCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CLAUDE.md")

	// backup = pristine WITH an inline sentinel in prose (user documented it).
	pristine := "# Doc\n\nExample marker `" + rules.ESSAIM_BEGIN + "` shown inline.\n"
	if err := os.WriteFile(path+backupSuffix, []byte(pristine), 0o644); err != nil {
		t.Fatal(err)
	}
	// current = pristine + real block + a later user edit (diverged).
	current := pristine + "\n" + rules.WrapBlock("- learned") + "\n\nLATER USER EDIT keep me.\n"
	if err := os.WriteFile(path, []byte(current), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := restoreNativeFile(path); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	s := string(got)
	if !strings.Contains(s, "LATER USER EDIT keep me.") {
		t.Fatalf("diverged unwire lost user edit; file:\n%s", s)
	}
	if !strings.Contains(s, "Example marker") {
		t.Fatalf("diverged unwire corrupted content around the inline sentinel; file:\n%s", s)
	}
	// The real managed block must be gone; the inline mention stays.
	if _, _, ok := rules.ManagedRegion(s); ok {
		t.Fatalf("managed block must be removed; file:\n%s", s)
	}
}
