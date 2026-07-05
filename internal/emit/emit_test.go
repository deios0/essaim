package emit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"oikos/internal/capture"
	"oikos/internal/rules"
)

func liveIndex(t *testing.T) *rules.Index {
	t.Helper()
	return rules.BuildIndex(rules.InjectableRules([]rules.Rule{
		{ID: "a", Title: "Use Postgres", Body: "Always use PostgreSQL", Status: "live", Weight: 0.9, Confidence: 0.9},
		{ID: "b", Title: "Tabs", Body: "Prefer tabs over spaces", Status: "live", Weight: 0.8, Confidence: 0.8},
		{ID: "d", Title: "Draft", Body: "a draft rule", Status: "draft", Weight: 0.9, Confidence: 0.9},
	}))
}

// Test 51: 2 live + 1 draft, 3 swaps in the debounce window ⇒ written ONCE, BOTH
// live rules fenced, NEVER the draft; a 4th identical swap does NOT rewrite.
//
// DETERMINISM (m4-polish): this used to arm a 40 ms debounce then Sleep(120 ms)
// hoping the timer had fired — which flakes under -race CPU starvation (the
// wall-clock sleep can elapse before the starved timer runs). It now waits on
// the SetOnEmitDone signal channel (the timer fires the callback exactly once
// when the coalesced emit completes) with a generous safety timeout, so the
// assertions only run AFTER the debounced write is on disk regardless of load.
// The "no-rewrite" idempotency check compares the emitter's deterministic write
// counter instead of coarse, FS-dependent file mtimes.
func TestNativeEmitterLiveOnlySameDelimiterDebounced(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(target, []byte("# My project\n\nuser content here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := New(rules.GuardConfig{EagerBytes: 4096}, []Tool{{Name: "claude-code", NativeFile: target}})

	// Signal channel: buffered so the emitter never blocks if the test is slow to
	// receive, and we drain it so a coalesced burst that somehow fires twice can't
	// wedge the emitter. The debounce stays short for speed, but correctness no
	// longer depends on the sleep outlasting the timer — we WAIT for the signal.
	emitted := make(chan struct{}, 8)
	e.SetOnEmitDone(func() { emitted <- struct{}{} })
	e.SetDebounce(40 * time.Millisecond)

	ix := liveIndex(t)
	// 3 swaps in the window → coalesce to ONE write.
	e.OnIndexSwap(ix)
	e.OnIndexSwap(ix)
	e.OnIndexSwap(ix)

	// Wait for the debounced emit to COMPLETE (deterministic), not a fixed sleep.
	// The timeout is a generous safety net (a hung emitter fails fast); it is never
	// the happy path, so -race slowness can't make it flake.
	select {
	case <-emitted:
	case <-time.After(5 * time.Second):
		t.Fatal("debounced emit did not complete within 5s (signal never fired)")
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, rules.OIKOS_BEGIN) || !strings.Contains(s, rules.OIKOS_END) {
		t.Fatalf("native file must contain the fenced block:\n%s", s)
	}
	if !strings.Contains(s, "Always use PostgreSQL") || !strings.Contains(s, "Prefer tabs over spaces") {
		t.Fatalf("both LIVE rules must be present:\n%s", s)
	}
	if strings.Contains(s, "a draft rule") {
		t.Fatalf("the DRAFT must NEVER be emitted:\n%s", s)
	}
	if !strings.Contains(s, "user content here") {
		t.Fatalf("user content must be preserved:\n%s", s)
	}
	// Exactly one fenced block (no duplication from the 3 swaps).
	if strings.Count(s, rules.OIKOS_BEGIN) != 1 {
		t.Fatalf("exactly one fenced block expected, got %d:\n%s", strings.Count(s, rules.OIKOS_BEGIN), s)
	}

	// The 3 coalesced swaps must have produced EXACTLY ONE disk write (debounce
	// proven by the counter, not by inspecting timing).
	if w := e.Writes(); w != 1 {
		t.Fatalf("3 coalesced swaps must produce exactly 1 write, got %d", w)
	}

	// A 4th identical swap must NOT rewrite (idempotent → write counter unchanged).
	// Deterministic: no mtime comparison, no sleep — EmitNow is synchronous and the
	// counter only advances on a real disk write.
	e.SetDebounce(0) // synchronous for the assertion
	if _, err := e.EmitNow(ix); err != nil {
		t.Fatal(err)
	}
	if w := e.Writes(); w != 1 {
		t.Fatalf("an identical emit must NOT rewrite the file (idempotent, git-clean): write count = %d, want 1", w)
	}
}

// P2-1: the STANDALONE (CLI) emit path must be idempotent across process
// boundaries. `oikos emit` constructs a FRESH Emitter each run, so its in-memory
// `last` map is always empty — the idempotent skip must therefore be driven by the
// on-disk fenced region, not only by the in-memory cache. A second emit with a
// brand-new emitter over an UNCHANGED vault+target must be a NO-OP: zero disk
// writes and the file's mtime untouched (so watchers/IDEs are not woken).
func TestNativeEmitterCLIPathIdempotentFreshEmitter(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(target, []byte("# My project\n\nuser content here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ix := liveIndex(t)

	// First emit: a fresh emitter (as the CLI does) writes the block.
	e1 := New(rules.GuardConfig{EagerBytes: 4096}, []Tool{{Name: "codex", NativeFile: target}})
	e1.SetDebounce(0)
	if _, err := e1.EmitNow(ix); err != nil {
		t.Fatalf("first emit: %v", err)
	}
	if w := e1.Writes(); w != 1 {
		t.Fatalf("first emit must write once, got %d", w)
	}
	fi1, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	before := fi1.ModTime()

	// Sleep past the filesystem mtime resolution so a rewrite WOULD be observable.
	time.Sleep(20 * time.Millisecond)

	// Second emit: a BRAND-NEW emitter (empty in-memory last map, exactly like a
	// second `oikos emit` invocation) over the unchanged file. This must be a no-op.
	e2 := New(rules.GuardConfig{EagerBytes: 4096}, []Tool{{Name: "codex", NativeFile: target}})
	e2.SetDebounce(0)
	if _, err := e2.EmitNow(ix); err != nil {
		t.Fatalf("second emit: %v", err)
	}
	if w := e2.Writes(); w != 0 {
		t.Fatalf("second emit over an unchanged file must NOT write (fresh CLI emitter idempotent), write count = %d, want 0", w)
	}
	fi2, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !fi2.ModTime().Equal(before) {
		t.Fatalf("second emit must not touch the file mtime: before=%v after=%v", before, fi2.ModTime())
	}
}

// Test 53: a CLAUDE.md with user content above and below the fence ⇒ only the
// fenced region is replaced; user bytes byte-identical; a backup exists.
func TestNativeEmitterReplacesOnlyFencedRegion(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "CLAUDE.md")
	above := "# Header\n\nABOVE user content\n\n"
	below := "\n\nBELOW user content\n"
	initial := above + rules.WrapBlock("- [H] Old: old rule") + below
	if err := os.WriteFile(target, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	e := New(rules.GuardConfig{EagerBytes: 4096}, []Tool{{Name: "cc", NativeFile: target}})
	e.SetDebounce(0)
	if _, err := e.EmitNow(liveIndex(t)); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	s := string(got)
	if !strings.HasPrefix(s, above) {
		t.Fatalf("content ABOVE the fence must be byte-identical:\n%q", s)
	}
	if !strings.HasSuffix(s, below) {
		t.Fatalf("content BELOW the fence must be byte-identical:\n%q", s)
	}
	if strings.Contains(s, "old rule") {
		t.Fatalf("the old fenced region must be REPLACED:\n%s", s)
	}
	if !strings.Contains(s, "Always use PostgreSQL") {
		t.Fatalf("the new live rules must be in the fence:\n%s", s)
	}
	// Backup exists.
	if _, err := os.Stat(target + ".oikos.bak"); err != nil {
		t.Fatalf("a restorable backup must exist: %v", err)
	}
}

// Test 54: a rule line with a key is dropped from the file; a target path
// matching a credential pattern is not written.
func TestNativeEmitterRefusesCredentialPathAndLine(t *testing.T) {
	// Line refusal: a live rule whose BODY contains a credential → its line is
	// dropped from the emitted block.
	dir := t.TempDir()
	target := filepath.Join(dir, "CLAUDE.md")
	_ = os.WriteFile(target, []byte("base\n"), 0o644)
	ix := rules.BuildIndex(rules.InjectableRules([]rules.Rule{
		{ID: "ok", Title: "Tabs", Body: "Prefer tabs over spaces", Status: "live", Weight: 0.9, Confidence: 0.9},
		{ID: "leak", Title: "Leak", Body: "api_key=sk-abcdefghijklmnop1234567890", Status: "live", Weight: 0.9, Confidence: 0.9},
	}))
	e := New(rules.GuardConfig{EagerBytes: 4096}, []Tool{{Name: "cc", NativeFile: target}})
	e.SetDebounce(0)
	if _, err := e.EmitNow(ix); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	if strings.Contains(string(got), "sk-abcdefghijklmnop1234567890") {
		t.Fatalf("a credential line must be dropped:\n%s", got)
	}
	if !strings.Contains(string(got), "Prefer tabs over spaces") {
		t.Fatalf("the clean rule must still be emitted:\n%s", got)
	}

	// Pre-public hardening: a context-gated markerless AWS *secret* access key in a
	// rule BODY must also be scrubbed before it reaches the native file.
	dir2 := t.TempDir()
	target2 := filepath.Join(dir2, "AGENTS.md")
	_ = os.WriteFile(target2, []byte("base\n"), 0o644)
	awsToken := "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	ix2 := rules.BuildIndex(rules.InjectableRules([]rules.Rule{
		{ID: "okp", Title: "Tabs", Body: "Prefer tabs over spaces", Status: "live", Weight: 0.9, Confidence: 0.9},
		{ID: "awsleak", Title: "Aws", Body: "use the aws secret access key " + awsToken + " in prod", Status: "live", Weight: 0.9, Confidence: 0.9},
	}))
	e3 := New(rules.GuardConfig{EagerBytes: 4096}, []Tool{{Name: "cc2", NativeFile: target2}})
	e3.SetDebounce(0)
	if _, err := e3.EmitNow(ix2); err != nil {
		t.Fatal(err)
	}
	got2, _ := os.ReadFile(target2)
	if strings.Contains(string(got2), awsToken) {
		t.Fatalf("the markerless AWS secret token must be scrubbed from the emitted file:\n%s", got2)
	}
	if !strings.Contains(string(got2), "Prefer tabs over spaces") {
		t.Fatalf("the clean rule must still be emitted alongside the scrubbed AWS rule:\n%s", got2)
	}

	// Path refusal: a target whose PATH carries a credential is not written.
	bad := filepath.Join(dir, "token=sk-abcdefghijklmnop1234567890.md")
	e2 := New(rules.GuardConfig{EagerBytes: 4096}, []Tool{{Name: "bad", NativeFile: bad}})
	e2.SetDebounce(0)
	if _, err := e2.EmitNow(liveIndex(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bad); err == nil {
		t.Fatal("a credential-bearing target PATH must NOT be written")
	}
}

// P1-b: dropCredentialLines must be BLOCK-aware. A live rule whose body carries a
// MULTI-LINE PEM private key would dodge a naive per-line scan (the base64 body
// lines look benign). The whole key block — header, base64 body, AND footer —
// must be scrubbed from the emitted native file; the clean rule survives.
func TestNativeEmitterDropsMultiLinePrivateKeyBlock(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "CLAUDE.md")
	_ = os.WriteFile(target, []byte("base\n"), 0o644)
	const keyBody = "MIIEpAIBAAKCAQEAPEMBODYSENTINEL0123456789base64moreandmore"
	pem := "-----BEGIN RSA PRIVATE KEY-----\n" + keyBody + "\nsecondBodyLine+slash/==\n-----END RSA PRIVATE KEY-----"
	ix := rules.BuildIndex(rules.InjectableRules([]rules.Rule{
		{ID: "ok", Title: "Tabs", Body: "Prefer tabs over spaces", Status: "live", Weight: 0.9, Confidence: 0.9},
		{ID: "leak", Title: "Leak", Body: "deploy key:\n" + pem, Status: "live", Weight: 0.9, Confidence: 0.9},
	}))
	e := New(rules.GuardConfig{EagerBytes: 8192}, []Tool{{Name: "cc", NativeFile: target}})
	e.SetDebounce(0)
	if _, err := e.EmitNow(ix); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	if strings.Contains(string(got), keyBody) {
		t.Fatalf("the PEM key BODY must be dropped from the emitted file:\n%s", got)
	}
	if strings.Contains(string(got), "PRIVATE KEY") {
		t.Fatalf("no BEGIN/END PRIVATE KEY line may survive in the emitted file:\n%s", got)
	}
	if !strings.Contains(string(got), "Prefer tabs over spaces") {
		t.Fatalf("the clean rule must still be emitted:\n%s", got)
	}
}

// Test 55: an un-wired tool ⇒ no native file written (opt-in, B-6).
func TestNativeEmitterOffByDefaultOptIn(t *testing.T) {
	dir := t.TempDir()
	// No tools wired.
	e := New(rules.GuardConfig{EagerBytes: 4096}, nil)
	e.SetDebounce(0)
	if _, err := e.EmitNow(liveIndex(t)); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("an un-wired emitter must write nothing, found %d files", len(entries))
	}
}

// Test 52: a native-file block round-tripped into a chat history is recognized
// by the capture hard-invariant recognizer (same delimiter, both channels).
func TestNativeEmitterSameRecognizerRoundTripStripped(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "CLAUDE.md")
	_ = os.WriteFile(target, []byte("base\n"), 0o644)
	e := New(rules.GuardConfig{EagerBytes: 4096}, []Tool{{Name: "cc", NativeFile: target}})
	e.SetDebounce(0)
	block, err := e.EmitNow(liveIndex(t))
	if err != nil {
		t.Fatal(err)
	}
	// The exact emitted block, pasted into a chat message, must trip the capture
	// recognizer (so it is excluded from learning — the B2 anti-echo on BOTH
	// channels).
	if !capture.ContainsCompleteOikosBlock(block) {
		t.Fatal("the emitted native block must be recognized by the capture recognizer")
	}
}

// Test 8 echo: only drafts → empty but well-fenced region (no panic).
func TestNativeEmitterEmptyLiveSetWellFenced(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "CLAUDE.md")
	_ = os.WriteFile(target, []byte("user\n"), 0o644)
	ix := rules.BuildIndex(rules.InjectableRules([]rules.Rule{
		{ID: "d", Title: "Draft", Body: "draft", Status: "draft", Weight: 0.9, Confidence: 0.9},
	}))
	e := New(rules.GuardConfig{EagerBytes: 4096}, []Tool{{Name: "cc", NativeFile: target}})
	e.SetDebounce(0)
	block, err := e.EmitNow(ix)
	if err != nil {
		t.Fatalf("empty live set must not error: %v", err)
	}
	if !strings.Contains(block, rules.OIKOS_BEGIN) || !strings.Contains(block, rules.OIKOS_END) {
		t.Fatalf("an empty live set must still produce a well-fenced (empty) region: %q", block)
	}
}
