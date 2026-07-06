package main

import (
	"testing"

	"essaim/internal/config"
)

func TestVersionString(t *testing.T) {
	if version == "" {
		t.Fatal("version must be set")
	}
}

// P2-3: a native-file wired-tool path that CONTAINS a comma must survive the
// config→emit-targets resolution intact. The pre-fix code round-tripped the tools
// through the comma-delimited ESSAIM_NATIVE_FILE_TOOLS env var, so a comma in a
// path (a Windows dir, a quoted path) silently split into a bogus extra tool and
// dropped the real one. Resolving directly off the config carries the comma path
// through byte-exact.
func TestNativeFileToolsCommaInPathSurvives(t *testing.T) {
	// A realistic comma-bearing path plus an ordinary one alongside it.
	commaPath := `/home/u/My Projects, Drafts/CLAUDE.md`
	cfg := config.Config{
		WiredTools: []config.WiredTool{
			{Name: "claude-code", Channel: "native_file", NativeFile: commaPath},
			{Name: "codex", Channel: "native_file", NativeFile: "/home/u/AGENTS.md"},
			{Name: "cursor", Channel: "base_url"}, // must be skipped (no native file)
		},
	}

	// No env override → resolve directly from config.
	t.Setenv("ESSAIM_NATIVE_FILE_TOOLS", "")

	got := nativeFileToolsForServe(cfg)
	if len(got) != 2 {
		t.Fatalf("want 2 native-file tools (base_url skipped), got %d: %+v", len(got), got)
	}

	byName := map[string]string{}
	for _, tl := range got {
		byName[tl.Name] = tl.NativeFile
	}
	if byName["claude-code"] != commaPath {
		t.Fatalf("comma-containing path was corrupted:\n got: %q\nwant: %q", byName["claude-code"], commaPath)
	}
	if byName["codex"] != "/home/u/AGENTS.md" {
		t.Fatalf("second tool path wrong: %q", byName["codex"])
	}
	if _, ok := byName["cursor"]; ok {
		t.Fatal("base_url tool must not appear as a native-file target")
	}
}

// The pre-fix comma-join is exactly what dropped a tool: prove the old encoding is
// lossy so the fix (direct config resolution) is the thing being relied on. This
// documents WHY we no longer round-trip through the comma-delimited env format.
func TestCommaJoinEnvFormatIsLossy(t *testing.T) {
	commaPath := `/home/u/My Projects, Drafts/CLAUDE.md`
	// Simulate the OLD encoding: name=path joined by commas.
	joined := "claude-code=" + commaPath + ",codex=/home/u/AGENTS.md"
	t.Setenv("ESSAIM_NATIVE_FILE_TOOLS", joined)

	parsed := nativeFileToolsFromEnv()
	// The comma-join splits the single claude-code path into two fragments, so the
	// re-parse can NEVER reproduce the original comma path. This is the bug the P2-3
	// fix avoids by not using this encoding for the config→serve path.
	for _, tl := range parsed {
		if tl.Name == "claude-code" && tl.NativeFile == commaPath {
			t.Fatal("unexpected: the comma-join format round-tripped a comma path (bug would be gone at the env layer too)")
		}
	}
}
