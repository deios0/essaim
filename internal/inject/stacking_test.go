package inject

import (
	"strings"
	"testing"

	"oikos/internal/rules"
)

// 8.C — Multi-turn / stacking --------------------------------------------------

// Test 8: strip_prior_block_no_stacking — feed turn-1 output back + new match ⇒
// exactly ONE oikos block.
func TestStripPriorBlockNoStacking(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	res1, _ := Build(body, []rules.Rule{mkRule("a", "A", "ba")}, []string{"a"})
	res2, err := Build(res1.Body, []rules.Rule{mkRule("a", "A", "ba")}, []string{"a"})
	if err != nil {
		t.Fatalf("turn2 Build: %v", err)
	}
	if n := countBeginInInstruction(t, res2.Body); n != 1 {
		t.Fatalf("want exactly 1 oikos block after re-inject, got %d\nbody=%s", n, res2.Body)
	}
}

// Bridge finding #1 + Test 11b: strip_changed_payload_no_stacking — turn 1
// injects rule {A}, turn 2 matches {B} (different payload, SAME wrapper) ⇒
// exactly ONE block (B), not A+B. This is the SENTINEL-ONLY strip: recognition
// keys on the wrapper, never a payload hash, so a prior block whose rules changed
// is still stripped.
func TestStripChangedPayloadNoStacking(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"keep"},{"role":"user","content":"hi"}]}`)
	res1, _ := Build(body, []rules.Rule{mkRule("a", "RuleA", "ALWAYS_A")}, []string{"a"})
	res2, _ := Build(res1.Body, []rules.Rule{mkRule("b", "RuleB", "ALWAYS_B")}, []string{"b"})
	if n := countBeginInInstruction(t, res2.Body); n != 1 {
		t.Fatalf("changed-payload re-inject must yield exactly 1 block, got %d\nbody=%s", n, res2.Body)
	}
	out := parseOut(t, res2.Body)
	var block string
	for _, m := range out {
		if strings.HasPrefix(m.Content, begin+"\n") {
			block = m.Content
		}
	}
	if strings.Contains(block, "ALWAYS_A") {
		t.Fatalf("turn-1 rule A must NOT survive; block=%q", block)
	}
	if !strings.Contains(block, "ALWAYS_B") {
		t.Fatalf("turn-2 rule B must be present; block=%q", block)
	}
}

// Test 9: idempotent_repeat_5_turns — apply Inject 5×, feeding each output back
// (plus a growing user tail) ⇒ every output has exactly one oikos block; the
// client tail is never mutated.
func TestIdempotentRepeat5Turns(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"u0"}]}`)
	set := []rules.Rule{mkRule("a", "A", "ba")}
	cur := body
	for turn := 1; turn <= 5; turn++ {
		res, err := Build(cur, set, []string{"a"})
		if err != nil {
			t.Fatalf("turn %d: %v", turn, err)
		}
		if n := countBeginInInstruction(t, res.Body); n != 1 {
			t.Fatalf("turn %d: want 1 block, got %d", turn, n)
		}
		out := parseOut(t, res.Body)
		// The client system "S" must be byte-identical every turn.
		foundS := false
		for _, m := range out {
			if m.Role == "system" && m.Content == "S" {
				foundS = true
			}
		}
		if !foundS {
			t.Fatalf("turn %d: client system mutated/lost", turn)
		}
		cur = res.Body
	}
}

// Test 10: empty_match_still_strips — stale block + user, matched==[] ⇒ stale
// removed, nothing injected.
func TestEmptyMatchStillStrips(t *testing.T) {
	// Turn 1 injects a block.
	res1, _ := Build([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), []rules.Rule{mkRule("a", "A", "ba")}, []string{"a"})
	// Turn 2 matches nothing.
	res2, err := Build(res1.Body, nil, nil)
	if err != nil {
		t.Fatalf("empty-match Build: %v", err)
	}
	if n := countBeginInInstruction(t, res2.Body); n != 0 {
		t.Fatalf("empty match must strip the stale block, got %d blocks\nbody=%s", n, res2.Body)
	}
	out := parseOut(t, res2.Body)
	if len(out) != 1 || out[0].Content != "hi" {
		t.Fatalf("only the user msg should remain: %v", out)
	}
}

// Test 11: strip_collapses_multiple_stale_blocks.
func TestStripCollapsesMultipleStaleBlocks(t *testing.T) {
	blk := begin + "\nstale\n" + end
	// Three separate oikos blocks + user.
	body := []byte(`{"messages":[` +
		`{"role":"system","content":` + jsonStr(blk) + `},` +
		`{"role":"system","content":` + jsonStr(blk) + `},` +
		`{"role":"system","content":` + jsonStr(blk) + `},` +
		`{"role":"user","content":"u"}]}`)
	res, err := Build(body, []rules.Rule{mkRule("a", "A", "fresh")}, []string{"a"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if n := countBeginInInstruction(t, res.Body); n != 1 {
		t.Fatalf("3 stale blocks must collapse to 1 fresh, got %d", n)
	}
}

// Test 12: no_orphan_empty_instruction_across_turns — start [{user}], inject,
// feed back, inject ⇒ length stable, exactly one block, NO orphan synthetic
// instruction msg (R-2 clean whole-element delete).
func TestNoOrphanEmptyInstructionAcrossTurns(t *testing.T) {
	res1, _ := Build([]byte(`{"messages":[{"role":"user","content":"u"}]}`), []rules.Rule{mkRule("a", "A", "ba")}, []string{"a"})
	res2, _ := Build(res1.Body, []rules.Rule{mkRule("a", "A", "ba")}, []string{"a"})
	out := parseOut(t, res2.Body)
	if len(out) != 2 {
		t.Fatalf("length must be stable at 2 (block+user), got %d: %v", len(out), out)
	}
	for _, m := range out {
		if instructionRoles[m.Role] && m.Content == "" {
			t.Fatalf("orphan empty instruction message present: %+v", m)
		}
	}
}

// 8.D — Delimiter safety -------------------------------------------------------

// Test 13: marker_not_stripped_from_client_user_text (factor 1 fails).
func TestMarkerNotStrippedFromClientUserText(t *testing.T) {
	content := "look: " + begin + " and " + end + " in my prose"
	body := []byte(`{"messages":[{"role":"user","content":` + jsonStr(content) + `}]}`)
	res, _ := Build(body, []rules.Rule{mkRule("a", "A", "ba")}, []string{"a"})
	out := parseOut(t, res.Body)
	found := false
	for _, m := range out {
		if m.Role == "user" && m.Content == content {
			found = true
		}
	}
	if !found {
		t.Fatalf("user text containing the marker must be preserved verbatim: %v", out)
	}
}

// Test 14: client_system_mentioning_marker_midtext_preserved (factor 2 fails:
// not byte-0).
func TestClientSystemMentioningMarkerMidtextPreserved(t *testing.T) {
	content := "note: " + begin + " is reserved"
	body := []byte(`{"messages":[{"role":"system","content":` + jsonStr(content) + `},{"role":"user","content":"u"}]}`)
	res, _ := Build(body, []rules.Rule{mkRule("a", "A", "ba")}, []string{"a"})
	out := parseOut(t, res.Body)
	found := false
	for _, m := range out {
		if m.Role == "system" && m.Content == content {
			found = true
		}
	}
	if !found {
		t.Fatalf("system msg merely MENTIONING the marker mid-text must be preserved: %v", out)
	}
}

// Test 15: multimodal_instruction_content_preserved — array-of-parts content
// that doesn't match the wrapper ⇒ preserved; no panic on non-string content.
func TestMultimodalInstructionContentPreserved(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"http://x"}}]},{"role":"user","content":"u"}]}`)
	res, err := Build(body, []rules.Rule{mkRule("a", "A", "ba")}, []string{"a"})
	if err != nil {
		t.Fatalf("multimodal Build must not error: %v", err)
	}
	// The multimodal system message must survive byte-identical (raw array).
	if !strings.Contains(string(res.Body), `"type":"image_url"`) {
		t.Fatalf("multimodal content must be preserved: %s", res.Body)
	}
	if n := countBeginInInstruction(t, res.Body); n != 1 {
		t.Fatalf("exactly one fresh block expected, got %d", n)
	}
}

// Test 15b: user_pasted_full_block_not_corrupted — a USER message whose body is
// an ENTIRE OIKOS_BEGIN…OIKOS_END block (dogfood paste) ⇒ preserved
// byte-identical (factor 1 fails: not an instruction role; never substring scan).
func TestUserPastedFullBlockNotCorrupted(t *testing.T) {
	pasted := begin + "\n- [H] Some rule: body\n" + end
	body := []byte(`{"messages":[{"role":"user","content":` + jsonStr(pasted) + `}]}`)
	res, _ := Build(body, []rules.Rule{mkRule("a", "A", "ba")}, []string{"a"})
	out := parseOut(t, res.Body)
	found := false
	for _, m := range out {
		if m.Role == "user" && m.Content == pasted {
			found = true
		}
	}
	if !found {
		t.Fatalf("user-pasted full block must be preserved byte-identical: %v", out)
	}
}

// Bridge finding #6: partial/truncated sentinel — a malformed/partial sentinel
// must be handled safely (preserved, never stripped, never panic).
func TestPartialSentinelSafe(t *testing.T) {
	cases := []string{
		begin,                           // begin only, no newline, no end
		begin + "\nbody",                // begin + body, no end
		"body\n" + end,                  // end only, no begin
		begin[:len(begin)-3],            // truncated begin
		begin + "\n" + end[:len(end)-2], // truncated end
		begin + "\n",                    // begin + newline, no body, no end
	}
	for i, c := range cases {
		body := []byte(`{"messages":[{"role":"system","content":` + jsonStr(c) + `},{"role":"user","content":"u"}]}`)
		res, err := Build(body, []rules.Rule{mkRule("a", "A", "fresh")}, []string{"a"})
		if err != nil {
			t.Fatalf("case %d: Build errored on partial sentinel: %v", i, err)
		}
		out := parseOut(t, res.Body)
		// The partial-sentinel system message must be preserved (not recognized
		// as a complete oikos block, so not stripped).
		preserved := false
		for _, m := range out {
			if m.Role == "system" && m.Content == c {
				preserved = true
			}
		}
		if !preserved {
			t.Fatalf("case %d: partial sentinel %q must be preserved, out=%v", i, c, out)
		}
		// And exactly one fresh, COMPLETE block is added.
		if n := countBeginInInstruction(t, res.Body); n != 1 {
			t.Fatalf("case %d: want 1 complete block, got %d", i, n)
		}
	}
}

// F-E (A-5.4 role-first strip): the recognizer reads ROLE FIRST and never
// strips a non-instruction element — even one whose content is byte-for-byte a
// COMPLETE oikos block (both sentinels). An assistant message echoing the exact
// block must survive the strip: the role gate (factor 1), evaluated first, is
// what protects it; the sentinel anchors alone would otherwise match. This pins
// the binding A-5.4 ordering (role before any content/sentinel check).
func TestRoleFirstStripPreservesAssistantBlock(t *testing.T) {
	exact := begin + "\n- [H] Some rule: body\n" + end // a complete, real block...
	body := []byte(`{"messages":[` +
		`{"role":"assistant","content":` + jsonStr(exact) + `},` + // ...but role=assistant
		`{"role":"user","content":"u"}` +
		`]}`)
	res, err := Build(body, []rules.Rule{mkRule("a", "A", "fresh")}, []string{"a"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	out := parseOut(t, res.Body)
	preserved := false
	for _, m := range out {
		if m.Role == "assistant" && m.Content == exact {
			preserved = true
		}
	}
	if !preserved {
		t.Fatalf("an assistant msg equal to a complete block must NOT be stripped (role-first): %v", out)
	}
	// Exactly one FRESH instruction-role block is injected (the assistant echo is
	// not counted as ours).
	if n := countBeginInInstruction(t, res.Body); n != 1 {
		t.Fatalf("want exactly 1 instruction-role block, got %d", n)
	}
}

// jsonStr returns a JSON-quoted string literal for s (test helper).
func jsonStr(s string) string {
	return string(encodeJSONString(s))
}
