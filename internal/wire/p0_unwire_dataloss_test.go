package wire

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"essaim/internal/rules"
)

// P0-3 [UNWIRE DESTROYS USER EDITS]: the .essaim.bak backup is a pristine snapshot
// taken ONCE at first wire and never refreshed. restoreNativeFile restored it
// byte-exact unconditionally, so `essaim unwire` silently overwrote the CURRENT
// file with the day-0 snapshot — destroying every edit the user made to
// CLAUDE.md/AGENTS.md after wiring — while the CLI printed "original config
// restored". This is unrecoverable data loss.
//
// The fix restores the backup byte-exact ONLY when the user has not diverged the
// file (current minus the managed block equals the backup). Otherwise it
// preserves the user's current content, stripping only essaim's managed block.

func managed(body string) string { return rules.WrapBlock(body) }

// User adds content AFTER wiring; unwire must keep it.
func TestP0UnwirePreservesUserEditsAfterWiring(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CLAUDE.md")

	// Day-0 pristine original captured as the backup.
	pristine := "# My rules\n\nOriginal line.\n"
	if err := os.WriteFile(path+backupSuffix, []byte(pristine), 0o644); err != nil {
		t.Fatal(err)
	}
	// Current file = pristine + managed block + a NEW user paragraph added later.
	current := pristine + "\n" + managed("- learned rule") + "\n\nUSER ADDED THIS AFTER WIRING — must survive.\n"
	if err := os.WriteFile(path, []byte(current), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := restoreNativeFile(path); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	s := string(got)
	if !strings.Contains(s, "USER ADDED THIS AFTER WIRING — must survive.") {
		t.Fatalf("P0-3: unwire destroyed user edits made after wiring; file now:\n%s", s)
	}
	if !strings.Contains(s, "Original line.") {
		t.Fatalf("P0-3: unwire lost original content; file now:\n%s", s)
	}
	if strings.Contains(s, rules.ESSAIM_BEGIN) {
		t.Fatalf("unwire must remove the managed block; file now:\n%s", s)
	}
}

// No user edits since wiring → byte-exact pristine restore (and backup removed).
func TestP0UnwireByteExactWhenNoUserEdits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")

	pristine := "# Rules\n\nKeep me.\n"
	if err := os.WriteFile(path+backupSuffix, []byte(pristine), 0o644); err != nil {
		t.Fatal(err)
	}
	// Current = pristine + one separator newline + block (the emit append shape),
	// with NO user edits outside the block.
	current := pristine + "\n" + managed("- rule") + "\n"
	if err := os.WriteFile(path, []byte(current), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := restoreNativeFile(path); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != pristine {
		t.Fatalf("no-edit unwire must restore the pristine original byte-exact;\n got: %q\nwant: %q", string(got), pristine)
	}
	if _, err := os.Stat(path + backupSuffix); !os.IsNotExist(err) {
		t.Fatalf("backup must be removed after a successful restore")
	}
}
