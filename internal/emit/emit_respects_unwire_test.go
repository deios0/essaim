package emit

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"essaim/internal/rules"
)

// P1 (respects-unwire): the NativeFileEmitter snapshots its wired tools once at
// boot, so `essaim unwire <tool>` on a running daemon must stop the emitter from
// re-injecting that tool's block on the next index swap — no restart. Mirrors
// heal.Watcher.SetLiveTools. With a live-tools predicate installed, a tool no
// longer in the live set is SKIPPED (its file is never written), while a still-
// live tool keeps emitting.
func TestEmitterRespectsUnwireSkipsRemovedTool(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "CLAUDE.md")
	unwired := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(live, []byte("# live\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unwired, []byte("# unwired\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	e := New(rules.GuardConfig{EagerBytes: 4096}, []Tool{
		{Name: "claude-code", NativeFile: live},
		{Name: "codex", NativeFile: unwired},
	})
	e.SetDebounce(0)

	// codex has been unwired on the running daemon: only claude-code's native file
	// is live now. The live set is keyed by NATIVE-FILE path (wire.LiveWiredTools
	// includes both name and file keys); the emitter checks the file it writes.
	e.SetLiveTools(func() (map[string]bool, bool) {
		return map[string]bool{"claude-code": true, live: true}, true
	})

	_, results, err := e.EmitNowWithResults(liveIndex(t))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]TargetStatus{}
	for _, r := range results {
		got[r.Name] = r.Status
	}
	if got["claude-code"] != StatusWritten {
		t.Fatalf("still-wired tool must be written, got %v", got["claude-code"])
	}
	if got["codex"] != StatusSkipped {
		t.Fatalf("unwired tool must be SKIPPED (respects-unwire), got %v", got["codex"])
	}

	liveContent, _ := os.ReadFile(live)
	if !strings.Contains(string(liveContent), rules.ESSAIM_BEGIN) {
		t.Fatalf("live tool file must carry the block:\n%s", liveContent)
	}
	unwiredContent, _ := os.ReadFile(unwired)
	if strings.Contains(string(unwiredContent), rules.ESSAIM_BEGIN) {
		t.Fatalf("unwired tool file must NOT be written (respects-unwire):\n%s", unwiredContent)
	}
}

// P1 (respects-unwire, PER-PROJECT): two projects wire the SAME tool name
// ("claude-code") to DIFFERENT native files. Unwiring one project must stop only
// ITS emit — the liveness check keys on the native file, not the tool name, so
// the other project's same-named tool keeps emitting (codex integration review).
func TestEmitterRespectsUnwirePerProjectSameToolName(t *testing.T) {
	dir := t.TempDir()
	projA := filepath.Join(dir, "a", "CLAUDE.md")
	projB := filepath.Join(dir, "b", "CLAUDE.md")
	for _, p := range []string{projA, projB} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("# user\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e := New(rules.GuardConfig{EagerBytes: 4096}, []Tool{
		{Name: "claude-code", NativeFile: projA},
		{Name: "claude-code", NativeFile: projB},
	})
	e.SetDebounce(0)
	// projA was unwired: only projB's file is live (the name "claude-code" is still
	// present because projB keeps it, which is exactly the trap — name-keying would
	// wrongly keep writing projA).
	e.SetLiveTools(func() (map[string]bool, bool) {
		return map[string]bool{"claude-code": true, projB: true}, true
	})
	_, results, err := e.EmitNowWithResults(liveIndex(t))
	if err != nil {
		t.Fatal(err)
	}
	st := map[string]TargetStatus{}
	for _, r := range results {
		st[r.NativeFile] = r.Status
	}
	if st[projA] != StatusSkipped {
		t.Fatalf("unwired project A must be SKIPPED, got %v", st[projA])
	}
	if st[projB] != StatusWritten {
		t.Fatalf("still-wired project B must be written, got %v", st[projB])
	}
	if b, _ := os.ReadFile(projA); strings.Contains(string(b), rules.ESSAIM_BEGIN) {
		t.Fatalf("unwired project A file must NOT be written")
	}
}

// P1 (respects-unwire, fail-toward-emitting): when the live-tools predicate can
// not determine the set (ok=false, e.g. a transient unreadable config) the
// emitter must keep emitting to every wired tool (matches heal's fail-toward-
// healing). No predicate set at all also emits to every tool (prior behavior).
func TestEmitterUndeterminableLiveToolsStillEmits(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(a, []byte("# a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := New(rules.GuardConfig{EagerBytes: 4096}, []Tool{{Name: "claude-code", NativeFile: a}})
	e.SetDebounce(0)
	e.SetLiveTools(func() (map[string]bool, bool) { return nil, false }) // undeterminable

	_, results, err := e.EmitNowWithResults(liveIndex(t))
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != StatusWritten {
		t.Fatalf("undeterminable live set must FAIL TOWARD EMITTING (keep guarding), got %v", results[0].Status)
	}
}

// P1 (surface per-target failures): OnIndexSwap (the daemon path, debounce 0 =
// synchronous) must NOT silently discard per-target emit failures — a failed
// target (here a refused credential path) must be surfaced to the registered
// error sink so a wired tool's file can never go permanently stale with zero
// signal. Mirrors heal.Watcher.SetOnError.
func TestEmitterOnIndexSwapSurfacesFailures(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(good, []byte("# good\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A target PATH that itself trips the credential pattern → StatusRefused, which
	// is a signal the daemon must surface (not silently drop).
	bad := filepath.Join(dir, "AKIAIOSFODNN7EXAMPLE.md")

	e := New(rules.GuardConfig{EagerBytes: 4096}, []Tool{
		{Name: "good", NativeFile: good},
		{Name: "bad", NativeFile: bad},
	})
	e.SetDebounce(0) // synchronous OnIndexSwap path (the immediate branch)

	var mu sync.Mutex
	var seen []TargetResult
	e.SetOnEmitProblem(func(r TargetResult) {
		mu.Lock()
		seen = append(seen, r)
		mu.Unlock()
	})

	e.OnIndexSwap(liveIndex(t))

	mu.Lock()
	defer mu.Unlock()
	var sawBad bool
	for _, r := range seen {
		if r.Name == "bad" {
			sawBad = true
		}
	}
	if !sawBad {
		t.Fatalf("OnIndexSwap must surface the refused/failed target to the problem sink; saw %+v", seen)
	}
}

// P2-2 (surface per-target failures on the DEBOUNCED path too): the immediate
// OnIndexSwap branch (debounce 0) surfaces failures to the sink, but the live
// daemon runs with debounce > 0, where the emit happens inside a time.AfterFunc
// timer. That timer's ([]TargetResult, error) must ALSO flow to the problem sink,
// or a wired tool's file goes permanently stale with zero signal on the real code
// path. This guards the debounced branch specifically so a future refactor can't
// silently re-drop it.
func TestEmitterOnIndexSwapDebouncedSurfacesFailures(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(good, []byte("# good\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A refused credential PATH → StatusRefused, which the daemon must surface even
	// when it happens on the debounced timer path.
	bad := filepath.Join(dir, "AKIAIOSFODNN7EXAMPLE.md")

	e := New(rules.GuardConfig{EagerBytes: 4096}, []Tool{
		{Name: "good", NativeFile: good},
		{Name: "bad", NativeFile: bad},
	})

	var mu sync.Mutex
	var seen []TargetResult
	e.SetOnEmitProblem(func(r TargetResult) {
		mu.Lock()
		seen = append(seen, r)
		mu.Unlock()
	})

	// Deterministic wait: the emit-done signal fires once the coalesced debounced
	// emit completes, so we assert only AFTER the timer path has run (no flaky
	// sleep-past-debounce under -race).
	done := make(chan struct{}, 4)
	e.SetOnEmitDone(func() { done <- struct{}{} })
	e.SetDebounce(30 * time.Millisecond) // > 0 ⇒ the timer (debounced) branch

	e.OnIndexSwap(liveIndex(t))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("debounced emit did not complete within 5s (signal never fired)")
	}

	mu.Lock()
	defer mu.Unlock()
	var sawBad bool
	for _, r := range seen {
		if r.Name == "bad" {
			sawBad = true
		}
	}
	if !sawBad {
		t.Fatalf("the DEBOUNCED OnIndexSwap path must surface the refused/failed target to the problem sink; saw %+v", seen)
	}
}
