package heal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const proxyURL = "http://127.0.0.1:4141/v1"

// jsonTarget builds a Target whose Repair faithfully mirrors the production
// contract (internal/wire.repairBaseURL): it heals ONLY when essaim's own value
// was clobbered to a vendor default or dropped, and LEAVES a deliberate user
// override alone. This keeps the watcher-level tests honest about the real
// heal-vs-leave decision rather than a "rewrite anything non-essaim" stand-in.
func jsonTarget(path string) Target {
	return Target{
		Path:        path,
		ExpectedURL: proxyURL,
		LastWritten: proxyURL,
		Repair: func(cur []byte) ([]byte, bool, error) {
			s := string(cur)
			if strings.Contains(s, proxyURL) {
				return cur, false, nil // already healthy — leave alone
			}
			// Heal only a vendor-default clobber or an emptied value; a different
			// non-vendor URL is a deliberate user override → leave it byte-exact.
			vendor := strings.Contains(s, "api.openai.com") ||
				strings.Contains(s, "api.anthropic.com")
			empty := strings.Contains(s, `"apiBase":""`)
			if !vendor && !empty {
				return cur, false, nil // user override
			}
			return []byte(`{"apiBase":"` + proxyURL + `"}`), true, nil
		},
	}
}

// namedTarget is jsonTarget plus a Tool name, so the running-watcher RESPECTS-
// UNWIRE tests can assert that a target whose tool is no longer in the live
// wired-tools set is skipped.
func namedTarget(tool, path string) Target {
	t := jsonTarget(path)
	t.Tool = tool
	return t
}

// THE CORE TEST: an IDE update overwrites the tool's base_url (drops the essaim
// proxy), silently bypassing essaim. The watcher must re-apply the essaim base_url
// within the heal window.
func TestWatcherReappliesOverwrittenBaseURL(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	// Start healthy: apiBase points at essaim.
	mustWrite(t, cfg, `{"apiBase":"`+proxyURL+`"}`)

	w := New([]Target{jsonTarget(cfg)})
	w.interval = 20 * time.Millisecond // test seam: short debounce
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Close()

	// Simulate the IDE update clobbering the base_url (Cursor/VSCode/Continue
	// rewriting config.json on upgrade) — the essaim proxy is gone.
	mustWrite(t, cfg, `{"apiBase":"https://api.openai.com/v1"}`)

	// The watcher must re-apply the essaim base_url within the heal window (≤5s;
	// tests use the short debounce so this is fast).
	deadline := time.After(5 * time.Second)
	for {
		b, _ := os.ReadFile(cfg)
		if strings.Contains(string(b), proxyURL) {
			return // healed
		}
		select {
		case <-deadline:
			t.Fatalf("watcher did not re-apply the essaim base_url; config still:\n%s", b)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A healthy config that already points at essaim must NOT be rewritten (no churn,
// no needless disk writes). HealCount stays 0.
func TestWatcherLeavesHealthyConfigAlone(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	mustWrite(t, cfg, `{"apiBase":"`+proxyURL+`"}`)

	w := New([]Target{jsonTarget(cfg)})
	w.interval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Close()

	// Touch the file WITHOUT removing the essaim url (a benign edit).
	mustWrite(t, cfg, `{"apiBase":"`+proxyURL+`","theme":"dark"}`)

	time.Sleep(300 * time.Millisecond)
	if n := w.HealCount(); n != 0 {
		t.Fatalf("a config that still points at essaim must not be re-applied; HealCount=%d", n)
	}
	// And the benign edit must be preserved (we never clobber a healthy file).
	b, _ := os.ReadFile(cfg)
	if !strings.Contains(string(b), "theme") {
		t.Fatalf("a benign edit to a healthy config was clobbered:\n%s", b)
	}
}

// CheckOnce is the synchronous heal primitive the debounced loop calls — it
// re-applies a clobbered base_url exactly once and reports it healed. This is the
// deterministic seam (no timing) the loop is built on.
func TestCheckOnceHealsAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	mustWrite(t, cfg, `{"apiBase":"https://api.openai.com/v1"}`)

	w := New([]Target{jsonTarget(cfg)})

	healed, err := w.CheckOnce()
	if err != nil {
		t.Fatalf("CheckOnce: %v", err)
	}
	if healed != 1 {
		t.Fatalf("first CheckOnce must heal exactly 1 target, got %d", healed)
	}
	b, _ := os.ReadFile(cfg)
	if !strings.Contains(string(b), proxyURL) {
		t.Fatalf("CheckOnce did not re-apply the base_url:\n%s", b)
	}
	// Second pass: already healthy ⇒ 0 heals (idempotent, no write churn).
	healed, err = w.CheckOnce()
	if err != nil {
		t.Fatalf("CheckOnce 2: %v", err)
	}
	if healed != 0 {
		t.Fatalf("second CheckOnce on a healthy file must heal 0, got %d", healed)
	}
}

// The watcher must be a no-op (no error, no goroutine) when there are no targets
// — `essaim serve` starts it only when tools are wired, but defensive: empty is
// safe.
func TestWatcherNoTargetsIsNoop(t *testing.T) {
	w := New(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start with no targets must not error: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// LOCAL-ONLY guarantee: the heal package must never make a network call. We
// assert the Repair seam is pure file I/O by confirming the watcher heals with no
// reachable network resource and the only effect is the local file. (The static
// phone-home grep gate covers the source; this guards behavior.)
func TestHealIsLocalFileIOOnly(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	mustWrite(t, cfg, `{"apiBase":"x"}`)

	var sideEffects atomic.Int64
	w := New([]Target{{
		Path:        cfg,
		ExpectedURL: proxyURL,
		Repair: func(cur []byte) ([]byte, bool, error) {
			sideEffects.Add(1) // proves Repair ran in-process, no network
			return []byte(`{"apiBase":"` + proxyURL + `"}`), true, nil
		},
	}})
	if _, err := w.CheckOnce(); err != nil {
		t.Fatalf("CheckOnce: %v", err)
	}
	if sideEffects.Load() == 0 {
		t.Fatal("Repair must be invoked in-process (local file I/O)")
	}
}

// THE SHIP-BLOCKER TEST (watcher level): a base_url the USER deliberately set to
// a non-essaim, non-vendor-default value must be LEFT ALONE across heal passes —
// the watcher must never stomp it back to essaim. Pairs with the unit-level
// internal/wire.TestRepairLeavesDeliberateUserOverrideAlone.
func TestWatcherLeavesUserSetURLAloneOnlyHealsClobber(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	// The user DELIBERATELY points Continue at their own gateway (NOT essaim).
	userSet := `{"apiBase":"https://my-gateway.example.com/v1"}`
	mustWrite(t, cfg, userSet)

	w := New([]Target{jsonTarget(cfg)})

	// Many passes — the user's choice must survive every one (no churn, 0 heals).
	for i := 0; i < 5; i++ {
		if n, err := w.CheckOnce(); err != nil || n != 0 {
			t.Fatalf("pass %d: a deliberate user override must never be healed; healed=%d err=%v", i, n, err)
		}
	}
	if b, _ := os.ReadFile(cfg); string(b) != userSet {
		t.Fatalf("user-set base_url was clobbered:\n got %s\nwant %s", b, userSet)
	}
	if w.HealCount() != 0 {
		t.Fatalf("HealCount must be 0 for a user override, got %d", w.HealCount())
	}

	// Now simulate an IDE update that factory-resets the value to a vendor default
	// (the ONLY case essaim heals). This MUST be re-pointed at the proxy.
	mustWrite(t, cfg, `{"apiBase":"https://api.openai.com/v1"}`)
	if n, err := w.CheckOnce(); err != nil || n != 1 {
		t.Fatalf("a vendor-default clobber must heal exactly once; healed=%d err=%v", n, err)
	}
	if b, _ := os.ReadFile(cfg); !strings.Contains(string(b), proxyURL) {
		t.Fatalf("clobbered value not re-pointed at essaim:\n%s", b)
	}
}

// P0(c): a LIVE kill-switch — a flag file checked at run time, not a boot-only
// env var — must disable healing WITHOUT a restart. While the flag exists,
// CheckOnce heals nothing; removing it resumes healing on the next pass.
func TestWatcherLiveDisableFlagStopsHealing(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	flag := filepath.Join(dir, "heal.disabled")
	mustWrite(t, cfg, `{"apiBase":"https://api.openai.com/v1"}`) // a clobber essaim would heal

	w := New([]Target{jsonTarget(cfg)})
	w.SetDisableFlagPath(flag)

	// Flag present → healing is OFF even though the config is clobbered.
	mustWrite(t, flag, "")
	if n, err := w.CheckOnce(); err != nil || n != 0 {
		t.Fatalf("with the kill-switch engaged, CheckOnce must heal 0; healed=%d err=%v", n, err)
	}
	if b, _ := os.ReadFile(cfg); !strings.Contains(string(b), "api.openai.com") {
		t.Fatalf("disabled watcher must not have touched the file:\n%s", b)
	}

	// Remove the flag → healing resumes on the next pass (no restart).
	if err := os.Remove(flag); err != nil {
		t.Fatalf("rm flag: %v", err)
	}
	if n, err := w.CheckOnce(); err != nil || n != 1 {
		t.Fatalf("after removing the flag, healing must resume; healed=%d err=%v", n, err)
	}
}

// P1 RESPECTS-UNWIRE (the heal-watcher must not fight `unwire` on a RUNNING
// daemon): on an already-running watcher, once a tool is removed from the live
// wired-tools set (what `essaim unwire` does to config.json), the watcher must
// STOP healing that tool's config file WITHOUT a restart. The targets list is
// built once at boot; the live-tools predicate is what makes unwire take effect
// in-process. A user who unwires to stop essaim touching their file must not find
// the daemon still rewriting it.
func TestWatcherRespectsUnwireOnRunningDaemon(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	mustWrite(t, cfg, `{"apiBase":"`+proxyURL+`"}`) // start healthy

	// The live wired-tools set the watcher consults. We flip "continue" out of it
	// mid-flight to simulate `essaim unwire continue` on the running daemon.
	var mu sync.Mutex
	wired := map[string]bool{"continue": true}
	w := New([]Target{namedTarget("continue", cfg)})
	w.SetLiveTools(func() (map[string]bool, bool) {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]bool, len(wired))
		for k, v := range wired {
			out[k] = v
		}
		return out, true
	})

	// Precondition: while STILL wired, a clobber is healed (the guard is live).
	mustWrite(t, cfg, `{"apiBase":"https://api.openai.com/v1"}`)
	if n, err := w.CheckOnce(); err != nil || n != 1 {
		t.Fatalf("while wired, a clobber must heal; healed=%d err=%v", n, err)
	}

	// Now `essaim unwire continue`: drop it from the live wired-tools set. No restart.
	mu.Lock()
	delete(wired, "continue")
	mu.Unlock()

	// Clobber the file again — the very thing the unwiring user wants essaim to STOP
	// touching. The watcher must NOT re-heal it now.
	clobber := `{"apiBase":"https://api.openai.com/v1"}`
	mustWrite(t, cfg, clobber)
	for i := 0; i < 5; i++ {
		if n, err := w.CheckOnce(); err != nil || n != 0 {
			t.Fatalf("pass %d: an unwired tool must NOT be healed; healed=%d err=%v", i, n, err)
		}
	}
	if b, _ := os.ReadFile(cfg); string(b) != clobber {
		t.Fatalf("an unwired tool's file must be left as the user/IDE left it; got:\n%s", b)
	}
	if w.HealCount() != 1 {
		t.Fatalf("only the pre-unwire heal should count; HealCount=%d want 1", w.HealCount())
	}
}

// RESPECTS-UNWIRE is per-target: unwiring ONE tool must not stop the watcher from
// healing a DIFFERENT tool that is still wired.
func TestWatcherUnwireIsPerTargetNotGlobal(t *testing.T) {
	dir := t.TempDir()
	cfgA := filepath.Join(dir, "continue.json")
	cfgB := filepath.Join(dir, "other.json")
	mustWrite(t, cfgA, `{"apiBase":"`+proxyURL+`"}`)
	mustWrite(t, cfgB, `{"apiBase":"`+proxyURL+`"}`)

	wired := map[string]bool{"continue": true, "other": true}
	w := New([]Target{namedTarget("continue", cfgA), namedTarget("other", cfgB)})
	w.SetLiveTools(func() (map[string]bool, bool) { return wired, true })

	// Unwire only "continue"; "other" stays wired.
	delete(wired, "continue")

	// Clobber BOTH. Only "other" (still wired) must heal.
	mustWrite(t, cfgA, `{"apiBase":"https://api.openai.com/v1"}`)
	mustWrite(t, cfgB, `{"apiBase":"https://api.openai.com/v1"}`)
	n, err := w.CheckOnce()
	if err != nil {
		t.Fatalf("CheckOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("exactly the still-wired tool must heal; healed=%d want 1", n)
	}
	if b, _ := os.ReadFile(cfgA); !strings.Contains(string(b), "api.openai.com") {
		t.Fatalf("the UNWIRED tool's file must be left clobbered:\n%s", b)
	}
	if b, _ := os.ReadFile(cfgB); !strings.Contains(string(b), proxyURL) {
		t.Fatalf("the still-wired tool's file must be healed:\n%s", b)
	}
}

// Fail-SAFE: if the live-tools predicate cannot determine the set (ok=false — a
// transient unreadable config), the watcher must KEEP healing (fail toward
// guarding), exactly like the kill-switch's stat-error policy. A transient read
// error must never silently stop the guard.
func TestWatcherHealsWhenLiveToolsUndeterminable(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	mustWrite(t, cfg, `{"apiBase":"https://api.openai.com/v1"}`)

	w := New([]Target{namedTarget("continue", cfg)})
	w.SetLiveTools(func() (map[string]bool, bool) { return nil, false }) // undeterminable

	if n, err := w.CheckOnce(); err != nil || n != 1 {
		t.Fatalf("an undeterminable live-tools set must fail toward healing; healed=%d err=%v", n, err)
	}
}

// A target with an empty Tool name (legacy/unnamed) is always considered live —
// the live-tools filter only gates targets that declare which tool they belong
// to. This keeps targets without a Tool (and the no-predicate default) healing.
func TestWatcherUnnamedTargetAlwaysHeals(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	mustWrite(t, cfg, `{"apiBase":"https://api.openai.com/v1"}`)

	w := New([]Target{jsonTarget(cfg)}) // no Tool set
	w.SetLiveTools(func() (map[string]bool, bool) { return map[string]bool{}, true })

	if n, err := w.CheckOnce(); err != nil || n != 1 {
		t.Fatalf("an unnamed target must heal regardless of the live set; healed=%d err=%v", n, err)
	}
}

// P0(c) cont.: a MISSING flag file (the common case) means healing is active.
func TestWatcherNoDisableFlagHealsNormally(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	mustWrite(t, cfg, `{"apiBase":"https://api.openai.com/v1"}`)
	w := New([]Target{jsonTarget(cfg)})
	w.SetDisableFlagPath(filepath.Join(dir, "does-not-exist.flag"))
	if n, err := w.CheckOnce(); err != nil || n != 1 {
		t.Fatalf("a missing flag means healing is on; healed=%d err=%v", n, err)
	}
}

// P2: CheckOnce is mutex-guarded — concurrent passes must be race-safe and never
// double-heal or interleave a read/transform/write. Run under -race to prove it.
func TestCheckOnceConcurrentPassesAreRaceSafe(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	mustWrite(t, cfg, `{"apiBase":"https://api.openai.com/v1"}`)
	w := New([]Target{jsonTarget(cfg)})

	const goroutines = 16
	var wg sync.WaitGroup
	var totalHealed atomic.Int64
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			n, err := w.CheckOnce()
			if err != nil {
				t.Errorf("CheckOnce: %v", err)
			}
			totalHealed.Add(int64(n))
		}()
	}
	wg.Wait()

	// Exactly ONE pass should have observed the clobber and healed it; the rest
	// see the already-healed file. Serialized access guarantees no double-heal.
	if got := totalHealed.Load(); got != 1 {
		t.Fatalf("concurrent passes must heal the clobber exactly once, got %d", got)
	}
	if w.HealCount() != 1 {
		t.Fatalf("HealCount must be exactly 1 after concurrent passes, got %d", w.HealCount())
	}
	if b, _ := os.ReadFile(cfg); !strings.Contains(string(b), proxyURL) {
		t.Fatalf("file not healed:\n%s", b)
	}
}

// P1: a Repair error from a background pass (startup + loop) must be SURFACED via
// the onError sink, not silently swallowed. Without this an unparseable config the
// user thinks essaim is guarding would fail invisibly.
func TestWatcherSurfacesRepairErrorViaOnError(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	mustWrite(t, cfg, `{"apiBase":"https://api.openai.com/v1"}`)

	var got atomic.Int64
	w := New([]Target{{
		Path:        cfg,
		ExpectedURL: proxyURL,
		Repair: func(cur []byte) ([]byte, bool, error) {
			return cur, false, errBoom
		},
	}})
	w.SetOnError(func(error) { got.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil { // Start's initial CheckOnce hits the error
		t.Fatalf("Start: %v", err)
	}
	defer w.Close()

	if got.Load() == 0 {
		t.Fatal("a startup Repair error must be reported via the onError sink")
	}
}

var errBoom = errBoomType{}

type errBoomType struct{}

func (errBoomType) Error() string { return "boom: unparseable config" }

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
