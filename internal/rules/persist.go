package rules

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// RuleWithPath pairs a loaded Rule with its on-disk file path, for the lifecycle
// sweep (which must rewrite the exact file on a class-cross).
type RuleWithPath struct {
	Rule Rule
	Path string
}

// LoadVaultWithPaths is LoadVault but also returns each rule's source file path.
// Used by the lifecycle sweep; the hot path uses LoadVault (no paths).
func LoadVaultWithPaths(dir string) ([]RuleWithPath, error) {
	if dir == "" {
		return nil, nil
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, nil
	}
	var out []RuleWithPath
	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".md") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		r, perr := parseRule(path, raw)
		if perr != nil {
			return nil
		}
		out = append(out, RuleWithPath{Rule: r, Path: path})
		return nil
	})
	return out, walkErr
}

var wsRunPersist = regexp.MustCompile(`\s+`)

// normalizeTitle ports Brain normalize_title (utils.py:6-8): strip → lowercase →
// collapse whitespace. Mirrors extract.normalizeTitle so the title-hash dedup
// key is identical across the extractor and the sweep.
func normalizeTitle(title string) string {
	return wsRunPersist.ReplaceAllString(strings.ToLower(strings.TrimSpace(title)), " ")
}

// TitleHashOf is the dedup primary key: sha256 of the normalized title (port of
// compute_title_hash, utils.py:11-13). Shared by the extractor (write-time) and
// the lifecycle sweep (dedup/reinforce/supersede) so they agree on identity.
func TitleHashOf(title string) string {
	sum := sha256.Sum256([]byte(normalizeTitle(title)))
	return hex.EncodeToString(sum[:])
}

// RenderRuleFile renders a Rule's DURABLE frontmatter + the given body into a
// one-rule-per-file `.md` document. Used by the lifecycle sweep's class-cross
// write. Only durable-coarse fields are written; hot counters never appear.
func RenderRuleFile(r Rule, body string) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "id: %s\n", r.ID)
	fmt.Fprintf(&b, "title: %s\n", yamlScalar(r.Title))
	if r.Kind != "" {
		fmt.Fprintf(&b, "kind: %s\n", r.Kind)
	}
	if r.Scope != "" {
		fmt.Fprintf(&b, "scope: %s\n", r.Scope)
	}
	if r.ProjectTag != "" {
		fmt.Fprintf(&b, "project_tag: %s\n", r.ProjectTag)
	}
	if r.LoadMode != "" {
		fmt.Fprintf(&b, "load_mode: %s\n", r.LoadMode)
	}
	fmt.Fprintf(&b, "status: %s\n", r.Status)
	fmt.Fprintf(&b, "confidence: %s\n", trimFloat(r.Confidence))
	fmt.Fprintf(&b, "weight: %s\n", trimFloat(r.Weight))
	if r.Timeless {
		b.WriteString("timeless: true\n")
	}
	fmt.Fprintf(&b, "criticality: %q\n", r.Criticality)
	if r.HalfLife > 0 {
		fmt.Fprintf(&b, "half_life_days: %s\n", trimFloat(r.HalfLife))
	}
	if r.DaysSinceReinforced > 0 {
		fmt.Fprintf(&b, "days_since_reinforced: %s\n", trimFloat(r.DaysSinceReinforced))
	}
	if r.LastReinforcedAt != "" {
		fmt.Fprintf(&b, "last_reinforced_at: %s\n", r.LastReinforcedAt)
	}
	if r.RemoteOrigin {
		// Preserve the sync quarantine marker across sweep rewrites so a remote
		// draft can never lose its "never auto-promote" flag (P1).
		b.WriteString("remote_origin: true\n")
	}
	b.WriteString("---\n")
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteByte('\n')
	}
	return b.String()
}

// yamlScalar quotes a YAML scalar when it could be mis-parsed.
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

// trimFloat formats a float compactly (1.0 → "1", 0.65 → "0.65").
func trimFloat(f float64) string { return fmt.Sprintf("%g", f) }
