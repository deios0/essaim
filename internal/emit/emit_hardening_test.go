package emit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"essaim/internal/rules"
)

// Bridge finding A: .essaim.bak must hold the PRISTINE original even after many
// emits (it once overwrote the backup with a with-block version → restore would
// re-inject a stale block instead of cleaning).
func TestBackupKeepsPristineOriginalAcrossEmits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CLAUDE.md")
	pristine := "# My rules\nUser content here.\n"
	if err := os.WriteFile(path, []byte(pristine), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writeRenameFencedWithBackup(path, rules.WrapBlock("- [H] r1: do A")); err != nil {
		t.Fatal(err)
	}
	if err := writeRenameFencedWithBackup(path, rules.WrapBlock("- [H] r2: do B")); err != nil {
		t.Fatal(err)
	}

	bak, err := os.ReadFile(path + ".essaim.bak")
	if err != nil {
		t.Fatal(err)
	}
	if string(bak) != pristine {
		t.Fatalf("backup must stay PRISTINE across emits, got:\n%q", string(bak))
	}
	live, _ := os.ReadFile(path)
	if !strings.Contains(string(live), "do B") || !strings.Contains(string(live), "User content here.") {
		t.Fatalf("live file must carry latest block + user content:\n%q", string(live))
	}
}

// Bridge finding B: a rule body that quotes the END sentinel must NOT corrupt the
// fence (the region-replacer keys on the first END; an un-defanged body END would
// orphan the real one and mangle the user's file).
func TestSentinelInBodyDoesNotCorruptNativeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CLAUDE.md")
	user := "# Header\nimportant user line\n"
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}

	block := rules.WrapBlock("- [H] meta: the end marker is " + rules.ESSAIM_END + " written literally")
	if err := writeRenameFencedWithBackup(path, block); err != nil {
		t.Fatal(err)
	}

	s := string(mustRead(t, path))
	if !strings.Contains(s, "important user line") {
		t.Fatalf("user content lost:\n%q", s)
	}
	if n := strings.Count(s, rules.ESSAIM_BEGIN); n != 1 {
		t.Fatalf("want exactly 1 BEGIN sentinel, got %d:\n%q", n, s)
	}
	if n := strings.Count(s, rules.ESSAIM_END); n != 1 {
		t.Fatalf("want exactly 1 END sentinel (body END must be defanged), got %d:\n%q", n, s)
	}
	reg, ok := extractFencedRegion(s)
	if !ok || !strings.HasPrefix(reg, rules.ESSAIM_BEGIN) || !strings.HasSuffix(reg, rules.ESSAIM_END) {
		t.Fatalf("fenced region malformed: ok=%v reg=%q", ok, reg)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Pre-public hardening (review follow-up #2): an emit must PRESERVE the existing
// file's mode. atomicWrite used os.CreateTemp (0600 by default), so a 0644
// git-tracked/shared AGENTS.md silently became 0600 after the first emit —
// harmless for content, surprising for a shared file. The temp must be chmod'd to
// the target's existing mode before the rename.
func TestEmitPreservesExistingFileMode(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o600, 0o664} {
		dir := t.TempDir()
		path := filepath.Join(dir, "AGENTS.md")
		if err := os.WriteFile(path, []byte("# rules\nuser content\n"), mode); err != nil {
			t.Fatal(err)
		}
		// os.WriteFile is subject to umask; normalize so the test asserts against the
		// mode the file ACTUALLY has on disk, then prove emit does not change it.
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		before := statMode(t, path)

		if err := writeRenameFencedWithBackup(path, rules.WrapBlock("- [H] r1: do A")); err != nil {
			t.Fatal(err)
		}
		if after := statMode(t, path); after != before {
			t.Errorf("emit changed file mode: before=%v after=%v (want preserved)", before, after)
		}
		// A second emit (which goes through the existing-region path) must also keep it.
		if err := writeRenameFencedWithBackup(path, rules.WrapBlock("- [H] r2: do B")); err != nil {
			t.Fatal(err)
		}
		if after := statMode(t, path); after != before {
			t.Errorf("second emit changed file mode: before=%v after=%v", before, after)
		}
	}
}

// A NEW (previously absent) native file must be created with a sane default mode
// (0644 modulo umask), NOT the 0600 os.CreateTemp default — a shared instruction
// file should be group/other-readable like a normal repo file.
func TestEmitNewFileSaneDefaultMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	if err := writeRenameFencedWithBackup(path, rules.WrapBlock("- [H] r1: do A")); err != nil {
		t.Fatal(err)
	}
	got := statMode(t, path)
	// Expect the world/group read bits present (0644-ish), not the restrictive 0600.
	if got&0o044 == 0 {
		t.Errorf("new file mode %v is too restrictive; want a 0644-style default (group/other readable)", got)
	}
}

func statMode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// Pre-public hardening (review follow-up #4): EmitNowWithResults must report the
// per-target outcome — written, then an idempotent SKIP on the unchanged re-emit,
// and a REFUSED for a credential-pathed target — so the CLI can stop claiming
// "wrote N rules" for targets it did not write.
func TestEmitNowWithResultsReportsPerTargetStatus(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "AGENTS.md")
	// A target path that itself trips the credential pattern → must be refused.
	bad := filepath.Join(dir, "AKIAIOSFODNN7EXAMPLE.md")

	em := New(rules.GuardConfig{}, []Tool{
		{Name: "codex", NativeFile: good},
		{Name: "evil", NativeFile: bad},
	})
	em.SetDebounce(0)
	ix := rules.BuildIndex([]rules.Rule{{
		ID: "r", Title: "T", Body: "B", Kind: "guardrail", Weight: 0.9, Confidence: 0.9, Status: "live",
	}})

	_, results, err := em.EmitNowWithResults(ix)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]TargetStatus{}
	for _, r := range results {
		got[r.Name] = r.Status
	}
	if got["codex"] != StatusWritten {
		t.Errorf("codex should be StatusWritten, got %v", got["codex"])
	}
	if got["evil"] != StatusRefused {
		t.Errorf("credential-pathed target should be StatusRefused, got %v", got["evil"])
	}
	if _, statErr := os.Stat(bad); statErr == nil {
		t.Errorf("a credential-pathed target must never be written")
	}

	// Second emit, nothing changed → the good target is an idempotent SKIP.
	_, results2, err := em.EmitNowWithResults(ix)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results2 {
		if r.Name == "codex" && r.Status != StatusSkipped {
			t.Errorf("unchanged re-emit should StatusSkipped codex, got %v", r.Status)
		}
	}
}
