package rules

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	yaml "gopkg.in/yaml.v3"
)

// frontmatterDelim is the YAML frontmatter fence used by Obsidian-style notes.
const frontmatterDelim = "---"

// parseRule parses a single `.md` document of the form:
//
//	---
//	id: ...
//	title: ...
//	---
//	<markdown body>
//
// A document with no frontmatter fence is treated as a body-only rule with
// zero-value frontmatter (so an arbitrary note never crashes the loader). The
// returned Rule's Body is the text after the closing fence, trimmed of leading
// and trailing blank lines.
func parseRule(name string, raw []byte) (Rule, error) {
	s := string(raw)
	// Normalize CRLF so the fence detection is stable across editors/OSes.
	s = strings.ReplaceAll(s, "\r\n", "\n")

	var r Rule
	// A leading `---` is frontmatter ONLY when a closing `---` fence follows. A
	// markdown note that merely BEGINS with `---` as a horizontal rule and has no
	// closing fence is NOT frontmatter — it is body-only, and must not be dropped
	// (P2). But when a closing fence IS present the block is treated as frontmatter
	// and parsed: if the YAML is invalid the file is DROPPED (malformed), NOT
	// recovered as body — recovering it would strip a `status: draft`/`rejected`
	// gate off a real (if slightly-malformed) rule and let it enter the injectable
	// set, bypassing the quarantine/draft wall (codex review). yaml.Unmarshal
	// handles comment/blank first lines, so a real frontmatter is never misread as
	// an HR.
	// A leading `---` is frontmatter only when a closing `---` fence follows AND the
	// block between the fences is valid YAML for a rule mapping — let the YAML
	// parser itself be the arbiter (no fragile line heuristic that a quoted key
	// `"status":` would slip past). Two outcomes preserve the draft/quarantine wall
	// AND the "don't drop an HR-leading note" P2 fix:
	//   - closing fence + parses  → use the frontmatter (status respected).
	//   - closing fence + INVALID → DROP (malformed frontmatter; recovering it as
	//     body would strip a status:draft/rejected gate and let it enter injection).
	// A leading `---` with NO closing fence is a markdown horizontal rule / genuine
	// body (oikos-authored and synced rules ALWAYS carry a closing fence, so this
	// path is never one of them) → index as body-only so the note is not dropped.
	if strings.HasPrefix(s, frontmatterDelim+"\n") {
		rest := strings.TrimPrefix(s[len(frontmatterDelim):], "\n")
		end := findClosingFence(rest)
		switch {
		case end >= 0:
			// Closing fence present → the block IS frontmatter. Let the YAML parser be
			// the arbiter (handles quoted keys like `"status":`, comments, blanks — a
			// line heuristic would miss them and strip the status): parse → use it;
			// INVALID → DROP (recovering it as body would strip a status:draft/rejected
			// gate and let a non-injectable rule enter injection — codex review).
			if err := yaml.Unmarshal([]byte(rest[:end]), &r); err != nil {
				return Rule{}, fmt.Errorf("%s: bad frontmatter: %w", name, err)
			}
			body := strings.TrimPrefix(strings.TrimPrefix(rest[end:], frontmatterDelim), "\n")
			r.Body = strings.TrimSpace(body)
		case !blockHasYAMLKey(rest):
			// No closing fence AND no `key:` line → a genuine markdown horizontal rule
			// / prose note that merely begins with `---`. Index it as body-only (P2 —
			// don't drop it). oikos-authored and synced rules ALWAYS carry a closing
			// fence, so this path is never one of them: no status can be lost.
			r.Body = strings.TrimSpace(s)
		default:
			// No closing fence but key lines present → an unterminated frontmatter the
			// author clearly intended as a rule but never closed → DROP (malformed),
			// never recover as body (status-wall).
			return Rule{}, fmt.Errorf("%s: unterminated YAML frontmatter", name)
		}
	} else {
		r.Body = strings.TrimSpace(s)
	}

	// Default the ID to the file stem when frontmatter omits it, so every rule
	// has a stable, deterministic identity for tiebreak sorting and dedup.
	if r.ID == "" {
		r.ID = strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	}
	return r, nil
}

// blockHasYAMLKey reports whether a candidate frontmatter block (the text after a
// leading `---`, up to a closing fence) contains AT LEAST ONE YAML mapping key
// line. This is what distinguishes real frontmatter INTENT (a `key:` line
// anywhere in the block — even after a leading comment/blank line, which a
// first-line-only check misclassified) from a markdown horizontal rule whose body
// is pure prose/headings. Frontmatter-intent blocks are parsed or dropped, never
// recovered as body (which would strip a status gate); a no-key block is a
// genuine HR-leading note indexed as body-only.
func blockHasYAMLKey(block string) bool {
	for _, line := range strings.Split(block, "\n") {
		if isYAMLKeyLine(line) {
			return true
		}
	}
	return false
}

// isYAMLKeyLine reports whether line looks like the first line of a YAML mapping:
// an unquoted key token followed by a colon (`key:` or `key: value`). It is a
// deliberately narrow gate — enough to tell a frontmatter opener from an HR line
// or prose — not a full YAML validator (the yaml.Unmarshal that follows is the
// real parse). A `---` closing fence line is not a key line.
func isYAMLKeyLine(line string) bool {
	line = strings.TrimLeft(line, " \t")
	if line == "" || strings.HasPrefix(line, "#") {
		return false // blank or comment line — not a mapping key
	}
	i := strings.IndexByte(line, ':')
	if i <= 0 {
		return false // no colon, or colon at position 0 → not `key:`
	}
	key := line[:i]
	// The key must be a plain scalar token (letters, digits, `_`, `-`, `.`). This
	// rejects prose that merely contains a colon (e.g. "See this: a note").
	for _, ch := range key {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9',
			ch == '_', ch == '-', ch == '.':
		default:
			return false
		}
	}
	// After the colon, YAML requires either end-of-line or a space before the value.
	rest := line[i+1:]
	return rest == "" || rest[0] == ' ' || rest[0] == '\t'
}

// findClosingFence returns the byte offset (within rest) of the start of the
// line that is exactly "---", or -1 if none. The offset points AT the fence
// line so the caller can slice the body after it.
func findClosingFence(rest string) int {
	off := 0
	for {
		nl := strings.IndexByte(rest[off:], '\n')
		var line string
		if nl < 0 {
			line = rest[off:]
		} else {
			line = rest[off : off+nl]
		}
		if strings.TrimRight(line, " \t") == frontmatterDelim {
			return off
		}
		if nl < 0 {
			return -1
		}
		off += nl + 1
	}
}

// statDir reports whether path exists and is a directory.
func statDir(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}

// walkDirs calls fn for dir and every subdirectory under it (for fsnotify, which
// is non-recursive). Unreadable subtrees are skipped.
func walkDirs(dir string, fn func(string)) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			fn(path)
		}
		return nil
	})
}

// LoadVault walks dir recursively and parses every `*.md` file into a Rule.
// Unreadable or malformed files are SKIPPED (not fatal) so one bad note never
// blinds the whole index — the request path must degrade to "fewer rules", not
// to an error. The returned slice order follows filepath.Walk (lexical); the
// index and bloat guard impose deterministic ordering downstream.
//
// If dir is empty or does not exist, LoadVault returns (nil, nil): no vault =
// no rules = no injection, cleanly (spec: skip cleanly when OIKOS_VAULT unset).
func LoadVault(dir string) ([]Rule, error) {
	if dir == "" {
		return nil, nil
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, nil
	}
	var out []Rule
	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries, keep walking
		}
		if d.IsDir() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".md") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil // skip unreadable file
		}
		r, perr := parseRule(path, raw)
		if perr != nil {
			return nil // skip malformed file (do not fail the whole load)
		}
		out = append(out, r)
		return nil
	})
	return out, walkErr
}
