package brain

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// brainMirrorDir is the vault subdir essaim owns for the Brain-zone mirror. It is
// MANAGED (rewritten on every pull) — never hand-edit it; put your own rules in
// the vault root. The existing emit path loads it (LoadVault recurses) and ranks
// these alongside the user's own rules.
const brainMirrorDir = "_brain"

var safeID = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// WriteVault mirrors the pulled zone rules into <vault>/_brain/ as live .md files
// that the existing emit path picks up. It REPLACES the whole mirror each call,
// so a rule removed from the zone disappears locally too. Only the mirror dir is
// touched — the user's own vault rules are never altered.
func WriteVault(vaultDir string, rs []Rule) error {
	if strings.TrimSpace(vaultDir) == "" {
		return fmt.Errorf("essaim brain: no vault directory")
	}
	dir := filepath.Join(vaultDir, brainMirrorDir)
	// Replace the mirror: remove it, then rewrite. RemoveAll on a fresh/missing
	// dir is a no-op, so first-run is fine.
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if len(rs) == 0 {
		return nil // nothing to mirror (zone empty) — leave no _brain dir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for i, r := range rs {
		id := safeID.ReplaceAllString(strings.TrimSpace(r.ID), "-")
		if id == "" {
			id = fmt.Sprintf("rule-%d", i)
		}
		title := strings.TrimSpace(r.Title)
		if title == "" {
			title = titleFromBody(r.Body) // never surface the opaque UUID as the title
		}
		// A managed, live, injectable rule. `source: brain-zone` marks provenance so
		// a reader knows it came from the zone, not their own vault.
		doc := "---\n" +
			"id: brain-" + id + "\n" +
			"title: " + sanitizeYAML(title) + "\n" +
			"status: live\n" +
			"source: brain-zone\n" +
			"---\n" +
			strings.TrimSpace(r.Body) + "\n"
		if err := os.WriteFile(filepath.Join(dir, id+".md"), []byte(doc), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// sanitizeYAML keeps a title safe on one YAML scalar line (strip newlines, cap).
func sanitizeYAML(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// titleFromBody derives a short human title from a rule body when the source has
// none: the first sentence/clause, capped, with newlines flattened. Falls back to
// a generic label for an empty body.
func titleFromBody(body string) string {
	s := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(body, "\n", " "), "\r", " "))
	if s == "" {
		return "zone rule"
	}
	// Cut at the first sentence/clause boundary for a tidy title.
	for _, sep := range []string{". ", ": ", " — ", "; "} {
		if i := strings.Index(s, sep); i > 0 && i <= 80 {
			s = s[:i]
			break
		}
	}
	if len(s) > 80 {
		s = strings.TrimSpace(s[:80])
	}
	return sanitizeYAML(s)
}
