package inject

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"essaim/internal/rules"
)

func oneRule() []rules.Rule {
	return []rules.Rule{{ID: "g", Title: "Guard", Body: "always do X", Kind: "guardrail"}}
}

// Bug 2a: empty messages array + a match must NOT panic / must fail-open.
func TestBuildEmptyMessagesNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PANIC on empty messages array: %v", r)
		}
	}()
	res, err := Build([]byte(`{"model":"gpt-4o","messages":[]}`), oneRule(), []string{"g"})
	if err == nil && res.Snapshot.Injected {
		t.Fatalf("empty messages must not inject; got injected body %q", string(res.Body))
	}
}

// Bug 2b: a body whose ONLY element is a prior essaim block → stripped→clean empty.
func TestBuildOnlyEssaimBlockNoPanic(t *testing.T) {
	blk := `{"role":"system","content":"` + rules.ESSAIM_BEGIN + `\nx\n` + rules.ESSAIM_END + `"}`
	body := []byte(`{"model":"gpt-4o","messages":[` + blk + `]}`)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PANIC on only-essaim-block body: %v", r)
		}
	}()
	res, err := Build(body, oneRule(), []string{"g"})
	if err == nil && len(res.Body) > 0 && !json.Valid(res.Body) {
		t.Fatalf("only-essaim-block produced INVALID json: %q", string(res.Body))
	}
	_ = errors.Is
}

// Bug 1: whitespace between '[' and the first element must survive (verbatim tail).
func TestHeadInjectPreservesPostBracketWhitespace(t *testing.T) {
	body := []byte("{\"model\":\"gpt-4o\",\"messages\":[\n  {\"role\":\"user\",\"content\":\"hi\"}\n]}")
	res, err := Build(body, oneRule(), []string{"g"})
	if err != nil {
		t.Fatalf("valid body must inject, got %v", err)
	}
	if !json.Valid(res.Body) {
		t.Fatalf("output must be valid JSON: %q", string(res.Body))
	}
	// The original first element + its surrounding whitespace must appear verbatim.
	if !strings.Contains(string(res.Body), "\n  {\"role\":\"user\",\"content\":\"hi\"}\n]") {
		t.Fatalf("post-'[' whitespace / first element NOT preserved verbatim:\n%q", string(res.Body))
	}
}
