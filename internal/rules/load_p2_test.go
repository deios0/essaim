package rules

import (
	"testing"
)

// P2-3: a markdown note that LEGITIMATELY begins with `---` (a horizontal rule as
// its first line, NOT YAML frontmatter) must be indexed as a body-only rule, not
// silently dropped. The old loader treated any leading `---` as the start of
// frontmatter; with no closing fence it errored and LoadVault skipped the file,
// losing a real note. A leading `---` is frontmatter ONLY when it is a genuine
// opener (immediately followed by a `key:` line) that is properly closed and
// parses; otherwise the file is body-only.
func TestParseRuleLeadingHorizontalRuleIsBodyOnly(t *testing.T) {
	// `---` on line 1 as a horizontal rule, then a blank line and prose. No YAML.
	raw := []byte("---\n\n# Section\n\nThis note starts with a horizontal rule.\n")
	r, err := parseRule("hr-note.md", raw)
	if err != nil {
		t.Fatalf("a `---`-leading note must NOT error (it is body-only, not frontmatter): %v", err)
	}
	if r.ID != "hr-note" {
		t.Fatalf("id should default to the file stem for a body-only note; got %q", r.ID)
	}
	// The whole document (including the leading `---` HR) is preserved as the body.
	if !containsAll(r.Body, "---", "# Section", "This note starts with a horizontal rule.") {
		t.Fatalf("the leading `---` note must be preserved as body; got %q", r.Body)
	}
}

// A `---`-leading note whose first line after the fence is prose (no colon key)
// and never closes is still body-only, not an unterminated-frontmatter error.
func TestParseRuleLeadingRuleThenProseIsBodyOnly(t *testing.T) {
	raw := []byte("---\nJust some prose with no frontmatter here at all.\nSecond line.\n")
	r, err := parseRule("prose.md", raw)
	if err != nil {
		t.Fatalf("prose after a leading `---` must be body-only, not an error: %v", err)
	}
	if !containsAll(r.Body, "Just some prose", "Second line.") {
		t.Fatalf("prose body must be preserved; got %q", r.Body)
	}
}

// Preserve existing behavior: a REAL frontmatter note (opener, key lines, closing
// fence) still parses into structured fields.
func TestParseRuleRealFrontmatterStillParses(t *testing.T) {
	raw := []byte("---\nid: rule-x\ntitle: Rule X\nstatus: live\nweight: 0.7\n---\nUse rule X.\n")
	r, err := parseRule("rule-x.md", raw)
	if err != nil {
		t.Fatalf("valid frontmatter must still parse: %v", err)
	}
	if r.ID != "rule-x" || r.Title != "Rule X" || r.Status != "live" || r.Weight != 0.7 {
		t.Fatalf("frontmatter fields not parsed: %+v", r)
	}
	if r.Body != "Use rule X." {
		t.Fatalf("body wrong: %q", r.Body)
	}
}

// Preserve existing behavior: a genuine frontmatter OPENER (`---` immediately
// followed by `key:` lines) that is never closed is still treated as malformed
// frontmatter and dropped by LoadVault — it is NOT recovered as body (that would
// re-index half-written config the author clearly intended as frontmatter).
func TestParseRuleUnterminatedFrontmatterOpenerStillErrors(t *testing.T) {
	raw := []byte("---\nid: bad\ntitle: [unterminated\n")
	if _, err := parseRule("bad.md", raw); err == nil {
		t.Fatal("an unterminated frontmatter OPENER (key lines, no closing fence) must still error/drop")
	}
}

// A leading `---` with `key:` lines and a closing fence but INVALID YAML is
// DROPPED (malformed frontmatter), NOT recovered as body — recovering it would
// strip a status:draft/rejected gate off a real rule and let it enter injection
// (SECURITY: the draft/quarantine wall must hold; codex integral review).
func TestParseRuleClosedButInvalidYAMLIsDropped(t *testing.T) {
	raw := []byte("---\nid: x\nbroken: [a, b\n---\nreal body content here\n")
	if _, err := parseRule("weird.md", raw); err == nil {
		t.Fatal("a closed-but-invalid-YAML frontmatter must be DROPPED, not recovered as body (status-wall)")
	}
	// And the security-critical case: a draft with malformed YAML must NOT become
	// an injectable (empty-status) rule.
	draft := []byte("---\nid: d\nstatus: draft\nbroken: [x\n---\nbody\n")
	if _, err := parseRule("d.md", draft); err == nil {
		t.Fatal("a status:draft rule with malformed frontmatter must be DROPPED, never recovered status-less")
	}
}

// LoadVault integration: a `---`-leading HR note in the vault is indexed (not
// dropped), while a genuine unterminated-frontmatter opener is still skipped.
func TestLoadVaultKeepsHRNoteDropsUnterminatedOpener(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "hr.md", "---\n\n# Heading\n\nbody of an hr-leading note")
	writeFile(t, dir, "opener.md", "---\nid: opener\ntitle: [unterminated\n")
	rs, err := LoadVault(dir)
	if err != nil {
		t.Fatalf("LoadVault: %v", err)
	}
	ids := map[string]bool{}
	for _, r := range rs {
		ids[r.ID] = true
	}
	if !ids["hr"] {
		t.Fatalf("the `---`-leading HR note must be indexed as body-only; got %v", ids)
	}
	if ids["opener"] {
		t.Fatalf("a genuine unterminated frontmatter opener must still be dropped; got %v", ids)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// SECURITY (codex): a QUOTED status key ("status": draft) must be respected — a
// line-heuristic that only recognized unquoted keys would treat this as body-only
// and strip the draft gate. The YAML parser (the arbiter for a fenced block)
// handles it, so the rule stays a non-injectable draft.
func TestParseRuleQuotedStatusKeyRespected(t *testing.T) {
	raw := []byte("---\n\"status\": draft\nid: q\n---\nbody\n")
	r, err := parseRule("q.md", raw)
	if err != nil {
		t.Fatalf("valid frontmatter with a quoted key must parse: %v", err)
	}
	if r.Status != "draft" {
		t.Fatalf("SECURITY: quoted status key must be respected (draft), got %q — would be injectable", r.Status)
	}
}
