package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"oikos/internal/rules"

	yaml "gopkg.in/yaml.v3"
)

// frontmatterDelim is the YAML frontmatter fence used by the Markdown vault.
const frontmatterDelim = "---"

// recordFrontmatter is the subset of vault frontmatter the sync layer reads and
// writes. It deliberately overlaps internal/rules.Rule on the durable fields and
// ADDS the sync transport fields (lamport/updated_at) — keeping the Markdown
// vault the single source of truth, with sync metadata riding ALONG in
// frontmatter rather than in a side database.
type recordFrontmatter struct {
	ID                 string `yaml:"id"`
	Title              string `yaml:"title"`
	Kind               string `yaml:"kind"`
	Status             string `yaml:"status"`
	RemoteOrigin       bool   `yaml:"remote_origin"`
	CredentialRedacted bool   `yaml:"credential_redacted"`
	Lamport            int64  `yaml:"lamport"`
	UpdatedAt          string `yaml:"updated_at"`
}

// LoadVaultRecords walks dir recursively and parses every `*.md` file into a
// Record, reading the sync metadata (lamport/updated_at) from frontmatter when
// present. A rule with no `id` defaults its Identity to the filename stem (a
// stable merge key). Malformed/unreadable files are SKIPPED, never fatal — one
// bad note never blinds a whole sync. An empty/missing dir returns (nil, nil).
//
// The vault's _inbox/ subtree is SKIPPED: it holds the local, gitignored
// quarantine (status:draft remote rules awaiting the user's explicit accept) and
// the hot-store sidecar. Reading the ACTIVE vault must never see a quarantined
// draft — that is the P0 supply-chain wall: a pulled remote rule is in _inbox/,
// so it is invisible to the active-set read, to the push set, and to the
// credential push-gate. (To read the quarantine itself, call LoadVaultRecords on
// the inbox dir directly.)
func LoadVaultRecords(dir string) ([]Record, error) {
	if dir == "" {
		return nil, nil
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, nil
	}
	var out []Record
	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			// Skip the _inbox/ quarantine subtree (local, gitignored drafts). Only
			// skip it when it is a DIRECT child of the vault root we were asked to
			// load — so a caller that points LoadVaultRecords AT the inbox to read
			// the quarantine still works.
			if d.Name() == rules.InboxDir && filepath.Dir(path) == dir {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".md") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		r, perr := parseRecord(path, raw)
		if perr != nil {
			return nil
		}
		out = append(out, r)
		return nil
	})
	return out, walkErr
}

// parseRecord parses one `.md` document (YAML frontmatter + body) into a Record.
// The Identity defaults to the filename stem when `id` is absent.
func parseRecord(path string, raw []byte) (Record, error) {
	s := strings.ReplaceAll(string(raw), "\r\n", "\n")

	var fm recordFrontmatter
	body := s
	if strings.HasPrefix(s, frontmatterDelim+"\n") || s == frontmatterDelim {
		rest := strings.TrimPrefix(s[len(frontmatterDelim):], "\n")
		end := findClosingFence(rest)
		if end < 0 {
			return Record{}, fmt.Errorf("%s: unterminated YAML frontmatter", path)
		}
		fmText := rest[:end]
		body = strings.TrimPrefix(rest[end:], frontmatterDelim)
		body = strings.TrimPrefix(body, "\n")
		if err := yaml.Unmarshal([]byte(fmText), &fm); err != nil {
			return Record{}, fmt.Errorf("%s: bad frontmatter: %w", path, err)
		}
	}

	id := strings.TrimSpace(fm.ID)
	if id == "" {
		id = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return Record{
		Identity:          id,
		Title:             fm.Title,
		Body:              strings.TrimSpace(body),
		Kind:              fm.Kind,
		Status:            fm.Status,
		RemoteOrigin:      fm.RemoteOrigin,       // preserve the quarantine marker on read (codex review)
		CredentialFlagged: fm.CredentialRedacted, // preserve the P2-5 credential-strip marker on read
		Lamport:           fm.Lamport,
		UpdatedAt:         fm.UpdatedAt,
	}, nil
}

// findClosingFence returns the byte offset (within rest) of the line that is
// exactly "---", or -1 if none.
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

// RenderRecord renders a Record to a one-rule-per-file Markdown document:
// durable content + the sync transport metadata in frontmatter. The output is
// deterministic (stable field order) so a write-back is byte-stable and git
// diffs stay minimal.
func RenderRecord(r Record) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "id: %s\n", r.Identity)
	if r.Title != "" {
		fmt.Fprintf(&b, "title: %s\n", yamlScalar(r.Title))
	}
	if r.Kind != "" {
		fmt.Fprintf(&b, "kind: %s\n", r.Kind)
	}
	if r.Status != "" {
		fmt.Fprintf(&b, "status: %s\n", r.Status)
	}
	if r.RemoteOrigin {
		// Quarantine provenance marker: the lifecycle sweep reads this
		// (rules.Rule.RemoteOrigin) and refuses to auto-promote the draft (P1).
		b.WriteString("remote_origin: true\n")
	}
	if r.CredentialFlagged {
		// P2-5: a credential was stripped from this remote rule on quarantine. The
		// marker surfaces the strip to the human reviewer; the body already carries
		// [REDACTED] in place of the secret, so nothing plaintext is persisted.
		b.WriteString("credential_redacted: true\n")
	}
	// Sync transport metadata — always written so the clock survives a round-trip.
	fmt.Fprintf(&b, "lamport: %d\n", r.Lamport)
	if r.UpdatedAt != "" {
		fmt.Fprintf(&b, "updated_at: %s\n", r.UpdatedAt)
	}
	// The content address is written for human/debug visibility and as the dedup
	// anchor a future CRDT keys on; it is DERIVED, never read back as input.
	fmt.Fprintf(&b, "cid: %s\n", r.ContentID())
	b.WriteString("---\n")
	b.WriteString(r.Body)
	if !strings.HasSuffix(r.Body, "\n") {
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

// WriteVaultRecords writes each Record as one `<identity>.md` file under dir,
// atomically (temp-then-rename) so a reader never sees a half-written file. The
// dir is created as needed. This is the merged-vault write-back the git sync
// performs after a Merge.
func WriteVaultRecords(dir string, recs []Record) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, r := range recs {
		name := sanitizeFileStem(r.Key()) + ".md"
		if err := atomicWrite(filepath.Join(dir, name), []byte(RenderRecord(r))); err != nil {
			return err
		}
	}
	return nil
}

// sanitizeFileStem makes a merge key safe as a filename stem (the key is usually
// an id like "no-force-push", but a content-id fallback contains a ':').
func sanitizeFileStem(key string) string {
	repl := func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '-'
		}
		return r
	}
	return strings.Map(repl, key)
}

// atomicWrite writes data to path via write-temp-then-rename so a reader never
// sees a partial file. The parent dir must already exist.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".oikos-sync-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}
