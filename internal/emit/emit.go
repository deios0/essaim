// Package emit implements the M3 NativeFileEmitter (spec §5): it writes the SAME
// ranked, LIVE-only oikos block the proxy injects into the user's own native
// instruction files (CLAUDE.md / AGENTS.md / GEMINI.md), fenced with the same
// sentinels. It reaches tools the proxy can't front (and default-mode Claude
// Code, covering the deferred /v1/messages) without a base_url repoint. It is
// LIVE-only (no drafts), opt-in per tool, one-channel-per-tool (never
// double-injects with the proxy path for the same tool), refuses any path
// containing a tracked credential, and is backup + restorable + debounced +
// idempotent.
package emit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"oikos/internal/extract"
	"oikos/internal/rules"
)

// DefaultDebounce coalesces a burst of index swaps into one write (B-8).
const DefaultDebounce = 2 * time.Second

// Tool is a wired native-file target (opt-in, B-6). NativeFile is the path of
// the tool's instruction file (e.g. ./CLAUDE.md).
type Tool struct {
	Name       string
	NativeFile string
}

// Emitter writes the ranked live block into wired tools' native files. It
// registers onIndexSwap via Store.SetOnSwap (the seam the Store already
// exposes). All work runs off the request path (the onSwap callback is off the
// hot path).
type Emitter struct {
	cfg   rules.GuardConfig
	tools []Tool

	mu       sync.Mutex
	timer    *time.Timer
	debounce time.Duration
	// last is the per-tool last-written block, for the idempotent skip.
	last map[string]string

	// onEmitDone, when set, is invoked once after every emit() completes (both the
	// debounced timer path and the synchronous path). It is a TEST seam: it lets a
	// test wait for the debounced write deterministically instead of sleeping past
	// the debounce window (which flakes under -race CPU starvation). nil in prod.
	onEmitDone func()

	// liveTools, if set, reports the CURRENT set of wired-tool names (what oikos's
	// config.json holds right now) so emit() can skip a target whose tool was
	// removed by `oikos unwire` on a running daemon — making unwire take effect
	// in-process, with NO restart (RESPECTS-UNWIRE / P1). The tools slice is
	// snapshotted once at boot; this predicate is the live overlay over it. It
	// mirrors heal.Watcher.SetLiveTools exactly.
	//
	// Contract: it returns (set, true) when it could read the live set, or
	// (_, false) when it couldn't (e.g. a transient unreadable config). On false
	// the emitter FAILS TOWARD EMITTING — it keeps guarding rather than silently
	// dropping a wired tool's block on a transient error (same policy as heal).
	// With no predicate set, every wired tool emits (prior behavior). Guarded by mu.
	liveTools func() (set map[string]bool, ok bool)

	// onEmitProblem, if set, is called for each per-target outcome that is NOT a
	// clean success — a StatusFailed write or a StatusRefused path — from the
	// DAEMON paths (OnIndexSwap's immediate + debounced-timer branches), whose
	// ([]TargetResult, error) were previously discarded so a wired tool's file
	// could go permanently stale with zero signal (P1). `oikos serve` wires this to
	// stderr. nil ⇒ problems are not surfaced (prior behavior; the explicit
	// EmitNow* callers still receive the full result slice as a return value).
	// Guarded by mu.
	onEmitProblem func(TargetResult)
	// writes counts completed write-to-disk operations across all tools, so a test
	// can assert "an identical re-emit did NOT touch the file" deterministically
	// (no reliance on coarse, FS-dependent mtime resolution). Guarded by mu.
	writes int
}

// New constructs an Emitter over the wired tools with the given guard config.
func New(cfg rules.GuardConfig, tools []Tool) *Emitter {
	return &Emitter{cfg: cfg, tools: tools, debounce: DefaultDebounce, last: map[string]string{}}
}

// SetDebounce overrides the debounce window (test seam; 0 ⇒ synchronous).
func (e *Emitter) SetDebounce(d time.Duration) { e.debounce = d }

// SetOnEmitDone registers a callback fired once after every emit() completes
// (TEST seam). It lets a test wait for the debounced write via a signal channel
// instead of sleeping past the debounce window — deterministic under -race load,
// where a wall-clock sleep can fire before a CPU-starved timer. nil ⇒ no-op.
func (e *Emitter) SetOnEmitDone(fn func()) {
	e.mu.Lock()
	e.onEmitDone = fn
	e.mu.Unlock()
}

// SetLiveTools registers the predicate emit() consults to learn the CURRENT set
// of wired-tool names, so a tool removed by `oikos unwire` on a running daemon is
// no longer emitted to — no restart needed (RESPECTS-UNWIRE / P1). It mirrors
// heal.Watcher.SetLiveTools: `oikos serve` wires wire.LiveWiredTools() into both.
// Without it every wired tool emits unconditionally (prior behavior). See the
// liveTools field doc for the fail-toward-emitting contract on an undeterminable
// live set. Normally set before the first emit; stored under mu so a concurrent
// set is safe.
func (e *Emitter) SetLiveTools(f func() (map[string]bool, bool)) {
	e.mu.Lock()
	e.liveTools = f
	e.mu.Unlock()
}

// SetOnEmitProblem registers a sink for per-target outcomes that are NOT a clean
// success (StatusFailed or StatusRefused) from the DAEMON emit paths (OnIndexSwap
// — both the immediate and the debounced-timer branch), whose results were
// previously discarded (P1). Without it a wired tool's native file could go
// permanently stale with zero signal. `oikos serve` wires this to stderr.
// Mirrors heal.Watcher.SetOnError. Stored under mu so a concurrent set is safe.
func (e *Emitter) SetOnEmitProblem(f func(TargetResult)) {
	e.mu.Lock()
	e.onEmitProblem = f
	e.mu.Unlock()
}

// toolIsLive reports whether the emitter should still write to tool given the
// live wired-tools predicate. With no predicate set, every tool is live (prior
// behavior). When the predicate cannot determine the set (ok=false), we FAIL
// TOWARD EMITTING — a transient unreadable config must not silently drop a wired
// tool's block. Mirrors heal.Watcher.targetIsLive.
func (e *Emitter) toolIsLive(tool Tool) bool {
	e.mu.Lock()
	f := e.liveTools
	e.mu.Unlock()
	if f == nil {
		return true // no live overlay configured — emit as before
	}
	set, ok := f()
	if !ok {
		return true // undeterminable — fail toward emitting (keep guarding)
	}
	// Check by NATIVE-FILE path, not tool name: native-file records are keyed per
	// (name, native_file), so two projects can wire the same tool name. Keying the
	// liveness check on the file this target actually writes means unwiring one
	// project stops ITS emit without silencing the other project's same-named tool.
	if tool.NativeFile != "" {
		return set[tool.NativeFile]
	}
	return set[tool.Name]
}

// Writes reports the number of completed disk writes across all tools (TEST
// accessor). A debounced burst that coalesces to one write increments this by
// one; an identical re-emit that hits the idempotent skip does not increment it.
// Lets a test assert idempotency deterministically without comparing mtimes.
func (e *Emitter) Writes() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.writes
}

// WiredTools returns the names of tools this emitter owns as native-file
// channels — the arbiter consults this to keep the proxy out (one channel per
// tool, §5.3).
func (e *Emitter) WiredTools() map[string]bool {
	out := make(map[string]bool, len(e.tools))
	for _, t := range e.tools {
		out[t.Name] = true
	}
	return out
}

// OnIndexSwap is the Store.SetOnSwap callback (registered off the request path).
// It debounces, builds the LIVE-only eager block, and writes it to each wired
// tool's native file (idempotent + backup + atomic). With debounce 0 it runs
// synchronously (test convenience).
func (e *Emitter) OnIndexSwap(ix *rules.Index) {
	if e.debounce <= 0 {
		// Immediate path: capture the results/err instead of discarding them, so a
		// failed or refused target is surfaced (P1) rather than going stale silently.
		_, results, err := e.emit(ix)
		e.reportProblems(results, err)
		e.signalEmitDone() // AFTER reportProblems: a test waiting on the signal is
		//                    guaranteed to observe the problem sink already filled.
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	// Capture the latest index in a closure; the timer fires once for the burst.
	target := ix
	if e.timer != nil {
		e.timer.Stop()
	}
	e.timer = time.AfterFunc(e.debounce, func() {
		// Debounced-timer path: same — do not discard the outcome (P1/P2).
		_, results, err := e.emit(target)
		e.reportProblems(results, err)
		e.signalEmitDone() // AFTER reportProblems (see the immediate path): the
		//                    onEmitDone seam must fire only once the problem sink
		//                    is filled, or a test racing on the signal reads it empty.
	})
}

// reportProblems surfaces per-target emit failures from the daemon paths (P1).
// It routes each non-clean outcome (a StatusFailed write or a StatusRefused
// path) to the registered onEmitProblem sink so a wired tool's native file can
// never go permanently stale with zero signal. A whole-emit err (e.g. an index
// render failure) is reported as a synthetic StatusFailed with no target name.
// A nil sink is a no-op (the prior behavior for callers that don't opt in).
func (e *Emitter) reportProblems(results []TargetResult, err error) {
	e.mu.Lock()
	fn := e.onEmitProblem
	e.mu.Unlock()
	if fn == nil {
		return
	}
	if err != nil {
		fn(TargetResult{Status: StatusFailed, Err: err})
	}
	for _, r := range results {
		if r.Status == StatusFailed || r.Status == StatusRefused {
			fn(r)
		}
	}
}

// TargetStatus is the per-target outcome of an emit (review follow-up #4 — so a
// caller can report skips/failures instead of blindly claiming "wrote N").
type TargetStatus int

const (
	// StatusWritten: the block was written to disk for this target.
	StatusWritten TargetStatus = iota
	// StatusSkipped: the target was unchanged (idempotent skip) — no disk write.
	StatusSkipped
	// StatusRefused: the target PATH itself contained a tracked credential, so the
	// emitter refused to touch it (B-7 path refusal).
	StatusRefused
	// StatusFailed: a disk write was attempted and errored (Err is set).
	StatusFailed
)

// TargetResult is the outcome for a single wired tool.
type TargetResult struct {
	Name       string
	NativeFile string
	Status     TargetStatus
	Err        error // set only when Status == StatusFailed
}

// EmitNow renders + writes synchronously (used by the demo + tests). Returns the
// block it wrote (or the empty-fenced block when there is no live rule).
func (e *Emitter) EmitNow(ix *rules.Index) (string, error) {
	block, _, err := e.emit(ix)
	e.signalEmitDone()
	return block, err
}

// EmitNowWithResults is EmitNow plus the per-target outcomes, so a caller (the
// `oikos emit` CLI) can report which targets were written, skipped, refused, or
// failed instead of asserting "wrote N rules" for every target unconditionally.
func (e *Emitter) EmitNowWithResults(ix *rules.Index) (string, []TargetResult, error) {
	block, results, err := e.emit(ix)
	e.signalEmitDone()
	return block, results, err
}

// emit renders the live-only eager block and writes it to every wired tool. It
// returns a per-target result slice (one entry per wired tool, in order)
// alongside the rendered block. It does NOT fire onEmitDone itself — each caller
// signals AFTER it has finished its own post-processing (the daemon paths signal
// after reportProblems), so a test waiting on the seam is guaranteed to observe
// that processing complete, not race it.
func (e *Emitter) emit(ix *rules.Index) (string, []TargetResult, error) {
	res, err := ix.EmitEager(e.cfg)
	if errors.Is(err, rules.ErrNoMatch) || errors.Is(err, rules.ErrIndexEmpty) {
		res = rules.GuardResult{} // empty live set → empty (but well-fenced) region
	} else if err != nil {
		return "", nil, err
	}
	body := dropCredentialLines(rules.RenderBody(res.Kept)) // B-7 belt
	block := rules.WrapBlock(body)

	results := make([]TargetResult, 0, len(e.tools))
	for _, tool := range e.tools {
		tr := TargetResult{Name: tool.Name, NativeFile: tool.NativeFile}
		if !e.toolIsLive(tool) { // RESPECTS-UNWIRE (P1): tool unwired on a running daemon
			tr.Status = StatusSkipped
			results = append(results, tr)
			continue
		}
		if refusesCredentialPath(tool.NativeFile) { // B-7 path refusal
			tr.Status = StatusRefused
			results = append(results, tr)
			continue
		}
		// B-8 idempotent skip. The AUTHORITATIVE test is the on-disk fenced region:
		// if it already equals the block, no write is needed regardless of what this
		// emitter last wrote. The in-memory `last` cache is only a hint (it lets the
		// long-lived daemon skip when it KNOWS it just wrote this block); it must NOT
		// gate the skip, or the standalone CLI path — which builds a FRESH emitter each
		// `oikos emit`, so `last` is always empty — would rewrite an unchanged file
		// every run and dirty its mtime (waking watchers/IDEs). Consult the disk (P2-1).
		if fencedRegionEquals(tool.NativeFile, block) {
			e.mu.Lock()
			e.last[tool.Name] = block // keep the cache consistent with disk
			e.mu.Unlock()
			tr.Status = StatusSkipped
			results = append(results, tr)
			continue
		}
		if werr := writeRenameFencedWithBackup(tool.NativeFile, block); werr != nil {
			tr.Status = StatusFailed // best-effort per tool; one bad path never blocks the others
			tr.Err = werr
			results = append(results, tr)
			continue
		}
		e.mu.Lock()
		e.last[tool.Name] = block
		e.writes++ // a real disk write happened (idempotent skips never reach here)
		e.mu.Unlock()
		tr.Status = StatusWritten
		results = append(results, tr)
	}
	return block, results, nil
}

// signalEmitDone fires the onEmitDone test seam (if registered) after an emit
// completes. Read under the lock so it composes with SetOnEmitDone; invoked
// without the lock held so the callback can re-enter the Emitter if it wants.
func (e *Emitter) signalEmitDone() {
	e.mu.Lock()
	fn := e.onEmitDone
	e.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// dropCredentialLines scrubs any credential from the rendered eager block (B-7
// belt-and-suspenders — a local store never emits a key into a file). It is
// BLOCK-aware (P1-b): a multi-line PEM/PGP private key spans several lines whose
// body lines look benign per-line, so a naive line-by-line scan would leave the
// base64 body + END footer behind. We first run RedactCredentials over the WHOLE
// body (the whole-block pattern removes header+body+footer in one shot, replacing
// the span with [REDACTED]); then we drop any line that STILL matches a
// credential (a single-line key, or a residual that a partial match left). This
// removes whole key blocks regardless of whether the renderer collapsed the body
// to one line.
func dropCredentialLines(body string) string {
	if body == "" {
		return body
	}
	// Whole-block redaction first: collapses a multi-line PEM/PGP key (and any
	// other credential span) to [REDACTED] before the per-line pass.
	body = extract.RedactCredentials(body)
	var keep []string
	for _, ln := range strings.Split(body, "\n") {
		if extract.ContainsCredential(ln) || extract.ContainsPrivateKeyMarker(ln) {
			continue
		}
		keep = append(keep, ln)
	}
	return strings.Join(keep, "\n")
}

// refusesCredentialPath reports whether a target path contains a tracked
// credential pattern (B-7 path refusal — design-closure §3 "refuses any path
// containing a tracked credential").
func refusesCredentialPath(path string) bool {
	return extract.ContainsCredential(path)
}

// fencedRegionEquals reports whether the file's current fenced region byte-
// content equals block (the idempotent skip predicate).
func fencedRegionEquals(path, block string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	cur, ok := extractFencedRegion(string(raw))
	return ok && cur == block
}

// extractFencedRegion returns the existing oikos-managed OIKOS_BEGIN…OIKOS_END
// region of s (inclusive of both sentinels) and whether one was found. Only a
// line-anchored, solo-line pair is recognized (rules.ManagedRegion) so a
// user-inline sentinel is never mistaken for the managed block (P0-2).
func extractFencedRegion(s string) (string, bool) {
	start, end, ok := rules.ManagedRegion(s)
	if !ok {
		return "", false
	}
	return s[start:end], true
}

// defaultNativeFileMode is the mode a brand-new native instruction file is
// created with (a normal, group/other-readable repo file). An EXISTING file keeps
// its own mode (review follow-up #2) — only a fresh file uses this default.
const defaultNativeFileMode os.FileMode = 0o644

// writeRenameFencedWithBackup replaces ONLY the fenced OIKOS region of the file
// (never user content), atomically (write-temp-then-rename), after backing the
// file up (restorable). If the file has no fenced region, the block is appended
// (preceded by a blank line). A missing file is created with just the block. The
// EXISTING file's mode is preserved across the rename (a 0644 AGENTS.md stays
// 0644, not the 0600 os.CreateTemp default); a new file gets defaultNativeFileMode.
//
// SYMLINK WRITE-THROUGH (P2-5): the standard CLAUDE.md -> AGENTS.md single-source
// setup makes the native file a SYMLINK. Two paths must be kept DISTINCT:
//   - the LOGICAL path (`path`): where the .oikos.bak lives and where wire/unwire
//     snapshot+restore. It is ALWAYS the caller's path and is never resolved for
//     the backup (a real-path backup would be orphaned — unwire looks at `path`).
//   - the WRITE TARGET (`writeTarget`): the file whose BYTES we replace. For a
//     symlink we resolve it (EvalSymlinks, or Readlink for a dangling link) so the
//     atomic rename replaces the REAL target and PRESERVES the link, instead of
//     clobbering the symlink with a detached regular file (which silently diverges
//     the two names). For a regular file the two paths are identical.
func writeRenameFencedWithBackup(path, block string) error {
	writeTarget, mode, err := resolveWriteTarget(path)
	if err != nil {
		return err
	}

	raw, err := os.ReadFile(path) // follows a live symlink → reads the real target
	if errors.Is(err, os.ErrNotExist) {
		// Nothing to read: either `path` does not exist, or it is a DANGLING symlink
		// whose target does not exist yet. Create just the block at the WRITE TARGET —
		// which resolveWriteTarget already pointed at the readlink target for a
		// dangling symlink, or at `path` itself otherwise — so the link is preserved
		// rather than replaced by a regular file. No original exists → nothing to back up.
		return atomicWrite(writeTarget, []byte(block+"\n"), defaultNativeFileMode)
	}
	if err != nil {
		return err
	}
	orig := string(raw)

	// Back up the PRISTINE original ONCE — only when no backup exists yet. A later
	// emit reads a file that ALREADY carries an oikos block; backing that up would
	// overwrite the user's clean original, so a restore would re-inject a stale
	// block instead of cleaning. The first backup is the pristine snapshot.
	//
	// The backup ALWAYS lives at the LOGICAL path (`path`.oikos.bak), NOT the
	// resolved real path — that is exactly where wire/unwire read it. `raw` already
	// holds the target's content (ReadFile followed the link), so the backup is the
	// correct pristine bytes regardless of the symlink.
	bak := path + ".oikos.bak"
	if _, statErr := os.Stat(bak); errors.Is(statErr, os.ErrNotExist) {
		// The backup is a snapshot of the original → keep the original's mode.
		if err := atomicWrite(bak, raw, mode); err != nil {
			return err
		}
	}

	var out string
	if _, ok := extractFencedRegion(orig); ok {
		out = replaceFencedRegion(orig, block)
	} else {
		// Append the block, preserving user content above it.
		sep := "\n"
		if !strings.HasSuffix(orig, "\n") {
			sep = "\n\n"
		} else if !strings.HasSuffix(orig, "\n\n") {
			sep = "\n"
		}
		out = orig + sep + block + "\n"
	}
	// Write to the RESOLVED target so a symlink is preserved (write-through).
	return atomicWrite(writeTarget, []byte(out), mode)
}

// resolveWriteTarget maps the logical native-file path to the actual file whose
// BYTES an emit should replace, so a symlinked native file is written THROUGH to
// its real target (preserving the link) instead of being clobbered by the atomic
// rename. It returns:
//   - writeTarget: the path to write content to (the resolved real path for a
//     symlink; the readlink target for a dangling symlink; `path` itself otherwise).
//   - mode: the permission bits to preserve (the real target's mode when it exists,
//     else defaultNativeFileMode).
//
// The backup path is intentionally NOT derived from this — it always stays at the
// logical `path` so wire/unwire find it (P2-5 backup-desync guard).
func resolveWriteTarget(path string) (writeTarget string, mode os.FileMode, err error) {
	mode = defaultNativeFileMode
	li, lerr := os.Lstat(path)
	if lerr != nil {
		// Path does not exist (or is unreadable): write target IS the path; a fresh
		// file will be created with the default mode. A non-not-exist error surfaces.
		if errors.Is(lerr, os.ErrNotExist) {
			return path, mode, nil
		}
		return "", mode, lerr
	}
	if li.Mode()&os.ModeSymlink == 0 {
		// Regular file (or dir/other): write in place, preserve its mode.
		if fi, statErr := os.Stat(path); statErr == nil {
			mode = fi.Mode().Perm()
		}
		return path, mode, nil
	}
	// It is a symlink. Prefer EvalSymlinks (fully resolves chains + returns the real
	// path); fall back to a single Readlink for a DANGLING link whose target is
	// missing (EvalSymlinks errors on a broken link).
	if real, evErr := filepath.EvalSymlinks(path); evErr == nil {
		if fi, statErr := os.Stat(real); statErr == nil {
			mode = fi.Mode().Perm()
		}
		return real, mode, nil
	}
	// DANGLING symlink (points at a missing target): write the LOGICAL path as a
	// normal file, replacing the broken link. Writing THROUGH to the readlink target
	// instead would create a file with only the oikos block and no backup, and a
	// later `unwire` (seeing no backup + a block-only file) would remove the LOGICAL
	// path — deleting the symlink and orphaning the created target (codex review).
	// The link was already broken, so replacing it loses nothing and keeps
	// backup+unwire consistent at one path.
	return path, mode, nil
}

// replaceFencedRegion replaces the oikos-managed OIKOS_BEGIN…OIKOS_END region of
// s with block, leaving every other byte untouched. Only a line-anchored,
// solo-line pair is replaced (rules.ManagedRegion), so a user's inline sentinel
// is never spliced into the replacement.
func replaceFencedRegion(s, block string) string {
	start, end, ok := rules.ManagedRegion(s)
	if !ok {
		return s
	}
	return s[:start] + block + s[end:]
}

// atomicWrite writes data to path via write-temp-then-rename, with the temp file
// chmod'd to mode BEFORE the rename so the destination ends up at the caller's
// requested mode (os.CreateTemp defaults to 0600; the caller passes the existing
// file's mode to preserve it, or defaultNativeFileMode for a fresh file). The
// chmod is applied to the temp (not the destination) so the file is never visible
// at the wrong mode at any point.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".oikos-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	// Flush data before the rename so a crash can't leave a renamed zero-length file.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	// Best-effort directory fsync so the rename itself survives a crash.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
