// Package heal is the config-drift self-heal watcher. It guards against the #1
// churn risk: an IDE update (Cursor / VSCode / Continue) rewriting a tool's
// config file and dropping the essaim proxy base_url — silently bypassing essaim
// so the user concludes "it broke."
//
// The watcher runs entirely OFF the request hot path, in its own goroutine. It
// is pure LOCAL file I/O — it never opens a socket, never phones home. When an
// IDE update DROPS or factory-resets the essaim base_url that essaim itself wrote,
// the watcher re-applies it within the debounce window (≤5s in production).
//
// It NEVER fights the user (the ship-blocker this design closes): the heal
// DECISION lives in each Target's Repair seam, which only re-applies essaim's own
// managed value when it was clobbered back to a vendor default or removed. A
// base_url a user DELIBERATELY set to something else (e.g. api.openai.com used
// directly) is a user override and is left untouched — Repair reports no change,
// so the watcher writes nothing.
//
// It is opt-in: `essaim serve` starts it only when tools are wired (there is
// something to keep pointed at the proxy). With no targets it is a no-op. A live
// kill-switch (DisableFlagPath, default <config-dir>/essaim/heal.disabled) turns
// healing off WITHOUT a restart: while the flag file exists, CheckOnce is a
// no-op.
//
// `essaim unwire <tool>` must also take effect WITHOUT a restart (RESPECTS-UNWIRE
// / P1): the targets slice is built once at boot, so a tool removed from the
// config while the daemon runs would otherwise keep being healed and fight the
// user's undo. SetLiveTools installs a predicate over the live wired-tools set
// (config.json); CheckOnce skips any target whose Tool is no longer present, so
// an unwired tool's file is never rewritten again — no restart.
package heal

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

// defaultInterval is the fsnotify coalescing/heal window. An IDE upgrade may
// rewrite several files in a burst; one debounced pass heals them all. Kept well
// under the ≤5s self-heal budget so a clobbered base_url is restored fast.
const defaultInterval = 750 * time.Millisecond

// Target is one managed config file the watcher keeps pointed at the essaim
// proxy.
//
// The heal DECISION is delegated entirely to Repair — the watcher itself makes
// no "looks unhealthy" judgement (the old `file lacks ExpectedURL ⇒ rewrite`
// pre-check was the ship-blocker: it stomped a base_url a user deliberately set
// to something else). Repair is the pure, in-process, LOCAL-ONLY healer:
//
//   - changed == false  ⇒ leave the file BYTE-untouched. This covers BOTH
//     "already healthy (holds essaim's managed value)" AND "deliberate user
//     override (holds some other value the user chose)". The watcher writes
//     nothing in either case.
//   - changed == true   ⇒ essaim's OWN previously-written value was clobbered
//     (dropped, or reset to a known vendor default by an IDE update); healed is
//     the surgically-repaired bytes to write back.
//   - err != nil        ⇒ the file could not be parsed/understood; the watcher
//     surfaces it (it does not silently no-op). Never corrupt a file we don't
//     understand.
type Target struct {
	// Tool is the wired-tool name this target belongs to (e.g. "continue"). It is
	// the key the live-tools filter (SetLiveTools) consults so the watcher can stop
	// healing a tool that `essaim unwire` removed from the config WITHOUT a restart.
	// Empty means "unnamed" — such a target is always treated as live (the filter
	// only gates targets that declare their tool).
	Tool string
	// Path is the absolute config file path to watch and heal.
	Path string
	// ExpectedURL is the essaim base_url essaim keeps the file pointed at. It is
	// informational/diagnostic now (the heal decision is Repair's); kept so a
	// Target is self-describing and tests can assert the managed url.
	ExpectedURL string
	// LastWritten is the EXACT base_url value essaim itself last wrote into the
	// managed key (reused from the wire record). It is the ground truth for
	// "ours": a managed key still holding LastWritten is healthy; a key holding
	// any OTHER value was changed by something other than essaim. Repair uses it to
	// avoid ever re-applying over a value essaim did not write. May equal
	// ExpectedURL on first wire; both are passed to the Repair builder.
	LastWritten string
	// Repair transforms the current file content into the healed content and
	// embodies the full heal-vs-leave-alone decision (see the type doc). It MUST
	// be local-only (no network).
	Repair func(current []byte) (healed []byte, changed bool, err error)
}

// Watcher re-applies the essaim base_url to a set of managed config files whenever
// an IDE rewrites them. It owns an fsnotify watcher over the parent directories
// of its targets (watching dirs, not files, so atomic save-via-rename — the
// common editor write pattern — is still observed).
type Watcher struct {
	targets  []Target
	interval time.Duration // debounce/heal window; a test seam (defaults to defaultInterval)

	// disableFlagPath is the live kill-switch: while a file exists at this path,
	// CheckOnce is a no-op (healing is OFF without a restart). Empty disables the
	// kill-switch entirely (always-on healing).
	disableFlagPath string

	// liveTools, if set, reports the CURRENT set of wired-tool names (what essaim's
	// config.json holds right now) so CheckOnce can skip a target whose tool was
	// removed by `essaim unwire` on a running daemon — making unwire take effect
	// in-process, with NO restart (RESPECTS-UNWIRE / P1). The targets slice is
	// built once at boot; this predicate is the live overlay over it.
	//
	// Contract: it returns (set, true) when it could read the live set, or
	// (_, false) when it couldn't (e.g. a transient unreadable config). On false
	// the watcher FAILS TOWARD HEALING — it keeps guarding rather than silently
	// dropping the guard on a transient error (same policy as the kill-switch's
	// stat-error fallback). A target whose Tool is "" is always live. The caller
	// (wire) backs this with an mtime cache so the common pass is cheap.
	liveTools func() (set map[string]bool, ok bool)

	// onError, if set, is called with any error a CheckOnce pass returns from the
	// background paths (startup + the debounced loop) — chiefly an unparseable
	// config surfaced by Repair (P1). It lets `essaim serve` make the failure
	// VISIBLE on stderr instead of the watcher silently swallowing it. The direct
	// CheckOnce return value is unchanged (callers that invoke it explicitly still
	// get the error); this is purely for the goroutine paths that have no caller.
	onError func(error)

	healed atomic.Int64 // total successful re-applies, for /health + tests

	// checkMu serializes CheckOnce so two debounced passes (or an explicit call
	// racing the loop) never interleave a read/transform/write on the same target.
	// It is a real invariant, not race-clean by luck.
	checkMu sync.Mutex

	mu      sync.Mutex
	watcher *fsnotify.Watcher
}

// New builds a Watcher for the given managed targets. It does not touch the
// filesystem or start any goroutine until Start is called.
func New(targets []Target) *Watcher {
	return &Watcher{targets: targets, interval: defaultInterval}
}

// HealCount reports how many times the watcher has re-applied a clobbered
// base_url over its lifetime (cumulative across all targets).
func (w *Watcher) HealCount() int64 { return w.healed.Load() }

// SetDisableFlagPath sets the live kill-switch file path. While a file exists at
// this path, CheckOnce is a no-op — healing is disabled WITHOUT a restart (the
// user can `touch` the flag to pause, `rm` it to resume; `essaim serve` defaults
// it to <config-dir>/essaim/heal.disabled). An empty path turns the kill-switch
// off (always-on healing). Call before Start.
func (w *Watcher) SetDisableFlagPath(p string) { w.disableFlagPath = p }

// SetLiveTools registers the predicate CheckOnce uses to learn the CURRENT set of
// wired-tool names, so a target whose tool was removed by `essaim unwire` on a
// running daemon is no longer healed — no restart needed (RESPECTS-UNWIRE / P1).
// Without it the watcher heals every boot-time target unconditionally (the prior
// behavior). The predicate is stored under the watcher lock so it is safe even if
// set concurrently with a running loop (it is normally set before Start). See the
// liveTools field doc for the fail-toward-healing contract.
func (w *Watcher) SetLiveTools(f func() (map[string]bool, bool)) {
	w.mu.Lock()
	w.liveTools = f
	w.mu.Unlock()
}

// targetIsLive reports whether target t should still be healed given the live
// wired-tools set. A target with no Tool name is always live (the filter only
// gates named targets). With no predicate set, every target is live (prior
// behavior). When the predicate cannot determine the set (ok=false), we FAIL
// TOWARD HEALING — a transient unreadable config must not silently stop the guard.
func (w *Watcher) targetIsLive(t Target) bool {
	if t.Tool == "" {
		return true // unnamed target — not subject to the live-tools filter
	}
	w.mu.Lock()
	f := w.liveTools
	w.mu.Unlock()
	if f == nil {
		return true // no live overlay configured — heal as before
	}
	set, ok := f()
	if !ok {
		return true // undeterminable — fail toward healing (keep guarding)
	}
	return set[t.Tool]
}

// SetOnError registers a sink for errors raised by background CheckOnce passes
// (startup + the loop) — principally an unparseable config (P1). Without it those
// errors would be invisible. The sink is stored under the watcher lock so it is
// safe even if set concurrently with a running loop (it is normally set before
// Start).
func (w *Watcher) SetOnError(f func(error)) {
	w.mu.Lock()
	w.onError = f
	w.mu.Unlock()
}

// reportError forwards err to the onError sink if one is set (and err is non-nil).
// The sink reference is read under the lock so it never races SetOnError.
func (w *Watcher) reportError(err error) {
	if err == nil {
		return
	}
	w.mu.Lock()
	f := w.onError
	w.mu.Unlock()
	if f != nil {
		f(err)
	}
}

// disabled reports whether the live kill-switch flag file is present. A missing
// flag (the common case) means healing is active. Any stat error other than
// "not exist" is treated as "active" (fail toward healing — a transient stat
// error must not silently disable the guard).
func (w *Watcher) disabled() bool {
	if w.disableFlagPath == "" {
		return false
	}
	_, err := os.Stat(w.disableFlagPath)
	return err == nil
}

// Start arms the fsnotify watch over the targets' parent directories and runs the
// debounced heal loop in its own goroutine. It returns once the watch is armed;
// the loop runs until ctx is cancelled or Close is called. With no targets it is
// a no-op (no watcher, no goroutine) so a serve with nothing wired stays inert.
//
// Start does an initial CheckOnce so a base_url that was already clobbered before
// the watcher came up is healed immediately, not only on the next edit.
func (w *Watcher) Start(ctx context.Context) error {
	if len(w.targets) == 0 {
		return nil
	}
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	// Watch the PARENT directory of each target. Watching the dir (not the file)
	// survives atomic save-via-rename (write tmp + rename over the target), which
	// is how most editors/IDEs write config — a direct file watch would lose the
	// inode on rename and go deaf.
	seen := make(map[string]bool)
	for _, t := range w.targets {
		dir := filepath.Dir(t.Path)
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		_ = fw.Add(dir) // best-effort; a missing dir just means nothing to heal yet
	}

	w.mu.Lock()
	w.watcher = fw
	w.mu.Unlock()

	// Heal anything already clobbered at startup (off the hot path; this runs in
	// Start's caller goroutine, which is `essaim serve`'s setup, not a request). An
	// error here (e.g. an unparseable config) is surfaced via the onError sink so
	// it is VISIBLE, not silently swallowed (P1).
	if _, err := w.CheckOnce(); err != nil {
		w.reportError(err)
	}

	go w.loop(ctx, fw)
	return nil
}

// Close stops the watcher. Safe to call once; idempotent against a nil watcher.
func (w *Watcher) Close() error {
	w.mu.Lock()
	fw := w.watcher
	w.watcher = nil
	w.mu.Unlock()
	if fw != nil {
		return fw.Close()
	}
	return nil
}

// loop is the debounced heal loop. fsnotify events (re)arm a single timer; when
// it fires we run one CheckOnce pass over every target. Coalescing a burst of
// events into one pass keeps an IDE upgrade that rewrites several files cheap.
func (w *Watcher) loop(ctx context.Context, fw *fsnotify.Watcher) {
	var (
		timer   *time.Timer
		timerCh <-chan time.Time
	)
	arm := func() {
		if timer == nil {
			timer = time.NewTimer(w.interval)
		} else {
			timer.Reset(w.interval)
		}
		timerCh = timer.C
	}
	for {
		select {
		case <-ctx.Done():
			_ = fw.Close()
			return
		case _, ok := <-fw.Events:
			if !ok {
				return
			}
			arm() // any edit in a watched dir re-arms a heal pass
		case _, ok := <-fw.Errors:
			if !ok {
				return
			}
			// Ignore individual watcher errors; the next event re-arms a pass.
		case <-timerCh:
			if _, err := w.CheckOnce(); err != nil {
				w.reportError(err) // surface a parse/IO failure instead of swallowing it
			}
			timerCh = nil
		}
	}
}

// CheckOnce runs one synchronous heal pass over every target and returns how many
// were re-applied. It is the deterministic primitive the debounced loop calls; it
// is also the unit seam (no timing) the tests exercise. Pure local file I/O.
//
// It is serialized by checkMu so two concurrent passes (the debounced loop and an
// explicit call, or two loop fires) cannot interleave a read/transform/write on
// the same file — race-safe by construction, not by luck (P2).
//
// While the live kill-switch flag exists it is a no-op (healing disabled WITHOUT
// a restart). For each target it then DELEGATES the whole heal-or-leave decision
// to Repair: there is NO "file lacks the essaim url ⇒ rewrite" pre-check (that was
// the ship-blocker — it stomped a base_url a user deliberately set to something
// else). Repair returns changed only when essaim's OWN previously-written value
// was clobbered; a deliberate user override yields changed=false and is left
// byte-untouched. A Repair error (e.g. an unparseable config) is surfaced, not
// silently swallowed.
func (w *Watcher) CheckOnce() (int, error) {
	w.checkMu.Lock()
	defer w.checkMu.Unlock()

	if w.disabled() {
		return 0, nil // live kill-switch engaged: heal nothing this pass
	}

	healed := 0
	for _, t := range w.targets {
		// RESPECTS-UNWIRE (P1): a target whose tool was removed from the live
		// wired-tools set by `essaim unwire` is no longer guarded — skip it so the
		// running daemon stops rewriting a file the user unwired to reclaim. No
		// restart needed; the targets slice is boot-time, this overlay is live.
		if !w.targetIsLive(t) {
			continue
		}
		cur, err := os.ReadFile(t.Path)
		if err != nil {
			if os.IsNotExist(err) {
				continue // nothing to heal: the tool isn't configured here (yet)
			}
			return healed, err
		}
		if t.Repair == nil {
			continue
		}
		// Repair owns the decision: it heals ONLY essaim's own clobbered value and
		// reports changed=false for both "healthy" and "deliberate user override".
		out, changed, rerr := t.Repair(cur)
		if rerr != nil {
			return healed, rerr
		}
		if !changed {
			continue
		}
		if err := writeAtomic(t.Path, out); err != nil {
			return healed, err
		}
		w.healed.Add(1)
		healed++
	}
	return healed, nil
}

// writeAtomic writes content to path via a temp file + rename, preserving the
// existing file mode (default 0644). The rename is atomic on a single filesystem,
// so an IDE that reads the config mid-heal never sees a torn file.
func writeAtomic(path string, content []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".essaim-heal-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(content); err != nil {
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
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
