package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"essaim/internal/config"
	"essaim/internal/server"
)

func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

func TestRunWireRequiresToolArg(t *testing.T) {
	var out bytes.Buffer
	if err := runWire(nil, &out); err == nil {
		t.Fatal("runWire with no tool must error")
	}
}

func TestRunWireCursorPrintsEnvExportAndPersists(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))

	var out bytes.Buffer
	if err := runWire([]string{"cursor"}, &out); err != nil {
		t.Fatalf("runWire(cursor): %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "OPENAI_BASE_URL=http://127.0.0.1:4141/v1") {
		t.Fatalf("wire cursor output missing the env-export line:\n%s", s)
	}
	c, _ := config.Load()
	if len(c.WiredTools) != 1 || c.WiredTools[0].Name != "cursor" {
		t.Fatalf("cursor not persisted: %+v", c.WiredTools)
	}
}

func TestRunWireClaudeCodeIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))

	var out bytes.Buffer
	// Anchor the native file under the temp dir via --dir so the test never
	// touches the real cwd.
	args := []string{"--dir", dir, "claude-code"}
	if err := runWire(args, &out); err != nil {
		t.Fatalf("runWire(claude-code): %v", err)
	}
	if err := runWire(args, &out); err != nil {
		t.Fatalf("runWire(claude-code) 2nd: %v", err)
	}
	c, _ := config.Load()
	n := 0
	for _, w := range c.WiredTools {
		if w.Name == "claude-code" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("claude-code wired %d times, want 1 (idempotent)", n)
	}
	// The native file got exactly one managed block.
	b := readTestFile(t, filepath.Join(dir, "CLAUDE.md"))
	if got := strings.Count(b, "essaim:rules:begin"); got != 1 {
		t.Fatalf("CLAUDE.md has %d managed blocks, want 1", got)
	}
}

func TestRunUnwireRequiresToolArg(t *testing.T) {
	var out bytes.Buffer
	if err := runUnwire(nil, &out); err == nil {
		t.Fatal("runUnwire with no tool must error")
	}
}

func TestRunUnwireRestoresAndRemovesRecord(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))
	target := filepath.Join(dir, "CLAUDE.md")
	const original = "# Mine\n\nUntouchable.\n"
	if err := writeTestFile(target, original); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	var out bytes.Buffer
	args := []string{"--dir", dir, "claude-code"}
	if err := runWire(args, &out); err != nil {
		t.Fatalf("runWire(claude-code): %v", err)
	}
	// Now unwire — the file must come back byte-exact and the record must be gone.
	if err := runUnwire(args, &out); err != nil {
		t.Fatalf("runUnwire(claude-code): %v", err)
	}
	if got := readTestFile(t, target); got != original {
		t.Fatalf("unwire must restore the original byte-exact.\nwant %q\ngot  %q", original, got)
	}
	c, _ := config.Load()
	for _, w := range c.WiredTools {
		if w.Name == "claude-code" {
			t.Fatalf("unwire must remove the record, still present: %+v", c.WiredTools)
		}
	}
	// Idempotent: a second unwire is a clean no-op.
	if err := runUnwire(args, &out); err != nil {
		t.Fatalf("second runUnwire must be a clean no-op: %v", err)
	}
}

// P1-BUG-2: unwiring a base_url tool whose config location essaim does NOT know
// (cursor stores config in an internal SQLite store) must NOT claim "original
// config restored" — it must print an honest manual-recovery hint so a user left
// pointing at the dead proxy knows to fix it.
func TestRunUnwireBaseURLUnknownConfigPrintsHint(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))

	var out bytes.Buffer
	if err := runWire([]string{"cursor"}, &out); err != nil {
		t.Fatalf("runWire(cursor): %v", err)
	}
	out.Reset()
	if err := runUnwire([]string{"cursor"}, &out); err != nil {
		t.Fatalf("runUnwire(cursor): %v", err)
	}
	s := out.String()
	if strings.Contains(s, "original config restored") {
		t.Fatalf("unwire of a base_url tool with an unknown config location must NOT claim the config was restored:\n%s", s)
	}
	// It must say something actionable about checking the base_url by hand.
	if !strings.Contains(strings.ToLower(s), "base") && !strings.Contains(strings.ToLower(s), "manually") {
		t.Fatalf("unwire must print a manual-recovery hint for an unknown config location:\n%s", s)
	}
}

// P1-BUG-2: unwiring a base_url tool whose config essaim DID auto-restore must say
// so honestly (it removed the proxy URL), pointing at the file it fixed.
func TestRunUnwireBaseURLRestoredPrintsHonestMessage(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(root, "config.json"))
	fakeHome := filepath.Join(root, "home")
	contDir := filepath.Join(fakeHome, ".continue")
	if err := os.MkdirAll(contDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)

	var out bytes.Buffer
	if err := runWire([]string{"continue"}, &out); err != nil {
		t.Fatalf("runWire(continue): %v", err)
	}
	// Simulate the heal watcher having written the proxy URL.
	cfgPath := filepath.Join(contDir, "config.json")
	if err := writeTestFile(cfgPath, `{"models":[{"apiBase":"http://127.0.0.1:4141/v1"}]}`); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runUnwire([]string{"continue"}, &out); err != nil {
		t.Fatalf("runUnwire(continue): %v", err)
	}
	s := out.String()
	if !strings.Contains(s, ".continue") {
		t.Fatalf("honest restore message must name the config file essaim fixed:\n%s", s)
	}
	// And the file no longer points at the proxy.
	got := readTestFile(t, cfgPath)
	if strings.Contains(got, "127.0.0.1:4141") {
		t.Fatalf("unwire must have removed the proxy URL:\n%s", got)
	}
}

func TestFirstRunBannerMentionsSetupURL(t *testing.T) {
	banner := firstRunBanner()
	if !strings.Contains(banner, "http://127.0.0.1:4141/setup") {
		t.Fatalf("first-run banner must point at the setup URL:\n%s", banner)
	}
}

// The first-run banner must LEAD with the wedge pitch — the same exact sentence
// the /setup page renders (one source of truth: server.WedgePitch). This is the
// first message a fresh user sees on `essaim serve`, so the differentiator lands
// before anything else.
func TestFirstRunBannerLeadsWithWedgePitch(t *testing.T) {
	banner := firstRunBanner()
	if !strings.Contains(banner, server.WedgePitch) {
		t.Fatalf("first-run banner must carry the wedge pitch %q:\n%s", server.WedgePitch, banner)
	}
	// "leads with": the pitch must appear BEFORE the setup URL box.
	if strings.Index(banner, server.WedgePitch) > strings.Index(banner, "/setup") {
		t.Fatalf("the wedge pitch must come before the setup URL in the banner:\n%s", banner)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := readFileBytes(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
