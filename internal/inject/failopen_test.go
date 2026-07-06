package inject

import (
	"bytes"
	"testing"

	"essaim/internal/rules"
)

// 8.E — Cache-stability & fail-open hardening (M2 review P2/P3) ----------------

// P2 cache-stability (Finding 1): a TRUE no-op — no rule matched AND no prior
// essaim block to strip — must return the ORIGINAL body bytes UNCHANGED. A
// pretty-printed (indented) body must NOT be re-serialized (which would strip
// intra-array whitespace and bust upstream prompt-caching, defeating the
// splice-not-Marshal design).
func TestNoOpReturnsOriginalBytesUnchanged(t *testing.T) {
	body := []byte("{\n\t\"messages\": [\n\t\t{\n\t\t\t\"role\": \"user\",\n\t\t\t\"content\": \"hi\"\n\t\t}\n\t]\n}")
	res, err := Build(body, nil, nil) // empty match, no prior essaim block
	if err != nil {
		t.Fatalf("no-op Build must not error: %v", err)
	}
	if !bytes.Equal(res.Body, body) {
		t.Fatalf("true no-op must return the ORIGINAL bytes unchanged (cache-stable)\nIN : %q\nOUT: %q", body, res.Body)
	}
	if res.Snapshot.Injected {
		t.Fatalf("no-op must not report Injected")
	}
}

// P2 cache-stability: when a prior essaim block IS present and matched==[], the
// no-op fast-path must NOT fire — the stale block must still be stripped (the
// body legitimately changes). Guards the fast-path against masking the strip.
func TestNoOpFastPathDoesNotSkipStripOfPriorBlock(t *testing.T) {
	// Turn 1 injects a block.
	res1, _ := Build([]byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		[]rules.Rule{mkRule("a", "A", "ba")}, []string{"a"})
	if !bytes.Contains(res1.Body, []byte(begin)) {
		t.Fatalf("turn1 must contain a block")
	}
	// Turn 2 matches nothing → the prior block MUST be stripped, body changes.
	res2, err := Build(res1.Body, nil, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if n := countBeginInInstruction(t, res2.Body); n != 0 {
		t.Fatalf("prior block must be stripped on empty match, got %d blocks", n)
	}
	if bytes.Equal(res2.Body, res1.Body) {
		t.Fatalf("no-op fast-path must NOT short-circuit when a prior block was stripped")
	}
}

// P2 cache-stability: the INJECT path (a rule matches) must STILL rebuild the
// messages array correctly — the no-op fast-path must only affect the true
// no-op. Confirms the fast-path didn't break injection.
func TestInjectPathStillRebuildsAfterNoOpFastPath(t *testing.T) {
	body := []byte("{\n\t\"messages\": [\n\t\t{\n\t\t\t\"role\": \"user\",\n\t\t\t\"content\": \"hi\"\n\t\t}\n\t]\n}")
	res, err := Build(body, []rules.Rule{mkRule("a", "Use Postgres", "ba")}, []string{"a"})
	if err != nil {
		t.Fatalf("inject Build: %v", err)
	}
	if n := countBeginInInstruction(t, res.Body); n != 1 {
		t.Fatalf("inject path must produce exactly 1 block, got %d", n)
	}
	out := parseOut(t, res.Body)
	if out[0].Role != "system" || !contains(out[0].Content, "Use Postgres") {
		t.Fatalf("injected block missing/incorrect: %+v", out[0])
	}
}

// P2 fail-open (Finding 2): a non-object `messages` element (e.g. null) must
// make injection SKIP (ErrSkip) so the request is forwarded VERBATIM, instead
// of silently dropping the element and byte-changing the array.
func TestNonObjectMessagesElementSkipsVerbatim(t *testing.T) {
	body := []byte(`{"messages":[null,{"role":"user","content":"hi"}]}`)
	res, err := Build(body, []rules.Rule{mkRule("a", "A", "ba")}, []string{"a"})
	if err != ErrSkip {
		t.Fatalf("non-object messages element must yield ErrSkip (fail-open); got err=%v body=%s", err, res.Body)
	}
	// Build returns a zero Result on skip; the CALLER forwards origBody verbatim.
	if res.Body != nil {
		t.Fatalf("ErrSkip must return a zero Result.Body; got %s", res.Body)
	}
}

// P2 fail-open: other non-object scalars (number, string, true, false) at the
// element position also fail open verbatim.
func TestNonObjectMessagesScalarsSkip(t *testing.T) {
	for _, bad := range []string{"null", "42", `"x"`, "true", "false", "[1]"} {
		body := []byte(`{"messages":[` + bad + `,{"role":"user","content":"hi"}]}`)
		_, err := Build(body, []rules.Rule{mkRule("a", "A", "ba")}, []string{"a"})
		if err != ErrSkip {
			t.Fatalf("non-object element %q must yield ErrSkip; got %v", bad, err)
		}
	}
}

// P3 fail-open (Finding 3): unbalanced / truncated JSON must make the scanner
// signal failure so the caller SKIPs (ErrSkip → forward original bytes verbatim),
// rather than rebuild+inject onto degenerate input. No panic.
func TestUnbalancedJSONSkipsVerbatim(t *testing.T) {
	cases := map[string][]byte{
		"trailing garbage after array close": []byte(`{"messages":[{"role":"user","content":"x"}] GARBAGE`),
		"truncated (missing array close)":    []byte(`{"messages":[{"role":"user","content":"x"}`),
		"truncated mid-object":               []byte(`{"messages":[{"role":"user","content":"x"`),
		"truncated empty array open":         []byte(`{"messages":[`),
	}
	for name, body := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s: Build panicked on unbalanced input: %v", name, r)
				}
			}()
			res, err := Build(body, []rules.Rule{mkRule("a", "A", "ba")}, []string{"a"})
			if err != ErrSkip {
				t.Fatalf("%s: unbalanced input must yield ErrSkip (verbatim forward); got err=%v body=%s", name, err, res.Body)
			}
		}()
	}
}

// contains is a tiny local helper (avoids importing strings here for one call).
func contains(s, sub string) bool { return bytes.Contains([]byte(s), []byte(sub)) }
