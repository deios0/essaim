// Package wire implements `essaim wire <tool>`: it points a tool at the local
// essaim proxy (http://127.0.0.1:4141) through the CORRECT channel for that tool,
// and persists the wiring so it survives a restart. It is idempotent — wiring
// the same tool twice changes nothing.
//
// Two channels (spec §1 step 6, §5.3 — one channel per tool, the proxy never
// double-injects with the native-file path):
//
//   - base_url  — the tool routes its OpenAI traffic through the proxy by
//     setting OPENAI_BASE_URL (or the tool's own setting). Used for Cursor,
//     Continue, and any generic OpenAI-compatible tool.
//   - native_file — the tool learns via a managed block in its instruction file
//     (CLAUDE.md / AGENTS.md). Used for Claude Code, which in v1 must NEVER be
//     pointed via ANTHROPIC_BASE_URL (essaim serves no /v1/messages — a base_url
//     repoint would brick it).
package wire

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"essaim/internal/config"
	"essaim/internal/rules"
)

// ProxyBaseURL is the loopback proxy every wired tool points at.
const ProxyBaseURL = "http://127.0.0.1:4141"

// Channel identifies how a tool is wired.
const (
	ChannelBaseURL    = "base_url"
	ChannelNativeFile = "native_file"
)

// Plan is the resolved wiring for one tool: which channel, and the concrete
// target (a base_url or a native file path). It is computed without touching the
// filesystem; Apply performs the side effects.
type Plan struct {
	Tool       string
	Channel    string // ChannelBaseURL | ChannelNativeFile
	BaseURL    string // set when Channel == ChannelBaseURL
	NativeFile string // absolute path; set when Channel == ChannelNativeFile
	// Note is an honest, human caveat printed to the user (e.g. Cursor only
	// routes its chat panel through a custom endpoint). May be empty.
	Note string
}

// Plan resolves the wiring for tool. dir is the working directory used to anchor
// a native-file tool's instruction file (default: cwd). Known tools get their
// correct channel; an unknown tool falls back to the generic base_url
// env-export form (which works for any OpenAI-compatible client).
func Resolve(tool, dir string) (Plan, error) {
	name := strings.ToLower(strings.TrimSpace(tool))
	if name == "" {
		return Plan{}, errors.New("essaim wire: a tool name is required (e.g. `essaim wire cursor`)")
	}

	switch name {
	case "claude-code", "claude", "claudecode":
		nf, err := nativeFilePath(dir, "CLAUDE.md")
		if err != nil {
			return Plan{}, err
		}
		return Plan{
			Tool:       "claude-code",
			Channel:    ChannelNativeFile,
			NativeFile: nf,
			Note:       "Claude Code learns via a managed block in CLAUDE.md (essaim v1 serves no /v1/messages — it is NEVER pointed via ANTHROPIC_BASE_URL).",
		}, nil

	case "codex", "agents":
		nf, err := nativeFilePath(dir, "AGENTS.md")
		if err != nil {
			return Plan{}, err
		}
		return Plan{
			Tool:       "codex",
			Channel:    ChannelNativeFile,
			NativeFile: nf,
			Note:       "Codex learns via a managed block in AGENTS.md.",
		}, nil

	case "cursor":
		return Plan{
			Tool:    "cursor",
			Channel: ChannelBaseURL,
			BaseURL: ProxyBaseURL,
			Note:    "Cursor routes only its chat panel through a custom OpenAI endpoint (Tab/autocomplete stays on Cursor's own models).",
		}, nil

	case "continue":
		return Plan{
			Tool:    "continue",
			Channel: ChannelBaseURL,
			BaseURL: ProxyBaseURL,
			Note:    "Set this base URL on Continue's OpenAI-compatible provider (config.json `apiBase`).",
		}, nil

	default:
		// Generic fallback: any OpenAI-compatible tool can point its base_url at
		// the proxy. This is the env-export form.
		return Plan{
			Tool:    name,
			Channel: ChannelBaseURL,
			BaseURL: ProxyBaseURL,
			Note:    "Generic OpenAI-compatible wiring — set this tool's API base URL (or OPENAI_BASE_URL) to the value below.",
		}, nil
	}
}

// nativeFilePath resolves an absolute path for a native instruction file under
// dir (default: cwd). filepath.Abs/Join are OS-native, so a Windows host yields
// a backslash-joined drive-letter path under %APPDATA%/%LOCALAPPDATA% or the
// project dir without any platform-divergent code (spec N11).
func nativeFilePath(dir, base string) (string, error) {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = wd
	}
	abs, err := filepath.Abs(filepath.Join(dir, base))
	if err != nil {
		return "", err
	}
	return abs, nil
}

// joinNativePath is the pure, platform-injectable join used to anchor a native
// file under an already-absolute dir. windows selects the backslash separator so
// a Linux test host can prove the Windows %APPDATA%/%LOCALAPPDATA% layout; the
// real path always flows through nativeFilePath (filepath.Join is host-native).
func joinNativePath(dir, base string, windows bool) string {
	sep := "/"
	if windows {
		sep = `\`
	}
	dir = strings.TrimRight(dir, sep)
	return dir + sep + base
}

// EnvExport returns the copy-pasteable shell line(s) that point a base_url tool
// at the proxy. Empty for a native-file plan (which has no env form).
func (p Plan) EnvExport() string {
	if p.Channel != ChannelBaseURL {
		return ""
	}
	// OpenAI clients expect the /v1 suffix on a base_url.
	return "export OPENAI_BASE_URL=" + p.BaseURL + "/v1"
}

// Apply performs the wiring side effects and persists the result to the config
// store. It is idempotent. It returns the loaded-and-updated config.
//
//   - base_url:    nothing is written to any tool config file (essaim can't know
//     every tool's config schema); the persisted record + the printed env-export
//     are the wiring. The user (or the /setup UI) applies the env line.
//   - native_file: a managed (empty) essaim block is seeded into the instruction
//     file so the emitter has an anchor, preserving all user content. Re-applying
//     never adds a second block.
func Apply(p Plan) (config.Config, error) {
	wt := config.WiredTool{Name: p.Tool, Channel: p.Channel}
	if p.Channel == ChannelBaseURL {
		// Deferred-6: capture the user's ORIGINAL upstream from the tool's IDE config
		// BEFORE essaim ever heals it, so `essaim unwire` restores THAT exact value (their
		// real provider — a non-OpenAI vendor, their own gateway) instead of a hardcoded
		// OpenAI default. Best-effort: "" when there is no config yet / no base_url set /
		// it already points at essaim — unwire then falls back to the vendor default.
		wt.OriginalBaseURL = captureOriginalBaseURL(baseURLConfigPath(p.Tool))
	}
	if p.Channel == ChannelNativeFile {
		wt.NativeFile = p.NativeFile
		// P0-2 (a): back up the PRISTINE target before modifying it, exactly like the
		// emitter (writeRenameFencedWithBackup) — so `essaim unwire` can restore it
		// byte-exact. The backup is taken ONCE: if one already exists (a prior wire /
		// a live emit), the file already carries an essaim block and re-backing it up
		// would overwrite the clean original. A file essaim is about to CREATE (no
		// pre-existing original) has nothing to back up — its "original" is "no file".
		//
		// These file side-effects run BEFORE the config read-modify-write so the
		// mutex-held Update below only wraps the pure Load→UpsertTool→Save cycle.
		if err := backupTargetOnce(p.NativeFile); err != nil {
			return config.Config{}, err
		}
		if err := seedManagedBlock(p.NativeFile); err != nil {
			return config.Config{}, err
		}
	}

	// Serialize the config read-modify-write (P2): two concurrent wires must not
	// lose each other's tool record. Update holds the package mutex across
	// Load→mutate→Save.
	var out config.Config
	if err := config.Update(func(c *config.Config) error {
		// Deferred-6: on a RE-wire, essaim may have already healed the IDE config, so
		// captureOriginalBaseURL above returns "" (only the proxy URL is present). Do
		// NOT overwrite a previously-captured original with "" — carry it forward so
		// the day-0 upstream is never lost across idempotent re-wires.
		if wt.Channel == ChannelBaseURL && wt.OriginalBaseURL == "" {
			if prev, ok := findWiredTool(*c, wt.Name, wt.NativeFile); ok && prev.OriginalBaseURL != "" {
				wt.OriginalBaseURL = prev.OriginalBaseURL
			}
		}
		*c = c.UpsertTool(wt)
		out = *c
		return nil
	}); err != nil {
		return config.Config{}, err
	}
	return out, nil
}

// seedManagedBlock ensures path contains exactly one empty essaim managed block
// (the BEGIN…END fence), preserving any existing user content. If the file
// already has a block (from a prior wire or a live emit) it is left untouched —
// the emitter owns the block's contents. A missing file/parent is created.
func seedManagedBlock(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	existing := string(raw)
	if strings.Contains(existing, rules.ESSAIM_BEGIN) {
		return nil // already anchored — idempotent
	}

	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	// An empty managed block: the emitter will replace its body on the first
	// index swap. WrapBlock applies the same sentinel discipline the proxy uses.
	block := rules.WrapBlock("")
	var out string
	switch {
	case existing == "":
		out = block + "\n"
	case strings.HasSuffix(existing, "\n\n"):
		out = existing + block + "\n"
	case strings.HasSuffix(existing, "\n"):
		out = existing + "\n" + block + "\n"
	default:
		out = existing + "\n\n" + block + "\n"
	}
	return os.WriteFile(path, []byte(out), 0o644)
}

// backupSuffix is appended to a native-file target to hold the pristine
// pre-wire snapshot, matching the emitter's backup convention (one delimiter, one
// reader). `essaim unwire` restores from it and removes it.
const backupSuffix = ".essaim.bak"

// backupTargetOnce snapshots the PRISTINE target file to <path>.essaim.bak before
// the first essaim modification (P0-2 (a)). It is idempotent: a backup is taken
// only when none exists yet — a later wire reads a file that ALREADY carries an
// essaim block, and backing THAT up would lose the user's clean original. A missing
// target (a file essaim is about to create) has no original to back up, so no
// backup is written; unwire then treats "no backup" as "the file did not exist /
// essaim created it" and strips the block (or removes a block-only file).
func backupTargetOnce(path string) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // nothing to back up: essaim will create this file
	}
	if err != nil {
		return err
	}
	bak := path + backupSuffix
	if _, statErr := os.Stat(bak); statErr == nil {
		return nil // already have the pristine snapshot — never overwrite it
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	return os.WriteFile(bak, raw, 0o644)
}

// UnwireOutcome describes what `essaim unwire` did, so the CLI can print an HONEST
// message (P1-BUG-2) instead of a blanket "original config restored". Channel is
// the wiring channel that was undone (or resolved). BaseURL carries the base_url
// tool's config-restore status: whether essaim removed its own proxy URL, whether
// the config could not be auto-restored (a manual-recovery hint is warranted), and
// the config path essaim acted on.
type UnwireOutcome struct {
	Tool    string
	Channel string
	Found   bool
	BaseURL RestoreStatus
}

// Unwire is the clean undo of `essaim wire <tool>` (P0-2 (b)). It is idempotent —
// unwiring a tool that is not wired is a clean no-op. It is a thin wrapper over
// UnwireResult that discards the outcome detail (kept for callers that only need
// success/failure). See UnwireResult for the full behavior.
func Unwire(tool, dir string) error {
	_, err := UnwireResult(tool, dir)
	return err
}

// UnwireResult is Unwire with a reported outcome. For a native-file tool it
// restores the target byte-exact from the .essaim.bak backup (then removes the
// backup); if there is no backup (essaim created the file), it removes the managed
// block, deleting the file when only the block remains (the original "no file"
// state). For a base_url tool it runs the INVERSE of the heal repair on the tool's
// IDE config (P1-BUG-2): the heal watcher wrote the essaim proxy URL there, so after
// the user deletes essaim that URL points at a dead loopback — unwire replaces it
// with the vendor default (or flags a manual-recovery hint if it can't). In all
// cases the matching wired-tool record is removed.
//
// dir anchors a native-file tool's instruction file the same way Resolve does, so
// `essaim unwire claude-code` undoes `essaim wire claude-code` in the same directory.
func UnwireResult(tool, dir string) (UnwireOutcome, error) {
	name := strings.ToLower(strings.TrimSpace(tool))
	if name == "" {
		return UnwireOutcome{}, errors.New("essaim unwire: a tool name is required (e.g. `essaim unwire claude-code`)")
	}

	c, err := config.Load()
	if err != nil {
		return UnwireOutcome{}, err
	}

	// Find the persisted wiring. Resolve gives the canonical tool name + channel +
	// native-file path; we match the persisted record by its WIRING IDENTITY —
	// (name + native-file path) for a native-file tool, name alone for a base_url
	// tool. Matching a native-file tool by name ALONE was the P1-BUG-1 defect:
	// `unwire --dir projA claude-code` would act on whatever record was stored last
	// (projB) instead of projA's. plan.NativeFile is anchored under --dir exactly as
	// Resolve anchors the wire, so the same --dir undoes the same wire.
	plan, perr := Resolve(name, dir)
	if perr != nil {
		return UnwireOutcome{}, perr
	}
	recorded, found := findWiredTool(c, plan.Tool, plan.NativeFile)
	outcome := UnwireOutcome{Tool: plan.Tool, Channel: plan.Channel, Found: found}

	// Restore/clean the native file using the RECORDED path when present (it is the
	// path actually wired), else the freshly-resolved path. base_url tools wrote no
	// file to a NATIVE target, but the heal watcher may have written the proxy URL
	// into the tool's IDE config — undone below.
	if found && recorded.Channel == ChannelNativeFile {
		nf := recorded.NativeFile
		if nf == "" {
			nf = plan.NativeFile
		}
		if err := restoreNativeFile(nf); err != nil {
			return outcome, err
		}
	} else if !found && plan.Channel == ChannelNativeFile {
		// Not recorded (e.g. config lost) but a native target may still carry a block
		// — best-effort clean so a re-wire/undo never leaves a stray block.
		if err := restoreNativeFile(plan.NativeFile); err != nil {
			return outcome, err
		}
	}

	// P1-BUG-2: a base_url tool DID leave state on disk once the daemon ran — the
	// heal watcher wrote the proxy URL into the tool's IDE config. Run the INVERSE
	// of heal on that config so the user isn't left pointing at a dead proxy after
	// deleting essaim. This runs whether or not a record was found (a lost config
	// must still be recoverable), and is best-effort: an un-restorable config yields
	// a hint via the returned status, not a hard failure.
	if plan.Channel == ChannelBaseURL {
		cfgPath := baseURLConfigPath(plan.Tool)
		// Deferred-6: restore the user's ORIGINAL upstream captured at wire time (their
		// real provider), not a hardcoded OpenAI default. A record with no captured
		// original (wired before this field existed, or capture failed) restores to the
		// vendor default with a hint — the previous behavior.
		st, rerr := restoreBaseURLConfigTo(cfgPath, recorded.OriginalBaseURL)
		if rerr != nil {
			return outcome, rerr
		}
		outcome.BaseURL = st
	}

	if !found {
		return outcome, nil // nothing recorded: clean no-op (idempotent)
	}

	// Serialize the record removal (P2): re-load and remove under the package mutex
	// via Update so a concurrent wire/unwire can't clobber this write with a stale
	// base. removeWiredTool is idempotent, so re-loading the (possibly newer) config
	// and dropping this exact wiring identity is correct even if the snapshot above
	// is now stale. We only reach here when the record WAS found, so we never create
	// an empty config where none existed.
	err = config.Update(func(cur *config.Config) error {
		*cur = removeWiredTool(*cur, plan.Tool, plan.NativeFile)
		return nil
	})
	return outcome, err
}

// restoreNativeFile undoes a native-file wiring on disk. When a .essaim.bak backup
// exists it is restored byte-exact (and removed). Otherwise the managed essaim
// block is stripped from the file, preserving all surrounding user content; if the
// file then holds nothing but whitespace (essaim had created it with only the
// block), it is removed so the original "no file" state is restored. A missing
// target is a no-op.
func restoreNativeFile(path string) error {
	if path == "" {
		return nil
	}
	bak := path + backupSuffix
	bakContent, bakErr := os.ReadFile(bak)
	hasBak := bakErr == nil
	if bakErr != nil && !errors.Is(bakErr, os.ErrNotExist) {
		return bakErr
	}

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		// No live file to restore. Drop any stale backup so a later re-wire starts
		// from a fresh snapshot; there is no user content to preserve.
		if hasBak {
			return os.Remove(bak)
		}
		return nil
	}
	if err != nil {
		return err
	}
	s := string(raw)
	stripped, hadOnlyBlock := stripManagedBlock(s)

	if hasBak {
		// P0-3: the backup is the DAY-0 pristine snapshot, never refreshed. Restore
		// it byte-exact ONLY when the user has not diverged the file since wiring —
		// i.e. the current content with essaim's managed block stripped equals the
		// backup (or the current file already IS the backup). Otherwise the user
		// edited the file after wiring and a byte-exact restore would silently
		// destroy those edits; preserve the current (block-stripped) content
		// instead. Either way the stale backup is dropped once unwiring is done.
		//
		// stripManagedBlock appends a single trailing '\n' when reconstructing a
		// backup that had no final newline, so `stripped == backup + "\n"` is that
		// exact, harmless artifact (NOT a user edit) and also counts as no
		// divergence — the byte-exact backup restore then reproduces the original.
		bakStr := string(bakContent)
		noDivergence := s == bakStr || stripped == bakStr || stripped == bakStr+"\n"
		if noDivergence {
			// Skip the write when the file already equals the backup — rewriting
			// identical bytes only dirties the mtime and can wake watchers/IDEs
			// (gemini review).
			if s != bakStr {
				if werr := os.WriteFile(path, bakContent, 0o644); werr != nil {
					return werr
				}
			}
			return os.Remove(bak)
		}
		// Diverged — preserve the user's current content, minus essaim's block.
		if hadOnlyBlock {
			if rerr := os.Remove(path); rerr != nil {
				return rerr
			}
		} else if werr := os.WriteFile(path, []byte(stripped), 0o644); werr != nil {
			return werr
		}
		return os.Remove(bak)
	}

	// No backup: only act if there is actually an essaim-managed block to remove. A
	// file with NO managed block (or only a user's inline sentinel) is none of
	// essaim's business — leave it byte-untouched so unwiring a never-wired (or
	// already-unwired) tool can never delete or rewrite an unrelated user file.
	if _, _, ok := rules.ManagedRegion(s); !ok {
		return nil // no managed block here → do not touch the file
	}
	if hadOnlyBlock {
		// The file held ONLY the essaim block (essaim created it) — restore the
		// original "no file" state.
		return os.Remove(path)
	}
	return os.WriteFile(path, []byte(stripped), 0o644)
}

// stripManagedBlock removes the single ESSAIM_BEGIN…ESSAIM_END region from s plus
// exactly the ONE separator the seeder/emitter inserts around it (a single
// trailing "\n" off the preceding content and a single leading "\n" off the
// following content) — never multiple, so user-authored blank lines around the
// block are preserved. onlyBlock reports whether the file was nothing but the
// block (modulo whitespace), i.e. essaim created the file. A string with no block
// is returned unchanged. The caller guarantees a block is present.
func stripManagedBlock(s string) (out string, onlyBlock bool) {
	// Use the shared line-anchored recognizer so a user's INLINE sentinel is never
	// mistaken for the managed block (same class as the emit-side P0-2 fix). The
	// old unanchored strings.Index here could splice from a user's inline BEGIN to
	// the real END and destroy user content on unwire.
	bi, end, ok := rules.ManagedRegion(s)
	if !ok {
		return s, false // no managed block (or unbalanced fence): leave untouched
	}
	before, after := s[:bi], s[end:]

	// essaim created the file iff everything outside the block is whitespace.
	onlyBlock = strings.TrimSpace(before) == "" && strings.TrimSpace(after) == ""
	if onlyBlock {
		return "", true
	}

	// Remove exactly ONE separator newline the seeder/emitter added: one trailing
	// '\n' before the block and one leading '\n' after it. We strip at most a
	// single newline on each side so a user's own blank line (which is itself a
	// trailing '\n' plus a blank line, i.e. "\n\n") is preserved.
	before = strings.TrimSuffix(before, "\n")
	after = strings.TrimPrefix(after, "\n")

	switch {
	case before == "":
		return after, false
	case after == "":
		// Keep a single terminating newline so the file ends cleanly.
		if strings.HasSuffix(before, "\n") {
			return before, false
		}
		return before + "\n", false
	default:
		// Re-join with a single separator newline (the one the seeder put between
		// the user's content and the block).
		return before + "\n" + after, false
	}
}

// nativeFileEqual compares two NativeFile paths for the wiring-identity match.
// Deferred-7: on Windows the filesystem is case-insensitive, so `essaim unwire
// --dir C:\Proj claude-code` must match a record wired as `c:\proj\CLAUDE.md` —
// an exact-string compare there would ORPHAN the record (leave it in config while
// the file is un-wired), and a re-wire would then duplicate it. We compare
// case-insensitively ONLY on Windows; on case-sensitive OSes an exact compare is
// correct (two paths differing only in case are genuinely different files).
func nativeFileEqual(a, b string) bool {
	if runtime.GOOS == "windows" {
		// Windows is case-insensitive AND accepts either slash direction, so
		// normalize both before comparing (filepath.Clean canonicalizes separators
		// on Windows) — otherwise C:\Proj\CLAUDE.md and C:/proj/claude.md orphan a
		// wired-tool record (gemini review).
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return a == b
}

// findWiredTool returns the persisted wiring for a tool's WIRING IDENTITY —
// (name + native-file path). A base_url tool has no native file (nativeFile ==
// ""), so it is matched by name alone. Keying by identity (not name alone) is what
// lets the SAME native-file tool coexist across projects and be un-wired per-dir
// (P1-BUG-1). The NativeFile comparison is case-insensitive on Windows (deferred-7).
func findWiredTool(c config.Config, name, nativeFile string) (config.WiredTool, bool) {
	for _, w := range c.WiredTools {
		if w.Name == name && nativeFileEqual(w.NativeFile, nativeFile) {
			return w, true
		}
	}
	return config.WiredTool{}, false
}

// removeWiredTool returns a copy of c with the tool matching (name + nativeFile)
// removed (idempotent). It removes ONLY the record for that exact wiring identity,
// so unwiring one project never drops another project's record (P1-BUG-1). The
// NativeFile comparison is case-insensitive on Windows (deferred-7) so a casing
// difference never orphans the record.
func removeWiredTool(c config.Config, name, nativeFile string) config.Config {
	out := c
	out.WiredTools = make([]config.WiredTool, 0, len(c.WiredTools))
	for _, w := range c.WiredTools {
		if w.Name == name && nativeFileEqual(w.NativeFile, nativeFile) {
			continue
		}
		out.WiredTools = append(out.WiredTools, w)
	}
	return out
}

// Summary returns a human, one-paragraph description of what a wire did — used
// by the CLI to print a clear next step.
func (p Plan) Summary() string {
	switch p.Channel {
	case ChannelNativeFile:
		return fmt.Sprintf("wired %s via its native file %s — essaim will keep a ranked rule block there.\n%s",
			p.Tool, p.NativeFile, p.Note)
	default:
		return fmt.Sprintf("wired %s to the essaim proxy. Point it at:\n  %s\n  %s\n%s",
			p.Tool, p.EnvExport(), p.BaseURL+"/v1", p.Note)
	}
}
