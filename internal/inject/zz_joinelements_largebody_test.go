package inject

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"essaim/internal/rules"
)

// P1-a-gap: the strip-path MAIN inject fix (headInjectStripped) landed, but
// joinElements still used a NON-presized doubling bytes.Buffer + a whole-array
// re-copy. joinElements is called for:
//
//   - the snapshot cleanArrJSON (build.go:~100) on the inject+stripped path, and
//   - the empty-match-stripped emit (build.go:~134), where its output IS the body.
//
// Same trigger as the original P0-1 bug — a large multi-turn body that already
// carries a prior essaim block forces the strip path — so a slow joinElements can
// blow the <15ms budget and fail-open the moat (snapshot side) / re-serialize
// slowly (empty-match side). This asserts joinElements is pre-sized single-copy:
// a ~5MB body exercising BOTH paths stays <15ms AND is byte-exact.

// EMPTY-MATCH STRIP PATH: a prior essaim block + NO matched rules. Build strips the
// block and rebuilds the array via joinElements — its output is the returned body.
// Must be <15ms and byte-exact (block gone, every survivor verbatim).
func TestJoinElementsEmptyMatchStripUnderBudget(t *testing.T) {
	priorBlock := `{"role":"system","content":` + jsonStr(begin+"\nold stale rule body\n"+end) + `}`
	huge := strings.Repeat("y", 5*1024*1024) // 5MB user content
	hugeUser := `{"role":"user","content":` + jsonStr(huge) + `}`
	plainSys := `{"role":"system","content":"sys"}`
	asstMsg := `{"role":"assistant","content":"prior answer"}`

	body := []byte(`{"model":"gpt-4o","messages":[` +
		priorBlock + `,` + plainSys + `,` + hugeUser + `,` + asstMsg + `]}`)

	start := time.Now()
	res, err := Build(body, nil, nil) // NO matched rules → empty-match strip path
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Build empty-match strip on 5MB: %v", err)
	}
	t.Logf("empty-match strip (joinElements) 5MB took %v", elapsed)

	if !json.Valid(res.Body) {
		t.Fatalf("empty-match strip output must be valid JSON")
	}
	if !raceEnabled && elapsed > 15*time.Millisecond {
		t.Fatalf("joinElements empty-match strip on 5MB took %v, must be < 15ms (single pre-sized copy)", elapsed)
	}

	// Byte-exact: the prior block is gone, NOTHING was injected, the three client
	// messages survive verbatim.
	if strings.Contains(string(res.Body), "old stale rule body") {
		t.Fatalf("the stripped prior-block body must be gone")
	}
	if n := countBeginInInstruction(t, res.Body); n != 0 {
		t.Fatalf("empty-match must inject NOTHING; found %d blocks", n)
	}
	out := parseOut(t, res.Body)
	if len(out) != 3 {
		t.Fatalf("want 3 surviving messages, got %d: %v", len(out), summarizeRoles(out))
	}
	for _, want := range []string{plainSys, hugeUser, asstMsg} {
		if !bytes.Contains(res.Body, []byte(want)) {
			label := want
			if len(label) > 60 {
				label = label[:60] + "…"
			}
			t.Fatalf("surviving client element NOT byte-verbatim: %s", label)
		}
	}
	if !strings.Contains(string(res.Body), huge) {
		t.Fatalf("5MB user content must be preserved byte-identical")
	}
}

// SNAPSHOT PATH: a prior essaim block + a matched rule. Build injects via
// headInjectStripped (already presized) but builds the capture snapshot
// cleanArrJSON via joinElements. The whole Build (including the snapshot build)
// must stay <15ms, and the snapshot must be essaim-free + byte-exact.
func TestJoinElementsSnapshotPathUnderBudget(t *testing.T) {
	priorBlock := `{"role":"system","content":` + jsonStr(begin+"\nold stale rule body\n"+end) + `}`
	huge := strings.Repeat("z", 5*1024*1024)
	hugeUser := `{"role":"user","content":` + jsonStr(huge) + `}`
	plainSys := `{"role":"system","content":"sys"}`

	body := []byte(`{"model":"gpt-4o","messages":[` +
		priorBlock + `,` + plainSys + `,` + hugeUser + `]}`)

	r := mkRule("a", "Use Postgres", "Always use PostgreSQL.")

	start := time.Now()
	res, err := Build(body, []rules.Rule{r}, []string{"a"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Build snapshot-path on 5MB: %v", err)
	}
	t.Logf("snapshot-path (joinElements cleanArrJSON) 5MB took %v", elapsed)

	if !raceEnabled && elapsed > 15*time.Millisecond {
		t.Fatalf("snapshot-path Build on 5MB took %v, must be < 15ms (pre-sized joinElements)", elapsed)
	}

	// Snapshot must be essaim-free and contain the surviving (huge) content verbatim.
	snap := string(res.Snapshot.CleanMessagesJSON)
	if strings.Contains(snap, begin) || strings.Contains(snap, "old stale rule body") {
		t.Fatalf("capture snapshot must be essaim-free")
	}
	if !strings.Contains(snap, huge) {
		t.Fatalf("snapshot must preserve the 5MB survivor byte-identical")
	}
	if !json.Valid(res.Snapshot.CleanMessagesJSON) {
		t.Fatalf("snapshot cleanArrJSON must be valid JSON")
	}
}
