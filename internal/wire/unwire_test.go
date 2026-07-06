package wire

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"essaim/internal/config"
)

// P0-2 — UNWIRE + BACKUP. `essaim wire <tool>` modifies a tool's config; there must
// be a clean undo. (a) wire backs up the target config (.essaim.bak) before
// modifying (like the emitter); (b) `essaim unwire <tool>` restores the original
// byte-exact / removes the managed block, idempotent.

// wire→backup created: wiring a native-file tool over an EXISTING user file backs
// up the pristine original to <path>.essaim.bak before touching it.
func TestWireNativeFileCreatesBackup(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))
	target := filepath.Join(dir, "CLAUDE.md")
	const original = "# My Project\n\nMy own rules. essaim must never touch these.\n"
	writeFile(t, target, original)

	p, _ := Resolve("claude-code", dir)
	if _, err := Apply(p); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	bak := target + ".essaim.bak"
	got, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("wire must create a %s backup, read err: %v", bak, err)
	}
	if string(got) != original {
		t.Fatalf("backup must be the PRISTINE original.\nwant %q\ngot  %q", original, string(got))
	}
}

// unwire→original restored byte-exact: after wiring an existing file (managed block
// added), unwire restores the EXACT original bytes and removes the backup.
func TestUnwireRestoresOriginalByteExact(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))
	target := filepath.Join(dir, "CLAUDE.md")
	const original = "# My Project\n\nMy own rules.\nLine two.\n"
	writeFile(t, target, original)

	p, _ := Resolve("claude-code", dir)
	if _, err := Apply(p); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Sanity: the block is in place now.
	if mid := readFile(t, target); !strings.Contains(mid, "essaim:rules:begin") {
		t.Fatalf("precondition: managed block should be present after wire:\n%s", mid)
	}

	if err := Unwire("claude-code", dir); err != nil {
		t.Fatalf("Unwire: %v", err)
	}

	// The file must be byte-IDENTICAL to the original.
	got := readFile(t, target)
	if got != original {
		t.Fatalf("unwire must restore the original byte-exact.\nwant %q\ngot  %q", original, got)
	}
	// The backup must be cleaned up.
	if _, err := os.Stat(target + ".essaim.bak"); !os.IsNotExist(err) {
		t.Fatalf("unwire must remove the .essaim.bak backup, stat err: %v", err)
	}
	// The config record must be gone.
	c, _ := config.Load()
	for _, w := range c.WiredTools {
		if w.Name == "claude-code" {
			t.Fatalf("unwire must remove the wired-tool record, still present: %+v", c.WiredTools)
		}
	}
}

// Wiring a native-file tool TWICE then unwiring must restore the ORIGINAL bytes
// exactly (m4-install-fix nit). The invariant holds by construction — the second
// wire reads a file that ALREADY carries an essaim block, and backupTargetOnce is
// stat-guarded so it never re-backs-up that already-modified file (which would
// overwrite the pristine .essaim.bak with a block-bearing snapshot and make unwire
// restore a stale block instead of the clean original). This test asserts that
// invariant directly: wire → wire → unwire ⇒ byte-identical to the original, and
// the .essaim.bak is cleaned up.
func TestWireTwiceThenUnwireRestoresByteExact(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))
	target := filepath.Join(dir, "CLAUDE.md")
	// User content with deliberate blank lines + no-trailing-newline tail, to make
	// any off-by-one in the wire/backup/strip path show up as a byte diff.
	const original = "# My Project\n\nMy own rules.\n\n- never let essaim touch these\nTAIL no newline"
	writeFile(t, target, original)

	p, err := Resolve("claude-code", dir)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Wire #1: backs up the pristine original to .essaim.bak and seeds the block.
	if _, err := Apply(p); err != nil {
		t.Fatalf("Apply #1: %v", err)
	}
	bak := target + ".essaim.bak"
	if got := readFile(t, bak); got != original {
		t.Fatalf("after wire #1 the backup must be the PRISTINE original.\nwant %q\ngot  %q", original, got)
	}
	afterFirst := readFile(t, target) // file now carries exactly one essaim block

	// Wire #2 (idempotent): must NOT add a second block and must NOT overwrite the
	// pristine backup with the already-modified file.
	if _, err := Apply(p); err != nil {
		t.Fatalf("Apply #2: %v", err)
	}
	if got := readFile(t, target); got != afterFirst {
		t.Fatalf("wire #2 must be idempotent (no second block, no rewrite).\nafter #1 %q\nafter #2 %q", afterFirst, got)
	}
	if c := strings.Count(readFile(t, target), "essaim:rules:begin"); c != 1 {
		t.Fatalf("after wiring twice there must be exactly ONE managed block, got %d", c)
	}
	if got := readFile(t, bak); got != original {
		t.Fatalf("wire #2 must NOT overwrite the pristine backup.\nwant %q\ngot  %q", original, got)
	}

	// Unwire once must restore the original byte-exact and remove the backup.
	if err := Unwire("claude-code", dir); err != nil {
		t.Fatalf("Unwire: %v", err)
	}
	if got := readFile(t, target); got != original {
		t.Fatalf("unwire after wiring TWICE must restore the original byte-exact.\nwant %q\ngot  %q", original, got)
	}
	if _, err := os.Stat(bak); !os.IsNotExist(err) {
		t.Fatalf("unwire must remove the .essaim.bak backup, stat err: %v", err)
	}
}

// A native file essaim CREATED (no pre-existing original) is removed on unwire when
// it holds only the managed block — restoring the "file did not exist" state.
func TestUnwireRemovesEssaimCreatedFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))
	target := filepath.Join(dir, "CLAUDE.md")
	// No pre-existing file.

	p, _ := Resolve("claude-code", dir)
	if _, err := Apply(p); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("precondition: wire should create the native file: %v", err)
	}

	if err := Unwire("claude-code", dir); err != nil {
		t.Fatalf("Unwire: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("unwire of an essaim-created file (block-only) must remove it (original = no file), stat err: %v", err)
	}
}

// unwire when NOT wired = clean no-op (idempotent).
func TestUnwireNotWiredIsCleanNoOp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))

	if err := Unwire("claude-code", dir); err != nil {
		t.Fatalf("unwire of a never-wired tool must be a clean no-op, got: %v", err)
	}
	// Double unwire after a real wire+unwire is also a no-op.
	target := filepath.Join(dir, "CLAUDE.md")
	writeFile(t, target, "x\n")
	p, _ := Resolve("claude-code", dir)
	if _, err := Apply(p); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := Unwire("claude-code", dir); err != nil {
		t.Fatalf("first Unwire: %v", err)
	}
	if err := Unwire("claude-code", dir); err != nil {
		t.Fatalf("second Unwire (already unwired) must be a clean no-op, got: %v", err)
	}
}

// unwire of a base_url tool removes the config record (no file was ever written by
// wire, so there is nothing on disk to restore — it is purely a record removal).
func TestUnwireBaseURLToolRemovesRecord(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))

	p, _ := Resolve("cursor", dir)
	if _, err := Apply(p); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := Unwire("cursor", dir); err != nil {
		t.Fatalf("Unwire(cursor): %v", err)
	}
	c, _ := config.Load()
	for _, w := range c.WiredTools {
		if w.Name == "cursor" {
			t.Fatalf("unwire must remove the cursor record, still present: %+v", c.WiredTools)
		}
	}
}

// Byte-exact restore is to the pristine backup; but if the user edited their OWN
// content around the block, the block-removal fallback (no backup) must still
// preserve that content — including user-authored blank lines (only the single
// essaim separator is removed, never user whitespace).
func TestUnwireWithoutBackupStripsBlockPreservingUserContent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))
	target := filepath.Join(dir, "CLAUDE.md")

	p, _ := Resolve("claude-code", dir)
	if _, err := Apply(p); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Remove the backup to force the block-strip fallback, then add user content
	// around the block so removal must keep that content.
	_ = os.Remove(target + ".essaim.bak")
	cur := readFile(t, target)
	edited := "# Header I added\n\n" + cur + "\nFooter I added\n"
	writeFile(t, target, edited)

	if err := Unwire("claude-code", dir); err != nil {
		t.Fatalf("Unwire: %v", err)
	}
	got := readFile(t, target)
	if strings.Contains(got, "essaim:rules:begin") || strings.Contains(got, "essaim:rules:end") {
		t.Fatalf("unwire must strip the managed block, still present:\n%s", got)
	}
	if !strings.Contains(got, "# Header I added") || !strings.Contains(got, "Footer I added") {
		t.Fatalf("unwire must preserve user content around the block:\n%s", got)
	}
}

// stripManagedBlock removes exactly ONE separator newline on each side, so a
// user-authored blank line adjacent to the block survives byte-exact.
func TestStripManagedBlockPreservesUserBlankLines(t *testing.T) {
	// User content with a deliberate blank line, then a seeder-style separator,
	// then the block, then more user content with a deliberate blank line.
	user := "# Title\n\nbody line\n"
	block := "<!-- essaim:rules:begin v=1 -->\n- [H] R: x\n<!-- essaim:rules:end -->"
	// Seeder appends "\n" separator before and "\n" after when content ends "\n".
	s := user + "\n" + block + "\nmore\n\ntrailing\n"
	out, only := stripManagedBlock(s)
	if only {
		t.Fatalf("onlyBlock must be false when user content surrounds the block")
	}
	if strings.Contains(out, "essaim:rules:begin") {
		t.Fatalf("block must be removed, got:\n%q", out)
	}
	// The user's blank line in the header ("# Title\n\nbody") must survive.
	if !strings.Contains(out, "# Title\n\nbody line\n") {
		t.Fatalf("user blank line before the block must be preserved, got:\n%q", out)
	}
	// The user's later blank line ("more\n\ntrailing") must survive.
	if !strings.Contains(out, "more\n\ntrailing\n") {
		t.Fatalf("user blank line after the block must be preserved, got:\n%q", out)
	}
}

// P1-BUG-2: once the heal watcher has run it wrote the essaim proxy URL into the
// tool's IDE config (e.g. ~/.continue/config.json). `unwire` of that base_url tool
// must run the INVERSE of heal on the config file — restoring the vendor default
// (or removing the override) — so the user is not left pointing at a dead proxy
// after deleting the essaim binary. This is the direct restoreBaseURLConfig seam.
func TestRestoreBaseURLConfigUndoesProxyURL(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	// The heal watcher left the proxy URL in the config.
	writeFile(t, cfgPath, `{"models":[{"apiBase":"`+proxyV1URL+`"}],"theme":"dark"}`)

	status, err := restoreBaseURLConfig(cfgPath)
	if err != nil {
		t.Fatalf("restoreBaseURLConfig: %v", err)
	}
	if !status.Changed {
		t.Fatal("restoreBaseURLConfig must report a change when the proxy URL was present")
	}
	got := readFile(t, cfgPath)
	if strings.Contains(got, "127.0.0.1:4141") {
		t.Fatalf("unwire must remove the essaim proxy URL from the config:\n%s", got)
	}
	// Unrelated keys are preserved (surgical).
	if !strings.Contains(got, `"theme"`) || !strings.Contains(got, `"dark"`) {
		t.Fatalf("restore must preserve unrelated keys:\n%s", got)
	}
}

// A config that does NOT hold the proxy URL (user set their own, or never ran the
// daemon) must be left byte-untouched and report no change — unwire never fights
// a user value.
func TestRestoreBaseURLConfigLeavesNonProxyValueAlone(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	const userVal = `{"apiBase":"https://my-gateway.internal/v1"}`
	writeFile(t, cfgPath, userVal)

	status, err := restoreBaseURLConfig(cfgPath)
	if err != nil {
		t.Fatalf("restoreBaseURLConfig: %v", err)
	}
	if status.Changed {
		t.Fatal("a non-proxy user value must not be changed on unwire")
	}
	if got := readFile(t, cfgPath); got != userVal {
		t.Fatalf("user value must be byte-exact; got %s", got)
	}
}

// A missing config file is not an error and yields a NeedsManualHint status so the
// CLI can print an honest recovery hint (the config could not be auto-restored).
func TestRestoreBaseURLConfigMissingFileYieldsHint(t *testing.T) {
	dir := t.TempDir()
	status, err := restoreBaseURLConfig(filepath.Join(dir, "does-not-exist.json"))
	if err != nil {
		t.Fatalf("a missing config must not error: %v", err)
	}
	if status.Changed {
		t.Fatal("a missing config cannot have been changed")
	}
}

// An empty config path (tool with no known/repairable config location) is a clean
// no-op with a hint — essaim doesn't know where the tool's config lives, so it must
// tell the user to check manually rather than silently claim "restored".
func TestRestoreBaseURLConfigEmptyPathYieldsHint(t *testing.T) {
	status, err := restoreBaseURLConfig("")
	if err != nil {
		t.Fatalf("empty path must not error: %v", err)
	}
	if status.Changed {
		t.Fatal("empty path cannot report a change")
	}
	if !status.NeedsManualHint {
		t.Fatal("an unknown config location must flag NeedsManualHint so the CLI can warn the user")
	}
}

// End-to-end via Unwire: wire continue (base_url), simulate the heal watcher
// writing the proxy URL into its config, then unwire — the config must no longer
// point at the dead proxy.
func TestUnwireBaseURLRestoresHealedConfigEndToEnd(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(root, "config.json"))
	// Point the "continue default config" resolver at a sandbox file via HOME so we
	// never touch the real ~/.continue.
	fakeHome := filepath.Join(root, "home")
	contDir := filepath.Join(fakeHome, ".continue")
	if err := os.MkdirAll(contDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)

	p, _ := Resolve("continue", "")
	if _, err := Apply(p); err != nil {
		t.Fatalf("Apply(continue): %v", err)
	}
	// Simulate the heal watcher having written the proxy URL into the IDE config.
	cfgPath := filepath.Join(contDir, "config.json")
	writeFile(t, cfgPath, `{"models":[{"apiBase":"`+proxyV1URL+`"}]}`)

	if err := Unwire("continue", ""); err != nil {
		t.Fatalf("Unwire(continue): %v", err)
	}
	got := readFile(t, cfgPath)
	if strings.Contains(got, "127.0.0.1:4141") {
		t.Fatalf("unwire of a base_url tool must undo the heal-written proxy URL:\n%s", got)
	}
}

// P1-BUG-1: claude-code wired in TWO projects must keep TWO independent records,
// and `unwire --dir projA` must act on projA's record ONLY — projB's record and
// its managed block must survive untouched. The old code keyed by Name, so wiring
// projB replaced projA's record and `unwire --dir projA` unwired projB.
func TestWireTwoProjectsThenUnwireOneIsScopedByDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(root, "config.json"))

	projA := filepath.Join(root, "projA")
	projB := filepath.Join(root, "projB")
	if err := os.MkdirAll(projA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projB, 0o755); err != nil {
		t.Fatal(err)
	}
	const origA = "# Project A\n\nA rules.\n"
	const origB = "# Project B\n\nB rules.\n"
	writeFile(t, filepath.Join(projA, "CLAUDE.md"), origA)
	writeFile(t, filepath.Join(projB, "CLAUDE.md"), origB)

	// Wire claude-code in BOTH projects.
	pa, _ := Resolve("claude-code", projA)
	if _, err := Apply(pa); err != nil {
		t.Fatalf("Apply projA: %v", err)
	}
	pb, _ := Resolve("claude-code", projB)
	if _, err := Apply(pb); err != nil {
		t.Fatalf("Apply projB: %v", err)
	}

	// Both records must coexist.
	c, _ := config.Load()
	if n := countTool(c, "claude-code"); n != 2 {
		t.Fatalf("wiring claude-code in two projects must keep two records, got %d: %+v", n, c.WiredTools)
	}
	// Both files must now carry a managed block.
	if !strings.Contains(readFile(t, filepath.Join(projA, "CLAUDE.md")), "essaim:rules:begin") {
		t.Fatal("projA CLAUDE.md must carry the managed block after wire")
	}
	if !strings.Contains(readFile(t, filepath.Join(projB, "CLAUDE.md")), "essaim:rules:begin") {
		t.Fatal("projB CLAUDE.md must carry the managed block after wire")
	}

	// Unwire ONLY projA.
	if err := Unwire("claude-code", projA); err != nil {
		t.Fatalf("Unwire projA: %v", err)
	}

	// projA restored byte-exact; its record gone.
	if got := readFile(t, filepath.Join(projA, "CLAUDE.md")); got != origA {
		t.Fatalf("unwire projA must restore projA byte-exact.\nwant %q\ngot  %q", origA, got)
	}
	// projB must be UNTOUCHED — still wired, still carrying its block.
	if got := readFile(t, filepath.Join(projB, "CLAUDE.md")); !strings.Contains(got, "essaim:rules:begin") {
		t.Fatalf("unwire projA must NOT touch projB's managed block:\n%s", got)
	}
	c, _ = config.Load()
	if n := countTool(c, "claude-code"); n != 1 {
		t.Fatalf("after unwiring only projA exactly one claude-code record must remain, got %d: %+v", n, c.WiredTools)
	}
	// The surviving record must be projB's.
	rec, found := findWiredTool(c, "claude-code", filepath.Join(projB, "CLAUDE.md"))
	if !found || rec.NativeFile != filepath.Join(projB, "CLAUDE.md") {
		t.Fatalf("the surviving record must be projB's, got found=%v rec=%+v", found, rec)
	}
}

func countTool(c config.Config, name string) int {
	n := 0
	for _, w := range c.WiredTools {
		if w.Name == name {
			n++
		}
	}
	return n
}

// unwiring a never-wired NATIVE file that has NO essaim block must not touch it.
func TestUnwireNeverWiredNativeFileUntouched(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))
	target := filepath.Join(dir, "CLAUDE.md")
	const userOnly = "# Just my notes\n\nno essaim here\n"
	writeFile(t, target, userOnly)

	// claude-code was never wired in this config.
	if err := Unwire("claude-code", dir); err != nil {
		t.Fatalf("Unwire of a never-wired tool must be a clean no-op: %v", err)
	}
	got := readFile(t, target)
	if got != userOnly {
		t.Fatalf("unwire of a never-wired tool must leave an unrelated user file byte-untouched.\nwant %q\ngot  %q", userOnly, got)
	}
}
