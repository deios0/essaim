package extract

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultHalfLifeDays is the decay half-life stamped on every oikos-AUTHORED rule
// (P2-2). Without it a written rule loads with HalfLife==0 and DecayedEffWeight /
// effWeight never decay it — so a wrongly-promoted rule would live forever. 30
// days is the spec's canonical preference half-life (v1-spec example
// `half_life_days: 30`) and matches the `kind: preference` the extractor writes.
// A timeless/guardrail rule is authored by hand, never by the extractor, so this
// default only ever lands on learned preference rules that SHOULD age out if
// never reinforced.
const DefaultHalfLifeDays = 30.0

// ruleDoc is the frontmatter+body of one written `.md` rule. Only DURABLE fields
// are written; hot counters never touch frontmatter (the frontmatter-immutability rule).
type ruleDoc struct {
	ID               string
	Title            string
	Kind             string
	Status           string
	Confidence       float64
	Weight           float64
	HalfLife         float64
	Criticality      string
	LastReinforcedAt string
	Body             string
}

// renderRuleDoc renders a one-rule-per-file Obsidian-style `.md` document
// (parseRule is strictly one-rule-per-file). The body is written verbatim after
// the closing fence.
func renderRuleDoc(d ruleDoc) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "id: %s\n", d.ID)
	fmt.Fprintf(&b, "title: %s\n", yamlScalar(d.Title))
	if d.Kind != "" {
		fmt.Fprintf(&b, "kind: %s\n", d.Kind)
	}
	fmt.Fprintf(&b, "status: %s\n", d.Status)
	fmt.Fprintf(&b, "confidence: %s\n", trimFloat(d.Confidence))
	fmt.Fprintf(&b, "weight: %s\n", trimFloat(d.Weight))
	// half_life_days: written only when positive (mirrors the canonical
	// rules/persist.go writer). An oikos-authored rule MUST carry it so the decay
	// clock can retire an unreinforced (or wrongly-promoted) rule — otherwise
	// HalfLife loads as 0 and the rule never decays (P2-2).
	if d.HalfLife > 0 {
		fmt.Fprintf(&b, "half_life_days: %s\n", trimFloat(d.HalfLife))
	}
	// criticality is written as an (optionally empty) STRING — Rule.Criticality
	// is string-typed and the demote-immunity compare Atoi-guards it.
	fmt.Fprintf(&b, "criticality: %q\n", d.Criticality)
	if d.LastReinforcedAt != "" {
		fmt.Fprintf(&b, "last_reinforced_at: %s\n", d.LastReinforcedAt)
	}
	b.WriteString("---\n")
	b.WriteString(d.Body)
	if !strings.HasSuffix(d.Body, "\n") {
		b.WriteByte('\n')
	}
	return b.String()
}

// yamlScalar quotes a YAML scalar when it could be mis-parsed (contains a colon,
// leading special char, etc.). Conservative: quote anything with a colon/hash or
// leading/trailing space; otherwise leave bare.
func yamlScalar(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, ":#\n\"'") || s != strings.TrimSpace(s) ||
		strings.HasPrefix(s, "- ") || strings.HasPrefix(s, "[") || strings.HasPrefix(s, "{") {
		return fmt.Sprintf("%q", s)
	}
	return s
}

// trimFloat formats a float without a trailing ".000000" tail (so 1.0 → "1",
// 0.65 → "0.65").
func trimFloat(f float64) string {
	s := fmt.Sprintf("%g", f)
	return s
}

// atomicWrite writes data to path via write-temp-then-rename (so a reader never
// sees a half-written file). The parent dir must already exist.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".oikos-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	// Flush the data to stable storage BEFORE the rename so a crash can't leave a
	// renamed-but-zero-length .md that the loader would parse as an empty rule.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	// Best-effort: persist the directory entry so the rename itself survives a
	// crash; failure here never undoes the already-durable file data.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
