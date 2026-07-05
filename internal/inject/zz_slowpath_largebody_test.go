package inject

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"oikos/internal/rules"
)

// RESIDUAL slow/stripped-path overrun (follow-up to P0-1).
//
// P0-1 fixed the FAST path (no prior block → single-copy headInject). But when the
// inject INPUT already carries a PRIOR oikos block — which happens in normal use
// because the NativeFileEmitter writes a block into CLAUDE.md and the tool then
// sends that file's content back as context — the code takes the STRIP-then-rebuild
// path. Before this fix that path did a ~2-copy doubling-buffer rewrite
// (spliceElement bytes.Buffer growth → splice whole-array re-copy); a large history
// blows the 15ms budget → safeBuild fail-opens to verbatim → the moat silently
// disappears on exactly the emitter→input→strip round-trip.
//
// This asserts: a ~5MB body that ALREADY contains a prior oikos block (forces the
// strip path) injects the fresh block in < 15ms AND byte-exact (old block removed,
// fresh block at index 0, every other message verbatim).
func TestMultiMBStrippedPayloadInjectsUnderBudget(t *testing.T) {
	// A prior oikos block exactly as the emitter would have written it inbound
	// (instruction role, sentinel-fenced, payload-agnostic). It MUST be recognized
	// by isOikosBlock and stripped → forces the SLOW path.
	priorBlock := `{"role":"system","content":` + jsonStr(begin+"\nold stale rule body\n"+end) + `}`

	huge := strings.Repeat("x", 5*1024*1024) // 5MB user content
	hugeUser := `{"role":"user","content":` + jsonStr(huge) + `}`
	plainSys := `{"role":"system","content":"sys"}`
	asstMsg := `{"role":"assistant","content":"prior answer"}`

	// Order: prior oikos block, then a plain system, a huge user, an assistant.
	// After stripping the prior block, three client messages survive and must
	// pass through byte-identically; the fresh block leads at index 0.
	body := []byte(`{"model":"gpt-4o","messages":[` +
		priorBlock + `,` + plainSys + `,` + hugeUser + `,` + asstMsg +
		`]}`)

	r := mkRule("a", "Use Postgres", "Always use PostgreSQL.")

	start := time.Now()
	res, err := Build(body, []rules.Rule{r}, []string{"a"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Build on 5MB stripped payload: %v", err)
	}
	t.Logf("strip-path 5MB inject took %v", elapsed)

	// SLOW path must have run (a prior block was present and stripped).
	// Sanity: result must be valid JSON.
	if !json.Valid(res.Body) {
		t.Fatalf("stripped 5MB body must be valid JSON")
	}

	if !raceEnabled && elapsed > 15*time.Millisecond {
		// The race detector inflates timings ~10x; only enforce the wall-clock
		// budget in a normal build. Correctness (below) is asserted always.
		t.Fatalf("5MB strip-path inject took %v, must be < 15ms (single pre-sized copy)", elapsed)
	}

	// Byte-exactness — EXACTLY ONE block (the old one removed, the new one added).
	if n := countBeginInInstruction(t, res.Body); n != 1 {
		t.Fatalf("want exactly 1 block (old stripped, fresh injected), got %d", n)
	}
	// The old stale body must be gone; the snapshot must be oikos-free.
	if strings.Contains(string(res.Body), "old stale rule body") {
		t.Fatalf("stale prior-block body must be stripped out")
	}
	if strings.Contains(string(res.Snapshot.CleanMessagesJSON), begin) {
		t.Fatalf("capture snapshot must be oikos-free, contains sentinel")
	}

	// The fresh block must lead at array index 0.
	out := parseOut(t, res.Body)
	if len(out) != 4 { // fresh block + 3 surviving client msgs
		t.Fatalf("want 4 messages (block + 3 survivors), got %d: %v", len(out), summarizeRoles(out))
	}
	if !strings.HasPrefix(out[0].Content, begin+"\n") || !strings.HasSuffix(out[0].Content, "\n"+end) {
		t.Fatalf("fresh block must lead at index 0, got role=%q", out[0].Role)
	}

	// Every OTHER message must be byte-verbatim: assert each surviving client
	// element's exact original bytes appear in the output unchanged.
	for _, want := range []string{plainSys, hugeUser, asstMsg} {
		if !bytes.Contains(res.Body, []byte(want)) {
			label := want
			if len(label) > 60 {
				label = label[:60] + "…"
			}
			t.Fatalf("surviving client element NOT byte-verbatim in output: %s", label)
		}
	}

	// The huge user content itself must be preserved byte-identical.
	if !strings.Contains(string(res.Body), huge) {
		t.Fatalf("5MB user content must be preserved byte-identical")
	}
}

func summarizeRoles(out []struct{ Role, Content string }) []string {
	r := make([]string, 0, len(out))
	for _, m := range out {
		r = append(r, m.Role)
	}
	return r
}

// Strip-path boundary: the body was ONLY a prior oikos block (so after stripping,
// the surviving `clean` slice is EMPTY) AND a match exists. headInjectStripped
// must emit a valid JSON array `[block]` (the fresh block alone, no trailing
// comma, no dangling element) — the same output the prior spliceElement produced.
func TestStripPathEmptyCleanInjectsValidJSON(t *testing.T) {
	blk := `{"role":"system","content":"` + begin + `\nx\n` + end + `"}`
	body := []byte(`{"model":"gpt-4o","messages":[` + blk + `]}`)
	res, err := Build(body, []rules.Rule{mkRule("a", "A", "ba")}, []string{"a"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !json.Valid(res.Body) {
		t.Fatalf("empty-clean strip output must be valid JSON: %q", res.Body)
	}
	if n := countBeginInInstruction(t, res.Body); n != 1 {
		t.Fatalf("want exactly 1 fresh block, got %d: %s", n, res.Body)
	}
	out := parseOut(t, res.Body)
	if len(out) != 1 {
		t.Fatalf("want exactly 1 message (the fresh block), got %d", len(out))
	}
}
