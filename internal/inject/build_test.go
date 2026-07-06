package inject

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"essaim/internal/rules"
)

// Bridge finding #5: a multi-MB payload must inject under the in-mem budget,
// because Build splices ONE element over the raw bytes and never Marshals the
// whole body. We assert (a) correctness (exactly one block, the huge client msg
// preserved byte-identical) and (b) speed (well under the 15ms hot-path ceiling).
func TestMultiMBPayloadInjectsUnderBudget(t *testing.T) {
	huge := strings.Repeat("x", 5*1024*1024) // 5MB user content
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"system","content":"sys"},{"role":"user","content":` + jsonStr(huge) + `}]}`)

	r := mkRule("a", "Use Postgres", "Always use PostgreSQL.")
	start := time.Now()
	res, err := Build(body, []rules.Rule{r}, []string{"a"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Build on 5MB payload: %v", err)
	}
	if !raceEnabled && elapsed > 15*time.Millisecond {
		// The race detector inflates timings ~10x; only enforce the wall-clock
		// budget in a normal build. Correctness (below) is asserted always.
		t.Fatalf("5MB inject took %v, must be < 15ms (zero-Marshal splice)", elapsed)
	}
	// Correctness: exactly one block, huge user content preserved.
	if n := countBeginInInstruction(t, res.Body); n != 1 {
		t.Fatalf("want 1 block, got %d", n)
	}
	if !strings.Contains(string(res.Body), huge) {
		t.Fatalf("5MB user content must be preserved byte-identical")
	}
	// The result must remain valid JSON.
	var sink map[string]json.RawMessage
	if err := json.Unmarshal(res.Body, &sink); err != nil {
		t.Fatalf("spliced 5MB body must be valid JSON: %v", err)
	}
}

// Bridge finding #3: tool-call alternation invariant — inserting the leading
// block must never break strict user/assistant/tool ordering. We inject into a
// realistic tool-call conversation and assert the relative order of all client
// messages (and tool_call_id linkage) is unchanged; the block lands before the
// first instruction message only.
func TestToolCallAlternationPreserved(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[` +
		`{"role":"system","content":"sys"},` +
		`{"role":"user","content":"weather?"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"sunny"},` +
		`{"role":"assistant","content":"It is sunny."}` +
		`]}`)
	res, err := Build(body, []rules.Rule{mkRule("a", "A", "ba")}, []string{"a"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Decode roles in order, skipping the injected block.
	var req struct {
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
			ToolCalls  json.RawMessage `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(res.Body, &req); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, res.Body)
	}
	var roles []string
	var sawToolLink bool
	for _, m := range req.Messages {
		var cs string
		if len(m.Content) > 0 && m.Content[0] == '"' {
			_ = json.Unmarshal(m.Content, &cs)
		}
		if strings.HasPrefix(cs, begin+"\n") {
			continue // skip the injected block
		}
		roles = append(roles, m.Role)
		if m.Role == "tool" && m.ToolCallID == "call_1" {
			sawToolLink = true
		}
	}
	want := []string{"system", "user", "assistant", "tool", "assistant"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("client message order broken: want %v got %v", want, roles)
	}
	if !sawToolLink {
		t.Fatalf("tool_call_id linkage must be preserved")
	}
	// The block must lead (before the first instruction = system at index 0).
	out := parseOut(t, res.Body)
	if !strings.HasPrefix(out[0].Content, begin+"\n") {
		t.Fatalf("block must lead before the first instruction msg")
	}
}

// Bridge finding #4: client-truncation → no stacking. If the client truncated
// history and a prior block is GONE (the user resends without it), essaim must
// not double-inject — it always rebuilds to exactly one block. (And if a block
// IS present it is stripped first.) Either way: exactly one block, never stacked.
func TestClientTruncationNoStack(t *testing.T) {
	// Turn 1: inject into a fresh history.
	res1, _ := Build([]byte(`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"u1"}]}`),
		[]rules.Rule{mkRule("a", "A", "ba")}, []string{"a"})
	// Turn 2: client TRUNCATED — sends history WITHOUT the prior essaim block
	// (only the original system+user, plus a new user turn).
	truncated := []byte(`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"u1"},{"role":"user","content":"u2"}]}`)
	res2, err := Build(truncated, []rules.Rule{mkRule("a", "A", "ba")}, []string{"a"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if n := countBeginInInstruction(t, res2.Body); n != 1 {
		t.Fatalf("truncated history must still yield exactly 1 block, got %d", n)
	}
	_ = res1
}

// Capture snapshot (spec §4.1): the pre-injection snapshot CleanMessagesJSON must
// be essaim-FREE (no ESSAIM_BEGIN) even when the inbound history carries a prior
// block (echo). MatchedRuleIDs travel out-of-band (not in the messages text).
func TestCaptureSnapshotIsEssaimFree(t *testing.T) {
	// Turn 1 injects a block; turn 2 receives it echoed back.
	res1, _ := Build([]byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		[]rules.Rule{mkRule("a", "A", "ALWAYS_USE_TABS")}, []string{"a"})
	res2, _ := Build(res1.Body, []rules.Rule{mkRule("a", "A", "ALWAYS_USE_TABS")}, []string{"a"})

	snap := res2.Snapshot
	if strings.Contains(string(snap.CleanMessagesJSON), begin) {
		t.Fatalf("capture snapshot must be essaim-free, but contains the sentinel:\n%s", snap.CleanMessagesJSON)
	}
	if strings.Contains(string(snap.CleanMessagesJSON), "ALWAYS_USE_TABS") {
		t.Fatalf("capture snapshot must not contain the injected rule body")
	}
	// MatchedRuleIDs threaded out-of-band, not in any message text.
	if len(snap.MatchedRuleIDs) != 1 || snap.MatchedRuleIDs[0] != "a" {
		t.Fatalf("MatchedRuleIDs must equal the matched set: %v", snap.MatchedRuleIDs)
	}
	if strings.Contains(string(snap.CleanMessagesJSON), `"a"`) && strings.Contains(string(snap.CleanMessagesJSON), "rule_id") {
		t.Fatalf("rule IDs must not be embedded in messages")
	}
}

// getModel must read the model field for role sniffing regardless of field order.
func TestGetModelFieldOrder(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"model":"o3-mini","messages":[{"role":"user","content":"u"}]}`),
		[]byte(`{"messages":[{"role":"user","content":"u"}],"model":"o3-mini"}`),
	} {
		m, ok := getModel(body)
		if !ok || m != "o3-mini" {
			t.Fatalf("getModel failed for %s: %q ok=%v", body, m, ok)
		}
	}
}

// Build must fail-open (ErrSkip) when there is no messages array or it is empty.
func TestBuildSkipsWhenNoMessages(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"model":"gpt-4o"}`),
		[]byte(`{"messages":[]}`),
	} {
		_, err := Build(body, []rules.Rule{mkRule("a", "A", "ba")}, []string{"a"})
		if err != ErrSkip {
			t.Fatalf("want ErrSkip for %s, got %v", body, err)
		}
	}
}
