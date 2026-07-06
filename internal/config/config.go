// Package config persists the small amount of first-run state oikos needs to
// remember between launches: which AI provider was chosen, which vault holds the
// corpus, and which tools have been wired. It is the store the /setup POST
// handlers write and `oikos serve` reads for first-run detection.
//
// Purity: the file is written ONLY by an explicit user action (the /setup POST
// or `oikos wire`). A binary that is installed and never configured writes
// nothing here. The store holds a key *location* (provider choice), never the
// key itself — secrets live in the OS keychain (internal/secret).
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// mu serializes the read-modify-write cycle (Load → mutate → Save) that the
// mutating callers (setup / wire / unwire) run. Without it two concurrent
// Load→modify→Save cycles race: both read the same base config, each applies its
// own change, and the second Save clobbers the first — a lost update (e.g. wiring
// two tools at once drops one). Update below holds this lock across the whole
// cycle so mutations are serialized IN-PROCESS. Cross-process coordination
// (two `oikos` binaries writing the same config.json at once) is out of scope —
// the atomic temp+rename in Save keeps each individual write from corrupting the
// file, but a lost update across processes would need file locking.
var mu sync.Mutex

// WiredTool records one tool oikos has pointed at the local proxy and the
// channel it was wired through ("base_url" for proxy-routed tools, "native_file"
// for tools that learn via a managed block in CLAUDE.md/AGENTS.md).
type WiredTool struct {
	Name string `json:"name"`
	// Channel is "base_url" or "native_file".
	Channel string `json:"channel"`
	// NativeFile is the absolute path to the managed native file (only set when
	// Channel == "native_file").
	NativeFile string `json:"native_file,omitempty"`
	// OriginalBaseURL is the base_url value the tool's IDE config held BEFORE oikos
	// first wired/healed it (only meaningful when Channel == "base_url"). `oikos
	// unwire` restores exactly this value so the user is returned to THEIR real
	// upstream (their own gateway, a non-OpenAI provider), not a hardcoded OpenAI
	// default. Empty means it was unknown at wire time (no config file yet, or the
	// tool was wired before this field existed) — unwire then falls back to the
	// vendor default with a hint.
	OriginalBaseURL string `json:"original_base_url,omitempty"`
}

// Config is the persisted first-run state. The zero value is the "first run,
// nothing chosen yet" state and reports IsEmpty.
type Config struct {
	// Provider is the chosen model backend: "openrouter" (a cloud key was
	// pasted) or "local" (an auto-detected Ollama/LM Studio is used). "" means
	// not yet chosen.
	Provider string `json:"provider,omitempty"`
	// VaultDir is the corpus directory (the OIKOS_VAULT the learning loop and
	// injection layer use). "" means not yet chosen.
	VaultDir string `json:"vault_dir,omitempty"`
	// WiredTools is the set of tools the user has wired to oikos.
	WiredTools []WiredTool `json:"wired_tools,omitempty"`
	// Bus is the opt-in aibus join (nil = not joined; default-off, no bus). It
	// records WHERE to reach the bus and WHICH zone key file to present — never
	// the raw key (the key stays in its file, e.g. ~/.bridge/keys/...).
	Bus *BusJoin `json:"bus,omitempty"`
}

// BusJoin is a persisted opt-in bus membership. The zone is informational only —
// the server derives and enforces the real zone from the key. KeyFile points at
// the existing zone key on disk; the raw key is never stored in config.json.
type BusJoin struct {
	URL     string `json:"url"`
	Zone    string `json:"zone,omitempty"`
	KeyFile string `json:"key_file,omitempty"`
}

// IsEmpty reports whether no setup has happened yet (first run). A binary that
// has only ever been installed returns true.
func (c Config) IsEmpty() bool {
	return c.Provider == "" && c.VaultDir == "" && len(c.WiredTools) == 0 && c.Bus == nil
}

// identity is the key a WiredTool is deduplicated by. A native-file tool is keyed
// by (Name + NativeFile) so the SAME tool wired in two different projects yields
// two coexisting records — each keeps its own managed block maintained (P1-BUG-1;
// keying by Name alone made a second `wire claude-code` in projB REPLACE projA's
// record and orphan projA's CLAUDE.md block forever). A base_url tool has no
// per-project file, so its identity is its Name (NativeFile is "").
func (t WiredTool) identity() (string, string) { return t.Name, t.NativeFile }

// UpsertTool returns a copy of c with t added or updated in place by its wiring
// identity — (Name + NativeFile) for native-file tools, Name alone for base_url
// tools. It is idempotent: wiring the same tool at the same path twice never
// produces a duplicate. Wiring the same native-file tool at a DIFFERENT path adds
// a new record (per-project coexistence). The receiver is not mutated.
func (c Config) UpsertTool(t WiredTool) Config {
	out := c
	out.WiredTools = make([]WiredTool, len(c.WiredTools))
	copy(out.WiredTools, c.WiredTools)
	tn, tf := t.identity()
	for i := range out.WiredTools {
		if n, f := out.WiredTools[i].identity(); n == tn && f == tf {
			out.WiredTools[i] = t
			return out
		}
	}
	out.WiredTools = append(out.WiredTools, t)
	return out
}

// Path resolves the config file path. OIKOS_CONFIG overrides it outright (used
// by tests and headless/server deployments). Otherwise it is
// <os.UserConfigDir>/oikos/config.json — a cross-platform location (no
// hostname/machine-id derivation, no platform-divergent code; spec N11).
//
// os.UserConfigDir resolves the right per-user, no-admin base on every OS:
// %AppData% (Roaming) on Windows, $XDG_CONFIG_HOME (or ~/.config) on Linux,
// ~/Library/Application Support on macOS. filepath.Join then uses the platform
// separator, so Windows paths come back backslash-joined under a drive letter
// without any platform-divergent code here.
func Path() (string, error) {
	return resolvePath(os.Getenv, os.UserConfigDir, string(filepath.Separator))
}

// resolvePath is the platform-injectable core of Path. getenv reads the
// OIKOS_CONFIG override; userConfigDir supplies the per-user base; sep is the
// path separator to join with ("\\" on Windows, "/" elsewhere). Splitting it out
// lets a test prove the Windows %AppData%\oikos\config.json layout on any host.
func resolvePath(getenv func(string) string, userConfigDir func() (string, error), sep string) (string, error) {
	if p := getenv("OIKOS_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := userConfigDir()
	if err != nil {
		return "", err
	}
	// Join with the supplied separator (filepath.Join would always use the host
	// separator, which on a Linux test host can't model a Windows result).
	dir = strings.TrimRight(dir, sep)
	return dir + sep + "oikos" + sep + "config.json", nil
}

// Load reads the persisted config. A missing file is NOT an error — it returns a
// zero (empty) Config so first-run detection works on a clean machine.
func Load() (Config, error) {
	p, err := Path()
	if err != nil {
		return Config{}, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil // first run: no file yet
		}
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Save persists c atomically with 0600 perms (the file records a provider
// choice / vault location). It creates the parent directory as needed. The write
// is atomic (temp file + rename) so a crash mid-write never truncates a good
// config.
func Save(c Config) error {
	p, err := Path()
	if err != nil {
		return err
	}
	if dir := filepath.Dir(p); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(p), ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, p)
}

// Update runs a read-modify-write cycle under the package mutex so concurrent
// callers never lose each other's changes. It Loads the current config, calls
// mutate on a pointer to it, and Saves the result — all while holding mu, so two
// in-process goroutines each adding a distinct tool both survive (the second no
// longer reads a stale base and clobbers the first). If mutate returns an error
// the config is NOT saved and that error is returned. A Load or Save error is
// returned as-is (mutate is not called on a Load failure).
//
// This is the SAFE path for every mutating caller (setup / wire / unwire); a bare
// Load→modify→Save outside Update is racy and should be migrated here. Cross-
// process safety is out of scope (see mu).
func Update(mutate func(*Config) error) error {
	mu.Lock()
	defer mu.Unlock()

	c, err := Load()
	if err != nil {
		return err
	}
	if err := mutate(&c); err != nil {
		return err
	}
	return Save(c)
}
