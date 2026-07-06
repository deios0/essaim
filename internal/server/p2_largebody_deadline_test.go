package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"essaim/internal/rules"
)

// P2-1a: the intercept deadline must SCALE modestly with body size so a multi-MB
// resent history still injects. A flat 15ms fails open (zero injection) exactly
// when the session is largest — the opposite of what essaim should do (upstream RTT
// dwarfs the extra few ms). scaleDeadline(base, bodyLen) grows the budget a few
// ms/MB, capped ~100ms.
func TestScaleDeadlineGrowsWithBodySize(t *testing.T) {
	const base = 15 * time.Millisecond
	small := scaleDeadline(base, 1024)   // ~1KB → base
	oneMB := scaleDeadline(base, 1<<20)  // 1MB
	tenMB := scaleDeadline(base, 10<<20) // 10MB
	huge := scaleDeadline(base, 1<<30)   // 1GB → capped

	if small != base {
		t.Fatalf("a small body must keep the base deadline, got %v", small)
	}
	if oneMB <= base {
		t.Fatalf("a 1MB body must get MORE than the base deadline, got %v", oneMB)
	}
	if tenMB <= oneMB {
		t.Fatalf("a 10MB body must get more budget than a 1MB body: 10MB=%v 1MB=%v", tenMB, oneMB)
	}
	if huge > maxInterceptDeadline {
		t.Fatalf("the scaled deadline must be capped at %v, got %v", maxInterceptDeadline, huge)
	}
	if maxInterceptDeadline > 200*time.Millisecond {
		t.Fatalf("the cap must stay modest (≤200ms), got %v", maxInterceptDeadline)
	}
}

// P2-1a end-to-end: a multi-MB body whose build takes ~25ms (longer than the flat
// 15ms base, but well within the scaled budget) must STILL inject — not fail open.
// buildHook simulates the extra parse cost of a big body.
func TestLargeBodyStillInjectsWithScaledDeadline(t *testing.T) {
	if raceEnabledServer {
		t.Skip("timing-sensitive: the race detector inflates wall-clock ~10x")
	}
	huge := strings.Repeat("the quick brown fox jumped over the lazy dog. ", 200_000) // ~9MB
	jsonStr := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	body := []byte(`{"model":"gpt-4o","messages":[` +
		`{"role":"user","content":` + jsonStr(huge) + `},` +
		`{"role":"user","content":"which database should I use?"}` +
		`]}`)

	dir := vaultWith(t, map[string]string{
		"pg.md": "---\nid: pg\ntitle: Use Postgres\nstatus: active\nconfidence: 0.9\nweight: 1\n---\nAlways use PostgreSQL for relational data; it is the database of choice.",
	})
	store, err := rules.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	in := newInjectorWithStore(store, rules.GuardConfig{EagerBytes: 4096, MatchFloor: 0.0})
	// Simulate the build being modestly slow (25ms) — over the 15ms base, under the
	// scaled budget for a ~9MB body.
	in.buildHook = func() { time.Sleep(25 * time.Millisecond) }

	out, _, berr := in.safeBuild(context.Background(), body)
	if berr != nil {
		t.Fatalf("a ~9MB body under the scaled deadline must inject, got err %v", berr)
	}
	if strings.Count(string(out), rules.ESSAIM_BEGIN) != 1 {
		t.Fatalf("scaled deadline must let the large body inject exactly 1 block")
	}
}

// P2-1b: buildOnce must be cancellation-aware — when its ctx is already cancelled
// it must stop and return promptly (ctx error) rather than parsing/matching the
// whole multi-MB body (detached CPU burn after the deadline fired).
func TestBuildOnceStopsOnCancelledCtx(t *testing.T) {
	huge := strings.Repeat("word ", 500_000) // ~2.5MB
	jsonStr := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":` + jsonStr(huge) + `}]}`)

	dir := vaultWith(t, map[string]string{
		"pg.md": "---\nid: pg\ntitle: Use Postgres\nstatus: live\nconfidence: 0.9\nweight: 1\n---\nUse PostgreSQL.",
	})
	store, _ := rules.NewStore(dir)
	in := newInjectorWithStore(store, rules.GuardConfig{EagerBytes: 4096})
	ix := in.store.Index()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before we start

	body2, _, err := in.buildOnce(ctx, body, ix)
	if err == nil {
		t.Fatalf("an already-cancelled ctx must make buildOnce bail with an error")
	}
	if !isCtxErr(err) {
		t.Fatalf("buildOnce must return the ctx error on cancel, got %v", err)
	}
	// Fail-open contract: the returned body is the verbatim original (never mutated).
	if string(body2) != string(body) {
		t.Fatalf("cancelled buildOnce must return the verbatim original body")
	}
}

// P2-1b: a cancellation surfaced via the goroutine result must be classified as
// a DEGRADING deadline event by safeBuild (never an honest miss), so the server
// forwards verbatim + marks degraded.
func TestSafeBuildCancelIsDegraded(t *testing.T) {
	dir := vaultWith(t, map[string]string{
		"pg.md": "---\nid: pg\ntitle: Use Postgres\nstatus: live\nconfidence: 0.9\nweight: 1\n---\nUse PostgreSQL.",
	})
	store, _ := rules.NewStore(dir)
	in := newInjectorWithStore(store, rules.GuardConfig{EagerBytes: 4096})
	// A tiny deadline + a slow build forces the deadline path.
	in.deadline = 2 * time.Millisecond
	in.buildHook = func() { time.Sleep(40 * time.Millisecond) }

	orig := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"which database?"}]}`)
	out, _, err := in.safeBuild(context.Background(), orig)
	if err != errDeadline {
		t.Fatalf("deadline overrun must classify as errDeadline (degrading), got %v", err)
	}
	if string(out) != string(orig) {
		t.Fatalf("fail-open must forward the verbatim original body")
	}
}

// isCtxErr reports whether err is a context cancellation/deadline error.
func isCtxErr(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}
