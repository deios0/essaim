package wire

import (
	"runtime"
	"testing"

	"oikos/internal/config"
)

// Deferred-7: nativeFileEqual compares NativeFile paths case-insensitively on
// Windows (case-insensitive FS) and case-sensitively elsewhere. We test the
// helper's logic directly, guarded to the running OS so the assertion matches the
// platform semantics.
func TestNativeFileEqualCaseSensitivity(t *testing.T) {
	// An exact match is equal on every OS.
	if !nativeFileEqual("/a/CLAUDE.md", "/a/CLAUDE.md") {
		t.Fatal("identical paths must be equal on every OS")
	}
	// A totally different path is never equal.
	if nativeFileEqual("/a/CLAUDE.md", "/b/CLAUDE.md") {
		t.Fatal("different paths must never be equal")
	}

	// A casing-only difference: equal on Windows (case-insensitive FS), NOT equal
	// on case-sensitive OSes (they are genuinely different files there).
	casingDiff := nativeFileEqual(`C:\Proj\CLAUDE.md`, `c:\proj\claude.md`)
	if runtime.GOOS == "windows" {
		if !casingDiff {
			t.Fatal("on Windows a casing-only path difference must be treated as EQUAL (case-insensitive FS)")
		}
	} else {
		if casingDiff {
			t.Fatal("on a case-sensitive OS a casing-only path difference must be treated as DIFFERENT")
		}
	}
}

// Deferred-7 end-to-end: findWiredTool must locate a record whose stored path
// differs only in casing from the lookup path, on the OS where the FS is
// case-insensitive. On case-sensitive OSes it must NOT match (correct there).
func TestFindWiredToolCaseInsensitiveOnWindows(t *testing.T) {
	c := config.Config{
		WiredTools: []config.WiredTool{
			{Name: "claude-code", Channel: "native_file", NativeFile: `C:\Proj\CLAUDE.md`},
		},
	}
	_, found := findWiredTool(c, "claude-code", `c:\proj\claude.md`)
	if runtime.GOOS == "windows" {
		if !found {
			t.Fatal("on Windows, findWiredTool must match a casing-different path (else the record orphans)")
		}
	} else if found {
		t.Fatal("on a case-sensitive OS, a casing-different path must NOT match")
	}

	// The exact path always matches, on every OS.
	if _, ok := findWiredTool(c, "claude-code", `C:\Proj\CLAUDE.md`); !ok {
		t.Fatal("the exact recorded path must always match")
	}
}

// removeWiredTool must remove the record for a casing-different path on Windows,
// so `oikos unwire` there never leaves an orphaned record behind.
func TestRemoveWiredToolCaseInsensitiveOnWindows(t *testing.T) {
	c := config.Config{
		WiredTools: []config.WiredTool{
			{Name: "claude-code", Channel: "native_file", NativeFile: `C:\Proj\CLAUDE.md`},
			{Name: "codex", Channel: "native_file", NativeFile: `C:\Proj\AGENTS.md`},
		},
	}
	out := removeWiredTool(c, "claude-code", `c:\proj\claude.md`)
	if runtime.GOOS == "windows" {
		if len(out.WiredTools) != 1 {
			t.Fatalf("on Windows, removeWiredTool must drop the casing-different record; got %d tools", len(out.WiredTools))
		}
		if out.WiredTools[0].Name != "codex" {
			t.Fatalf("the unrelated tool must survive; got %+v", out.WiredTools)
		}
	} else {
		if len(out.WiredTools) != 2 {
			t.Fatalf("on a case-sensitive OS a casing-different path must NOT remove the record; got %d tools", len(out.WiredTools))
		}
	}
}
