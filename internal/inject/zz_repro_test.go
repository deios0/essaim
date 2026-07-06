package inject

import (
	"encoding/json"
	"errors"
	"testing"

	"essaim/internal/rules"
)

func TestReproBodyWellFormedGap(t *testing.T) {
	m := []rules.Rule{{ID: "r1", Title: "T", Body: "x", Confidence: 0.9}}
	ids := []string{"r1"}
	// Brace-balanced but INVALID JSON: missing colon after "temperature".
	// The messages array is locally valid, so the structural scanner is happy and
	// essaim would splice into it — but the ENCLOSING object is malformed, so the
	// spliced output is ALSO invalid JSON. Spec §5.1 / A-5.5 bind "any ambiguity ⇒
	// forward the EXACT original bytes verbatim." (P1-3.)
	bad := `{"messages":[{"role":"user","content":"hi"}],"temperature"0.5}`
	if json.Valid([]byte(bad)) {
		t.Fatal("precondition: input should be INVALID json")
	}
	res, err := Build([]byte(bad), m, ids)
	t.Logf("err=%v injected=%v", err, res.Snapshot.Injected)
	// FIX: a malformed body must be SKIPPED (ErrSkip) so the caller forwards the
	// original bytes verbatim — never rewritten into different still-invalid JSON.
	if !errors.Is(err, ErrSkip) {
		t.Fatalf("malformed-but-balanced body must return ErrSkip (forward verbatim), got err=%v injected=%v out=%s", err, res.Snapshot.Injected, res.Body)
	}
	if res.Snapshot.Injected {
		t.Fatalf("a skipped body must NOT be injected")
	}
}

// P1-3: a VALID body whose splice would produce valid JSON is unaffected — the
// post-splice json.Valid guard only trips on genuinely malformed envelopes.
func TestBodyValidStillInjects(t *testing.T) {
	m := []rules.Rule{{ID: "r1", Title: "T", Body: "x", Confidence: 0.9}}
	good := `{"messages":[{"role":"user","content":"hi"}],"temperature":0.5}`
	if !json.Valid([]byte(good)) {
		t.Fatal("precondition: input should be VALID json")
	}
	res, err := Build([]byte(good), m, []string{"r1"})
	if err != nil {
		t.Fatalf("valid body must inject, got err=%v", err)
	}
	if !res.Snapshot.Injected {
		t.Fatalf("valid body must be injected")
	}
	if !json.Valid(res.Body) {
		t.Fatalf("injected output of a valid body must be valid JSON:\n%s", res.Body)
	}
}

func TestReproModelObjectLevel(t *testing.T) {
	// A user content string contains `"model":"gpt-5"`; the REAL top-level model is gpt-4o.
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"example: \"model\":\"gpt-5\""}]}`
	got := Model([]byte(body))
	if got != "gpt-4o" {
		t.Errorf("A-2.1/F-V7: top-level model misread as %q (want gpt-4o)", got)
	}
	t.Logf("Model()=%q", got)
}
