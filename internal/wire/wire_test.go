package wire

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"oikos/internal/config"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestPlanKnownProxyTool(t *testing.T) {
	p, err := Resolve("cursor", "")
	if err != nil {
		t.Fatalf("Plan(cursor): %v", err)
	}
	if p.Channel != ChannelBaseURL {
		t.Fatalf("cursor channel = %q, want base_url", p.Channel)
	}
	if p.BaseURL != ProxyBaseURL {
		t.Fatalf("cursor base_url = %q, want %q", p.BaseURL, ProxyBaseURL)
	}
	// Cursor's honest caveat must be surfaced.
	if !strings.Contains(strings.ToLower(p.Note), "chat") {
		t.Fatalf("cursor plan should carry the chat-panel caveat, got note %q", p.Note)
	}
}

func TestPlanClaudeCodeIsNativeFileNeverBaseURL(t *testing.T) {
	p, err := Resolve("claude-code", "/home/u/proj")
	if err != nil {
		t.Fatalf("Plan(claude-code): %v", err)
	}
	if p.Channel != ChannelNativeFile {
		t.Fatalf("claude-code channel = %q, want native_file (NEVER base_url — v1 has no /v1/messages)", p.Channel)
	}
	if p.BaseURL != "" {
		t.Fatalf("claude-code must NOT set a base_url (would brick it), got %q", p.BaseURL)
	}
	if filepath.Base(p.NativeFile) != "CLAUDE.md" {
		t.Fatalf("claude-code native file = %q, want .../CLAUDE.md", p.NativeFile)
	}
	if !filepath.IsAbs(p.NativeFile) {
		t.Fatalf("native file path must be absolute, got %q", p.NativeFile)
	}
}

func TestPlanContinueShortcut(t *testing.T) {
	p, err := Resolve("continue", "")
	if err != nil {
		t.Fatalf("Plan(continue): %v", err)
	}
	if p.Channel != ChannelBaseURL {
		t.Fatalf("continue channel = %q, want base_url", p.Channel)
	}
}

func TestPlanUnknownToolFallsBackToGenericEnvExport(t *testing.T) {
	p, err := Resolve("some-random-tool", "")
	if err != nil {
		t.Fatalf("Plan(unknown): %v", err)
	}
	if p.Channel != ChannelBaseURL {
		t.Fatalf("unknown tool should default to the generic base_url env-export, got %q", p.Channel)
	}
	if p.BaseURL != ProxyBaseURL {
		t.Fatalf("unknown tool base_url = %q, want %q", p.BaseURL, ProxyBaseURL)
	}
}

func TestPlanEmptyToolErrors(t *testing.T) {
	if _, err := Resolve("", ""); err == nil {
		t.Fatal("Plan with an empty tool name must error")
	}
}

func TestEnvExportFormIsCopyPasteable(t *testing.T) {
	p, _ := Resolve("aider", "")
	got := p.EnvExport()
	if !strings.Contains(got, "OPENAI_BASE_URL=") {
		t.Fatalf("env export must set OPENAI_BASE_URL, got:\n%s", got)
	}
	if !strings.Contains(got, ProxyBaseURL) {
		t.Fatalf("env export must point at the proxy %q, got:\n%s", ProxyBaseURL, got)
	}
	// The /v1 suffix is what OpenAI clients expect on a base_url.
	if !strings.Contains(got, ProxyBaseURL+"/v1") {
		t.Fatalf("env export base_url should include the /v1 suffix, got:\n%s", got)
	}
}

func TestApplyProxyToolPersistsWiringIdempotently(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OIKOS_CONFIG", filepath.Join(dir, "config.json"))

	p, _ := Resolve("cursor", "")
	if _, err := Apply(p); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Apply twice — must not duplicate.
	if _, err := Apply(p); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	c, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	n := 0
	for _, w := range c.WiredTools {
		if w.Name == "cursor" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("cursor wired %d times in config, want exactly 1 (idempotent)", n)
	}
	if c.WiredTools[0].Channel != "base_url" {
		t.Fatalf("persisted channel = %q, want base_url", c.WiredTools[0].Channel)
	}
}

func TestApplyNativeFileCreatesManagedBlockAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OIKOS_CONFIG", filepath.Join(dir, "config.json"))

	p, err := Resolve("claude-code", dir)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, err := Apply(p); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// The native file should now exist with the managed sentinels so the emitter
	// has an anchor to write into.
	b := readFile(t, p.NativeFile)
	if !strings.Contains(b, "oikos:rules:begin") || !strings.Contains(b, "oikos:rules:end") {
		t.Fatalf("native file should contain the managed oikos block sentinels, got:\n%s", b)
	}

	// Apply again — must not append a second managed block.
	if _, err := Apply(p); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	b2 := readFile(t, p.NativeFile)
	if got := strings.Count(b2, "oikos:rules:begin"); got != 1 {
		t.Fatalf("native file has %d managed blocks after re-apply, want exactly 1 (idempotent)", got)
	}

	c, _ := config.Load()
	if len(c.WiredTools) != 1 || c.WiredTools[0].Channel != "native_file" {
		t.Fatalf("claude-code should be persisted once as native_file, got %+v", c.WiredTools)
	}
}

// TestJoinNativeWindowsAbsolutePath asserts that anchoring a native-file tool
// under a Windows-style absolute dir keeps the drive letter and joins with a
// backslash, producing a Windows path even when the test runs on Linux. The pure
// joiner is platform-injectable so the behavior is provable on any host.
func TestJoinNativeWindowsAbsolutePath(t *testing.T) {
	dir := `C:\Users\denis\proj`
	got := joinNativePath(dir, "CLAUDE.md", true)
	want := `C:\Users\denis\proj\CLAUDE.md`
	if got != want {
		t.Fatalf("windows native path = %q, want %q", got, want)
	}
}

// TestJoinNativeWindowsLocalAppData covers a %LOCALAPPDATA%-rooted dir (the
// no-admin Programs dir the .ps1 installer uses) — the backslash join must hold.
func TestJoinNativeWindowsLocalAppData(t *testing.T) {
	dir := `C:\Users\denis\AppData\Local\Programs\oikos`
	got := joinNativePath(dir, "AGENTS.md", true)
	want := `C:\Users\denis\AppData\Local\Programs\oikos\AGENTS.md`
	if got != want {
		t.Fatalf("windows localappdata native path = %q, want %q", got, want)
	}
}

// TestJoinNativeAlreadyHasTrailingSep guards the dir that already ends in a
// separator (no doubled backslash).
func TestJoinNativeAlreadyHasTrailingSep(t *testing.T) {
	got := joinNativePath(`C:\Users\denis\proj\`, "CLAUDE.md", true)
	want := `C:\Users\denis\proj\CLAUDE.md`
	if got != want {
		t.Fatalf("trailing-sep windows native path = %q, want %q", got, want)
	}
}

func TestApplyNativeFilePreservesExistingUserContent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OIKOS_CONFIG", filepath.Join(dir, "config.json"))
	target := filepath.Join(dir, "CLAUDE.md")
	writeFile(t, target, "# My Project\n\nMy own rules. oikos must never touch these.\n")

	p, _ := Resolve("claude-code", dir)
	if _, err := Apply(p); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	b := readFile(t, target)
	if !strings.Contains(b, "My own rules. oikos must never touch these.") {
		t.Fatalf("Apply clobbered the user's existing content:\n%s", b)
	}
	if !strings.Contains(b, "oikos:rules:begin") {
		t.Fatalf("Apply did not add the managed block:\n%s", b)
	}
}
