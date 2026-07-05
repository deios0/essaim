package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- loader -------------------------------------------------------------------

func TestParseRuleFrontmatter(t *testing.T) {
	raw := []byte("---\nid: use-pg\ntitle: Use Postgres\nkind: guardrail\nweight: 0.9\nconfidence: 0.95\nstatus: live\n---\nAlways use PostgreSQL, never MySQL.\n")
	r, err := parseRule("use-pg.md", raw)
	if err != nil {
		t.Fatalf("parseRule: %v", err)
	}
	if r.ID != "use-pg" || r.Title != "Use Postgres" || r.Kind != "guardrail" {
		t.Fatalf("frontmatter not parsed: %+v", r)
	}
	if r.Weight != 0.9 || r.Confidence != 0.95 || r.Status != "live" {
		t.Fatalf("numeric/status fields not parsed: %+v", r)
	}
	if r.Body != "Always use PostgreSQL, never MySQL." {
		t.Fatalf("body wrong: %q", r.Body)
	}
}

func TestParseRuleNoFrontmatterIsBodyOnly(t *testing.T) {
	r, err := parseRule("note.md", []byte("just a note\nwith two lines"))
	if err != nil {
		t.Fatalf("parseRule: %v", err)
	}
	if r.ID != "note" {
		t.Fatalf("id should default to stem: %q", r.ID)
	}
	if r.Body != "just a note\nwith two lines" {
		t.Fatalf("body wrong: %q", r.Body)
	}
}

func TestLoadVaultSkipsBadFilesAndMissingDir(t *testing.T) {
	// Missing dir → no rules, no error.
	rs, err := LoadVault(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil || rs != nil {
		t.Fatalf("missing dir must be (nil,nil); got %v %v", rs, err)
	}
	// Empty env-style skip.
	if rs, _ := LoadVault(""); rs != nil {
		t.Fatalf("empty dir must be nil")
	}

	dir := t.TempDir()
	writeFile(t, dir, "good.md", "---\nid: good\ntitle: Good\n---\nbody")
	writeFile(t, dir, "bad.md", "---\nid: bad\ntitle: [unterminated\n") // unterminated frontmatter
	writeFile(t, dir, "notes.txt", "ignored, not markdown")             // non-md
	writeFile(t, filepath.Join(dir, "sub"), "nested.md", "---\nid: nested\ntitle: Nested\n---\nx")
	rs, err = LoadVault(dir)
	if err != nil {
		t.Fatalf("LoadVault: %v", err)
	}
	ids := map[string]bool{}
	for _, r := range rs {
		ids[r.ID] = true
	}
	if !ids["good"] || !ids["nested"] {
		t.Fatalf("good+nested must load; got %v", ids)
	}
	if ids["bad"] {
		t.Fatalf("malformed file must be skipped")
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	_ = os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// --- index / match ------------------------------------------------------------

func TestBuildIndexAndMatch(t *testing.T) {
	rs := []Rule{
		{ID: "pg", Title: "Use Postgres", Body: "Always use PostgreSQL database, never MySQL.", Weight: 0.9, Confidence: 0.9},
		{ID: "tabs", Title: "Use tabs", Body: "Indent with tabs not spaces.", Weight: 0.8, Confidence: 0.8},
		{ID: "py", Title: "Python style", Body: "Use black formatting for Python code.", Weight: 0.7, Confidence: 0.7},
	}
	ix := BuildIndex(rs)
	if ix.Len() != 3 {
		t.Fatalf("index len: %d", ix.Len())
	}
	got := ix.Match("what database should I use, postgres or mysql?")
	if len(got) == 0 {
		t.Fatalf("expected matches for a database query")
	}
	if got[0].rule.ID != "pg" {
		t.Fatalf("postgres rule should rank first, got %q", got[0].rule.ID)
	}
	if got[0].score <= 0 || got[0].score > 1.0001 {
		t.Fatalf("score must be normalized to (0,1]; got %v", got[0].score)
	}
}

func TestMatchEmptyIndexAndEmptyQuery(t *testing.T) {
	if got := BuildIndex(nil).Match("anything"); got != nil {
		t.Fatalf("empty index must return nil")
	}
	ix := BuildIndex([]Rule{{ID: "a", Title: "A", Body: "b"}})
	if got := ix.Match("   "); got != nil {
		t.Fatalf("empty query must return nil")
	}
}

// --- confBucket hysteresis (Test 7) ------------------------------------------

func TestConfBucketHysteresisNoFlicker(t *testing.T) {
	// A weight drifting by epsilon WITHIN a 0.1 band must not flip buckets.
	r1 := Rule{Confidence: 0.851}
	r2 := Rule{Confidence: 0.849}
	if confBucket(r1) != confBucket(r2) {
		t.Fatalf("epsilon drift flipped bucket: %q vs %q", confBucket(r1), confBucket(r2))
	}
	if confBucket(Rule{Confidence: 0.95}) != "H" {
		t.Fatalf("0.95 must bucket H")
	}
	if confBucket(Rule{Confidence: 0.6}) != "M" {
		t.Fatalf("0.6 must bucket M")
	}
	if confBucket(Rule{Confidence: 0.2}) != "L" {
		t.Fatalf("0.2 must bucket L")
	}
}

// --- render -------------------------------------------------------------------

func TestRenderBodyDeterministicAndWrap(t *testing.T) {
	rs := []Rule{
		{ID: "b", Title: "B", Body: "bee  body\nwith  newline", Weight: 0.5, Confidence: 0.5},
		{ID: "a", Title: "A", Body: "ay body", Weight: 0.9, Confidence: 0.9},
	}
	b1 := RenderBody(rs)
	b2 := RenderBody([]Rule{rs[1], rs[0]}) // shuffled
	if b1 != b2 {
		t.Fatalf("render must be order-independent:\n%q\n%q", b1, b2)
	}
	// Higher weight A first.
	if !strings.HasPrefix(b1, "- [H] A:") {
		t.Fatalf("A (weight .9) must render first as H: %q", b1)
	}
	// oneline collapse: no double spaces, no newline inside the body line.
	if strings.Contains(b1, "bee  body") || strings.Contains(b1, "with  newline") {
		t.Fatalf("oneline must collapse whitespace: %q", b1)
	}
	w := WrapBlock(b1)
	if !strings.HasPrefix(w, OIKOS_BEGIN+"\n") || !strings.HasSuffix(w, "\n"+OIKOS_END) {
		t.Fatalf("WrapBlock fences wrong: %q", w)
	}
}
