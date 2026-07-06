package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"essaim/internal/rules"
)

// raceEnabledServer mirrors the inject package's build-tag flag: under -race the
// detector inflates timings ~10x, so the wall-clock budget is only enforced in a
// normal build. Correctness is asserted always.
//
// P0-1 regression: the WHOLE request-side transform (the server's buildOnce:
// LastUserMessage parse + Build parse + splice) must inject a ~5MB resent
// history UNDER the 15ms hot-path deadline, AND do so byte-exactly (every
// original message survives verbatim, exactly one essaim block prepended). Before
// the fix the body was parsed twice and copied twice (~21.5ms / ~8.5x body in
// allocs at 5MB) → the transform overran the 15ms deadline and silently failed
// open (zero injection) exactly in long agent sessions.
func TestP01LargeBodyInjectsUnderDeadline(t *testing.T) {
	// Build a realistic ~5MB resent chat history: a leading system message, a
	// huge prior-turn user message, an assistant reply, and a final user query
	// that matches the rule.
	huge := strings.Repeat("the quick brown fox jumped over the lazy dog. ", 110_000) // ~5MB
	jsonStr := func(s string) string {
		b, _ := json.Marshal(s)
		return string(b)
	}
	body := []byte(`{"model":"gpt-4o","messages":[` +
		`{"role":"system","content":"You are helpful."},` +
		`{"role":"user","content":` + jsonStr(huge) + `},` +
		`{"role":"assistant","content":"Understood."},` +
		`{"role":"user","content":"which database should I use?"}` +
		`]}`)

	dir := vaultWith(t, map[string]string{
		"pg.md": "---\nid: pg\ntitle: Use Postgres\nstatus: active\nconfidence: 0.9\nweight: 1\n---\nAlways use PostgreSQL for relational data; it is the database of choice.",
	})
	store, err := rules.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	cfg := rules.GuardConfig{EagerBytes: 4096, MatchFloor: 0.0}
	in := newInjectorWithStore(store, cfg)
	ix := in.store.Index()
	if ix.Len() == 0 {
		t.Fatalf("precondition: the rule must be indexed")
	}

	// Drive the real transform path (the double-parse + splice) once, timed.
	start := time.Now()
	out, _, berr := in.buildOnce(context.Background(), body, ix)
	elapsed := time.Since(start)
	if berr != nil {
		t.Fatalf("buildOnce returned an unexpected error (must inject): %v", berr)
	}

	if !raceEnabledServer && elapsed > 15*time.Millisecond {
		t.Fatalf("P0-1: 5MB transform took %v, must be < 15ms (single-parse, single-copy splice)", elapsed)
	}
	t.Logf("P0-1: 5MB buildOnce transform = %v", elapsed)

	// Byte-exact correctness: every original message survives verbatim and
	// exactly one essaim block is prepended.
	if !strings.Contains(string(out), huge) {
		t.Fatalf("P0-1: the 5MB user content must survive byte-identical")
	}
	if got := strings.Count(string(out), rules.ESSAIM_BEGIN); got != 1 {
		t.Fatalf("P0-1: want exactly 1 essaim block, got %d", got)
	}
	// The output must remain valid JSON and preserve the original messages.
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("P0-1: spliced 5MB body must be valid JSON: %v", err)
	}
	if req.Model != "gpt-4o" {
		t.Fatalf("P0-1: model field must be preserved, got %q", req.Model)
	}
	// messages = [essaim-block, system, user(huge), assistant, user] = 5.
	if len(req.Messages) != 5 {
		t.Fatalf("P0-1: want 5 messages (block + 4 originals), got %d", len(req.Messages))
	}
	// The block leads; the four originals follow in order, byte-identical.
	wantRoles := []string{"system", "system", "user", "assistant", "user"}
	for i, m := range req.Messages {
		if m.Role != wantRoles[i] {
			t.Fatalf("P0-1: message[%d] role = %q, want %q", i, m.Role, wantRoles[i])
		}
	}
}
