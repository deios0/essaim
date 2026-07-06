package inject

import (
	"encoding/json"
	"strings"
	"testing"

	"essaim/internal/rules"
)

// helpers ---------------------------------------------------------------------

type msgJSON struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// parseOut unmarshals a result body's messages array into role+string-content
// pairs. It fatals if content is non-string (callers that pass multimodal use
// rawMessages instead).
func parseOut(t *testing.T, body []byte) []struct{ Role, Content string } {
	t.Helper()
	var req struct {
		Messages []msgJSON `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("result body is not valid JSON: %v\nbody=%s", err, body)
	}
	out := make([]struct{ Role, Content string }, 0, len(req.Messages))
	for _, m := range req.Messages {
		var s string
		// content may be a string or array; only unmarshal when it's a string.
		if len(m.Content) > 0 && m.Content[0] == '"' {
			_ = json.Unmarshal(m.Content, &s)
		} else {
			s = string(m.Content) // raw (e.g. multimodal array) for inspection
		}
		out = append(out, struct{ Role, Content string }{m.Role, s})
	}
	return out
}

func mkRule(id, title, body string) rules.Rule {
	return rules.Rule{ID: id, Title: title, Body: body, Confidence: 0.9, Weight: 0.9, Status: "live"}
}

func countBeginInInstruction(t *testing.T, body []byte) int {
	t.Helper()
	var req struct {
		Messages []msgJSON `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	n := 0
	for _, m := range req.Messages {
		if !instructionRoles[m.Role] {
			continue
		}
		var s string
		if len(m.Content) > 0 && m.Content[0] == '"' {
			_ = json.Unmarshal(m.Content, &s)
		}
		if strings.HasPrefix(s, begin+"\n") && strings.HasSuffix(s, "\n"+end) {
			n++
		}
	}
	return n
}

// 8.A — Placement & role -------------------------------------------------------

// Test 1: inject_into_zero_instruction_msgs.
func TestInjectIntoZeroInstructionMsgs(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	r := mkRule("r1", "Use Postgres", "Always use PostgreSQL, never MySQL.")
	res, err := Build(body, []rules.Rule{r}, []string{"r1"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	out := parseOut(t, res.Body)
	if len(out) != 2 {
		t.Fatalf("want 2 messages, got %d: %v", len(out), out)
	}
	if out[0].Role != "system" {
		t.Fatalf("block role: want system, got %q", out[0].Role)
	}
	if !strings.HasPrefix(out[0].Content, begin+"\n") || !strings.HasSuffix(out[0].Content, "\n"+end) {
		t.Fatalf("block not fenced correctly: %q", out[0].Content)
	}
	if !strings.Contains(out[0].Content, "Use Postgres") {
		t.Fatalf("rule body missing from block: %q", out[0].Content)
	}
	if out[1].Role != "user" || out[1].Content != "hi" {
		t.Fatalf("user msg not preserved: %+v", out[1])
	}
}

// Test 2: inject_preserves_single_client_system_byte_identical (no merge).
func TestInjectPreservesSingleClientSystemByteIdentical(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"BIG SYSTEM PROMPT"},{"role":"user","content":"x"}]}`)
	res, err := Build(body, []rules.Rule{mkRule("r1", "T", "B")}, []string{"r1"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	out := parseOut(t, res.Body)
	if len(out) != 3 {
		t.Fatalf("want 3, got %d", len(out))
	}
	if out[0].Role != "system" || !strings.HasPrefix(out[0].Content, begin+"\n") {
		t.Fatalf("essaim block must lead: %+v", out[0])
	}
	if out[1].Role != "system" || out[1].Content != "BIG SYSTEM PROMPT" {
		t.Fatalf("client system must be byte-identical (no merge): %+v", out[1])
	}
}

// Test 3: inject_with_N_instruction_msgs_preserves_all_in_order.
func TestInjectWithNInstructionMsgsPreservesAllInOrder(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"s1"},{"role":"system","content":"s2"},{"role":"system","content":"s3"},{"role":"user","content":"u"}]}`)
	res, err := Build(body, []rules.Rule{mkRule("r1", "T", "B")}, []string{"r1"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	out := parseOut(t, res.Body)
	want := []string{"s1", "s2", "s3", "u"}
	if len(out) != 5 {
		t.Fatalf("want 5, got %d: %v", len(out), out)
	}
	if !strings.HasPrefix(out[0].Content, begin+"\n") {
		t.Fatalf("block must lead")
	}
	for i, w := range want {
		if out[i+1].Content != w {
			t.Fatalf("order/byte mismatch at %d: want %q got %q", i, w, out[i+1].Content)
		}
	}
}

// Test 4: block_byte_stable_across_turns_same_matchset.
func TestBlockByteStableAcrossTurnsSameMatchset(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	set := []rules.Rule{mkRule("a", "A", "body a"), mkRule("b", "B", "body b")}
	res1, _ := Build(body, set, []string{"a", "b"})
	res2, _ := Build(body, set, []string{"a", "b"})
	b1 := parseOut(t, res1.Body)[0].Content
	b2 := parseOut(t, res2.Body)[0].Content
	if b1 != b2 {
		t.Fatalf("block must be byte-stable across turns:\n%q\n%q", b1, b2)
	}
}

// Test 5: rules_sorted_deterministically (shuffled input, distinct weights).
func TestRulesSortedDeterministically(t *testing.T) {
	r1 := rules.Rule{ID: "z", Title: "Z", Body: "zb", Weight: 0.9}
	r2 := rules.Rule{ID: "a", Title: "A", Body: "ab", Weight: 0.3}
	r3 := rules.Rule{ID: "m", Title: "M", Body: "mb", Weight: 0.6}
	res1, _ := Build([]byte(`{"messages":[{"role":"user","content":"x"}]}`), []rules.Rule{r1, r2, r3}, nil)
	res2, _ := Build([]byte(`{"messages":[{"role":"user","content":"x"}]}`), []rules.Rule{r3, r1, r2}, nil)
	c1 := parseOut(t, res1.Body)[0].Content
	c2 := parseOut(t, res2.Body)[0].Content
	if c1 != c2 {
		t.Fatalf("render must be order-independent:\n%q\n%q", c1, c2)
	}
	// Highest weight (Z) first, lowest (A) last.
	zi := strings.Index(c1, "Z:")
	mi := strings.Index(c1, "M:")
	ai := strings.Index(c1, "A:")
	if !(zi < mi && mi < ai) {
		t.Fatalf("weight-desc order wrong: Z=%d M=%d A=%d in %q", zi, mi, ai, c1)
	}
}

// 8.B — Developer-role & instruction-set placement -----------------------------

// Test 8r: developer_role_block_inherits_developer.
func TestDeveloperRoleBlockInheritsDeveloper(t *testing.T) {
	body := []byte(`{"model":"o3-mini","messages":[{"role":"developer","content":"X"},{"role":"user","content":"u"}]}`)
	res, err := Build(body, []rules.Rule{mkRule("r1", "T", "B")}, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	out := parseOut(t, res.Body)
	if out[0].Role != "developer" {
		t.Fatalf("block role must inherit developer, got %q", out[0].Role)
	}
	if out[1].Role != "developer" || out[1].Content != "X" {
		t.Fatalf("client developer msg must be byte-identical: %+v", out[1])
	}
}

// Bridge finding #2: developer-role chosen by MODEL sniff when no instruction
// message is present (o-series / GPT-5.x).
func TestModelSniffPicksDeveloperWithoutInstructionMsg(t *testing.T) {
	for _, model := range []string{"o1", "o3-mini", "o4-preview", "gpt-5.4", "openai/o3"} {
		body := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"u"}]}`)
		res, err := Build(body, []rules.Rule{mkRule("r1", "T", "B")}, nil)
		if err != nil {
			t.Fatalf("Build(%s): %v", model, err)
		}
		out := parseOut(t, res.Body)
		if out[0].Role != "developer" {
			t.Fatalf("model %s: want developer block, got %q", model, out[0].Role)
		}
	}
	// A non-reasoning model defaults to system.
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"u"}]}`)
	res, _ := Build(body, []rules.Rule{mkRule("r1", "T", "B")}, nil)
	if parseOut(t, res.Body)[0].Role != "system" {
		t.Fatalf("gpt-4o must default to system role")
	}
}

// Test 9r: developer_present_deeper_defaults_to_developer.
func TestDeveloperPresentDeeperDefaultsToDeveloper(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"u1"},{"role":"developer","content":"d"}]}`)
	res, _ := Build(body, []rules.Rule{mkRule("r1", "T", "B")}, nil)
	out := parseOut(t, res.Body)
	// developer message exists deeper; the block is placed at array index 0
	// (v1.1 A-5.3.1) and inherits the `developer` role (the first instruction msg).
	var blockRole string
	for _, m := range out {
		if strings.HasPrefix(m.Content, begin+"\n") {
			blockRole = m.Role
		}
	}
	if blockRole != "developer" {
		t.Fatalf("block must inherit developer, got %q", blockRole)
	}
}

// Test 10r: mixed_instruction_roles_uses_first.
func TestMixedInstructionRolesUsesFirst(t *testing.T) {
	body := []byte(`{"messages":[{"role":"developer","content":"d"},{"role":"system","content":"s"},{"role":"user","content":"u"}]}`)
	res, _ := Build(body, []rules.Rule{mkRule("r1", "T", "B")}, nil)
	out := parseOut(t, res.Body)
	if out[0].Role != "developer" || !strings.HasPrefix(out[0].Content, begin+"\n") {
		t.Fatalf("block role must be first instruction role (developer), inserted before it: %+v", out[0])
	}
	if out[1].Content != "d" {
		t.Fatalf("developer msg must follow block: %+v", out[1])
	}
}

// Test 11r (SUPERSEDED → index-0 per B1 v1.1 A-5.3.1 / supersession note, F-V6):
// leading_user_preamble_block_at_index0. The v1.0 reading placed the block at
// index 1 (behind a leading `user` bootstrap, before the first instruction); v1.1
// pins the block to ARRAY INDEX 0 UNCONDITIONALLY. The role is still chosen per
// A-2 (here: inherit the `system` of the later instruction msg), only the
// position is index 0.
func TestLeadingUserPreambleBlockAtIndex0(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"bootstrap"},{"role":"system","content":"s"},{"role":"user","content":"u"}]}`)
	res, _ := Build(body, []rules.Rule{mkRule("r1", "T", "B")}, nil)
	out := parseOut(t, res.Body)
	// Expected (v1.1 A-5.3.1): [essaim_block, user bootstrap, system, user]
	if !strings.HasPrefix(out[0].Content, begin+"\n") || !strings.HasSuffix(out[0].Content, "\n"+end) {
		t.Fatalf("block must be at index 0 (A-5.3.1), got: %+v", out[0])
	}
	// Role inherits the client's leading instruction role (the system msg).
	if out[0].Role != "system" {
		t.Fatalf("block role must inherit system, got %q", out[0].Role)
	}
	if out[1].Content != "bootstrap" {
		t.Fatalf("user bootstrap must follow the block at index 1: %+v", out[1])
	}
	if out[2].Content != "s" {
		t.Fatalf("system must keep its position after the bootstrap: %+v", out[2])
	}
	if out[3].Content != "u" {
		t.Fatalf("user must remain last: %+v", out[3])
	}
}

// V1.1-31 (A-3): inject_does_not_split_assistant_tool_pair — a pathological array
// whose index 0 is itself an assistant{tool_calls} (no leading instruction msg).
// Index-0 insertion must put the block at the array head, leaving the
// assistant→tool pair adjacent right after it (alternation intact). This is the
// case the v1.0 "before-first-instruction" rule could NOT handle (no instruction
// exists), which is exactly why A-5.3.1 pins to index 0.
func TestInjectIndex0DoesNotSplitAssistantToolPair(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"call_9","type":"function","function":{"name":"f","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"call_9","content":"r"},` +
		`{"role":"user","content":"thanks"}` +
		`]}`)
	res, err := Build(body, []rules.Rule{mkRule("a", "A", "ba")}, []string{"a"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var req struct {
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
			ToolCalls  json.RawMessage `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(res.Body, &req); err != nil {
		t.Fatalf("spliced body must be valid JSON: %v\n%s", err, res.Body)
	}
	// Index 0 must be the essaim block; the assistant{tool_calls} and its tool reply
	// must be adjacent and in order right after it.
	var cs0 string
	if len(req.Messages[0].Content) > 0 && req.Messages[0].Content[0] == '"' {
		_ = json.Unmarshal(req.Messages[0].Content, &cs0)
	}
	if !strings.HasPrefix(cs0, begin+"\n") {
		t.Fatalf("block must be at array index 0, got role=%q", req.Messages[0].Role)
	}
	if req.Messages[1].Role != "assistant" || len(req.Messages[1].ToolCalls) == 0 {
		t.Fatalf("assistant{tool_calls} must immediately follow the block: %+v", req.Messages[1])
	}
	if req.Messages[2].Role != "tool" || req.Messages[2].ToolCallID != "call_9" {
		t.Fatalf("tool reply must immediately follow its assistant parent: %+v", req.Messages[2])
	}
}
