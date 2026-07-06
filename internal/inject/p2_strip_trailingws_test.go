package inject

import (
	"strings"
	"testing"

	"oikos/internal/rules"
)

// P2-3: the prior-injected-block recognizer must strip a prior oikos block even
// when a tool/IDE round-trip has appended cosmetic trailing whitespace/newlines
// (or prepended a leading newline) so the block differs from WrapBlock's exact
// output by only surrounding whitespace. Without normalization the block ends
// with "END\n" (not "END"), the HasSuffix("\n"+end) gate fails, the stale block
// is NOT stripped, and the fresh injection STACKS on top of it.
func TestStripPriorBlockTrailingNewline(t *testing.T) {
	inner := begin + "\n- [H] A: ba\n" + end
	cases := map[string]string{
		"trailing_newline":    inner + "\n",
		"trailing_2_newlines": inner + "\n\n",
		"trailing_spaces":     inner + "  \t",
		"leading_newline":     "\n" + inner,
		"leading_trailing":    "\n" + inner + "\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			body := []byte(`{"messages":[` +
				`{"role":"system","content":` + jsonStr(content) + `},` +
				`{"role":"user","content":"hi"}]}`)
			res, err := Build(body, []rules.Rule{mkRule("a", "A", "fresh")}, []string{"a"})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if n := countBeginInInstruction(t, res.Body); n != 1 {
				t.Fatalf("prior block with cosmetic whitespace must be stripped → exactly 1 block, got %d\nbody=%s", n, res.Body)
			}
			// The stale payload must be gone, the fresh one present.
			out := parseOut(t, res.Body)
			var block string
			for _, m := range out {
				if instructionRoles[m.Role] && strings.Contains(m.Content, begin) {
					block = m.Content
				}
			}
			if !strings.Contains(block, "fresh") {
				t.Fatalf("fresh rule body must be present; block=%q", block)
			}
		})
	}
}

// P2-3 negative: normalization must NOT widen recognition to non-oikos content.
// A partial/truncated sentinel, an inline-marker prose message, and a
// non-instruction (user) role echoing a complete block must STILL be preserved
// (never stripped), exactly as before.
func TestStripNormalizationDoesNotOverMatch(t *testing.T) {
	inner := begin + "\n- [H] A: ba\n" + end
	type tc struct {
		role, content string
	}
	cases := []tc{
		{"system", begin + "\nbody no end trailing\n"}, // no END → not a block
		{"system", "prefix junk " + inner},             // real content before BEGIN → not byte-0
		{"system", inner + " visible tail text"},       // non-whitespace after END → not a block
		{"user", inner + "\n"},                         // user role → never stripped (role-first)
		{"assistant", inner},                           // assistant echo → never stripped
	}
	for i, c := range cases {
		body := []byte(`{"messages":[` +
			`{"role":` + jsonStr(c.role) + `,"content":` + jsonStr(c.content) + `},` +
			`{"role":"user","content":"u"}]}`)
		res, err := Build(body, []rules.Rule{mkRule("a", "A", "fresh")}, []string{"a"})
		if err != nil {
			t.Fatalf("case %d: Build: %v", i, err)
		}
		out := parseOut(t, res.Body)
		preserved := false
		for _, m := range out {
			if m.Role == c.role && m.Content == c.content {
				preserved = true
			}
		}
		if !preserved {
			t.Fatalf("case %d (%s): content must be preserved verbatim, not stripped\ncontent=%q\nout=%v", i, c.role, c.content, out)
		}
	}
}
