package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestZeroValueIsEmpty(t *testing.T) {
	var c Config
	if !c.IsEmpty() {
		t.Fatal("zero-value Config must report IsEmpty (first-run)")
	}
	c.VaultDir = "/tmp/vault"
	if c.IsEmpty() {
		t.Fatal("a Config with a vault dir is not empty")
	}
	var c2 Config
	c2.Provider = "local"
	if c2.IsEmpty() {
		t.Fatal("a Config with a provider is not empty")
	}
	var c3 Config
	c3.WiredTools = []WiredTool{{Name: "cursor"}}
	if c3.IsEmpty() {
		t.Fatal("a Config with a wired tool is not empty")
	}
}

func TestPathHonorsEnvOverride(t *testing.T) {
	want := "/tmp/custom/essaim.json"
	t.Setenv("ESSAIM_CONFIG", want)
	got, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if got != want {
		t.Fatalf("Path with ESSAIM_CONFIG override = %q, want %q", got, want)
	}
}

func TestPathDefaultUnderConfigDir(t *testing.T) {
	// Clear the override so we exercise the default-dir branch.
	t.Setenv("ESSAIM_CONFIG", "")
	os.Unsetenv("ESSAIM_CONFIG")
	got, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	base := filepath.Base(got)
	if base != "config.json" {
		t.Fatalf("default config filename = %q, want config.json", base)
	}
	if filepath.Base(filepath.Dir(got)) != "essaim" {
		t.Fatalf("default config dir must be .../essaim/, got %q", got)
	}
}

func TestLoadMissingReturnsEmptyNotError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "nope", "config.json"))
	c, err := Load()
	if err != nil {
		t.Fatalf("Load of a missing file must not error, got %v", err)
	}
	if !c.IsEmpty() {
		t.Fatal("Load of a missing file must return an empty (first-run) Config")
	}
}

func TestSaveThenLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "sub", "config.json"))

	want := Config{
		Provider: "openrouter",
		VaultDir: "/home/u/vault",
		WiredTools: []WiredTool{
			{Name: "cursor", Channel: "base_url"},
			{Name: "claude-code", Channel: "native_file", NativeFile: "/home/u/CLAUDE.md"},
		},
	}
	if err := Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Provider != want.Provider || got.VaultDir != want.VaultDir {
		t.Fatalf("round-trip scalar mismatch: got %+v want %+v", got, want)
	}
	if len(got.WiredTools) != len(want.WiredTools) {
		t.Fatalf("round-trip tools len = %d, want %d", len(got.WiredTools), len(want.WiredTools))
	}
	for i := range want.WiredTools {
		if got.WiredTools[i] != want.WiredTools[i] {
			t.Fatalf("tool[%d] = %+v, want %+v", i, got.WiredTools[i], want.WiredTools[i])
		}
	}
}

func TestSaveIsPrivateFilePerm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file mode not meaningful on windows")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	t.Setenv("ESSAIM_CONFIG", p)
	if err := Save(Config{Provider: "local"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config file perm = %o, want 600 (it may hold a key location)", perm)
	}
}

// TestResolvePathWindowsAppData asserts that on Windows the config lands under
// %AppData%\essaim\config.json — the no-admin, per-user roaming dir — using
// backslash-joined Windows-style paths. resolvePath is platform-injectable so
// this runs on any host (the real Path() defers to os.UserConfigDir).
func TestResolvePathWindowsAppData(t *testing.T) {
	appData := `C:\Users\denis\AppData\Roaming`
	env := func(k string) string {
		if k == "ESSAIM_CONFIG" {
			return ""
		}
		return ""
	}
	got, err := resolvePath(env, func() (string, error) {
		// Mimic os.UserConfigDir on Windows: it returns %AppData%.
		return appData, nil
	}, "\\")
	if err != nil {
		t.Fatalf("resolvePath: %v", err)
	}
	want := appData + `\essaim\config.json`
	if got != want {
		t.Fatalf("windows config path = %q, want %q", got, want)
	}
}

// TestResolvePathHonorsWindowsEnvOverride asserts a Windows-style absolute path
// passed via ESSAIM_CONFIG is returned verbatim (drive letter + backslashes
// preserved), so a corp deployment can pin the store under %LOCALAPPDATA%.
func TestResolvePathHonorsWindowsEnvOverride(t *testing.T) {
	want := `C:\Users\denis\AppData\Local\Programs\essaim\config.json`
	env := func(k string) string {
		if k == "ESSAIM_CONFIG" {
			return want
		}
		return ""
	}
	got, err := resolvePath(env, func() (string, error) {
		return "", errUnusedConfigDir
	}, "\\")
	if err != nil {
		t.Fatalf("resolvePath: %v", err)
	}
	if got != want {
		t.Fatalf("windows ESSAIM_CONFIG override = %q, want %q", got, want)
	}
}

// errUnusedConfigDir is returned by a userConfigDir stub that must not be called
// (the env override short-circuits before it).
var errUnusedConfigDir = errSentinel("userConfigDir must not be called when ESSAIM_CONFIG is set")

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

func TestUpsertToolIsIdempotent(t *testing.T) {
	var c Config
	c = c.UpsertTool(WiredTool{Name: "cursor", Channel: "base_url"})
	c = c.UpsertTool(WiredTool{Name: "cursor", Channel: "base_url"})
	if len(c.WiredTools) != 1 {
		t.Fatalf("UpsertTool added a duplicate: %d tools", len(c.WiredTools))
	}
	// Re-upserting the same base_url tool (no NativeFile) updates in place. A
	// base_url tool has no per-project native file, so its identity is its name.
	c = c.UpsertTool(WiredTool{Name: "cursor", Channel: "base_url"})
	if len(c.WiredTools) != 1 {
		t.Fatalf("UpsertTool of an existing base_url tool must update in place, got %d tools", len(c.WiredTools))
	}
	c = c.UpsertTool(WiredTool{Name: "claude-code", Channel: "native_file", NativeFile: "/a/CLAUDE.md"})
	if len(c.WiredTools) != 2 {
		t.Fatalf("UpsertTool of a new name must append, got %d tools", len(c.WiredTools))
	}
}

// P1-BUG-1: a native-file tool wired in TWO different projects must produce TWO
// records, keyed by (Name + NativeFile). The second wire of claude-code at a
// different path must NOT replace the first project's record (which would orphan
// projA's managed block forever).
func TestUpsertToolPerProjectNativeFileCoexist(t *testing.T) {
	var c Config
	c = c.UpsertTool(WiredTool{Name: "claude-code", Channel: "native_file", NativeFile: "/projA/CLAUDE.md"})
	c = c.UpsertTool(WiredTool{Name: "claude-code", Channel: "native_file", NativeFile: "/projB/CLAUDE.md"})
	if len(c.WiredTools) != 2 {
		t.Fatalf("wiring claude-code in two projects must keep two records, got %d: %+v", len(c.WiredTools), c.WiredTools)
	}
	// projA's record must still be present and unchanged.
	var foundA bool
	for _, w := range c.WiredTools {
		if w.NativeFile == "/projA/CLAUDE.md" {
			foundA = true
		}
	}
	if !foundA {
		t.Fatalf("wiring projB must NOT clobber projA's record: %+v", c.WiredTools)
	}
	// Re-wiring projA (same name + same path) updates in place — no duplicate.
	c = c.UpsertTool(WiredTool{Name: "claude-code", Channel: "native_file", NativeFile: "/projA/CLAUDE.md"})
	if len(c.WiredTools) != 2 {
		t.Fatalf("re-wiring the SAME project must update in place, got %d: %+v", len(c.WiredTools), c.WiredTools)
	}
}

// P2: Update serializes the read-modify-write cycle so N goroutines each adding a
// DISTINCT tool all survive. A bare Load→modify→Save (the pre-fix path) races —
// two goroutines read the same base config and the second Save clobbers the
// first, losing an update. Under Update every one of the N writes must land.
func TestUpdateConcurrentAddsAllSurvive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))

	const n = 25
	var wg sync.WaitGroup
	wg.Add(n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("tool-%02d", i)
			err := Update(func(c *Config) error {
				*c = c.UpsertTool(WiredTool{
					Name:       "claude-code",
					Channel:    "native_file",
					NativeFile: "/proj/" + name + "/CLAUDE.md",
				})
				return nil
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Update: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.WiredTools) != n {
		t.Fatalf("concurrent Update lost writes: got %d tools, want %d", len(got.WiredTools), n)
	}
	// Every distinct tool must be present (no lost update, no duplicate).
	seen := make(map[string]bool, n)
	for _, w := range got.WiredTools {
		if seen[w.NativeFile] {
			t.Fatalf("duplicate wired tool %q", w.NativeFile)
		}
		seen[w.NativeFile] = true
	}
	for i := 0; i < n; i++ {
		want := fmt.Sprintf("/proj/tool-%02d/CLAUDE.md", i)
		if !seen[want] {
			t.Fatalf("lost update: tool %q is missing from the saved config", want)
		}
	}
}

// P2: Update must NOT save when the mutate callback returns an error — the config
// on disk is left exactly as it was.
func TestUpdateMutateErrorDoesNotSave(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))

	// Seed a known state.
	if err := Save(Config{Provider: "local", VaultDir: "/v"}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	sentinel := fmt.Errorf("mutate failed")
	err := Update(func(c *Config) error {
		c.Provider = "openrouter" // this mutation must be discarded
		return sentinel
	})
	if err != sentinel {
		t.Fatalf("Update must return the mutate error, got %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Provider != "local" || got.VaultDir != "/v" {
		t.Fatalf("Update saved despite a mutate error: got %+v", got)
	}
}
