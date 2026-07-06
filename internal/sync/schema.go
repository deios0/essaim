// Package sync is the M4 rule-sync PRIMITIVE — the $0, BYO-storage scaffold the
// future paid Team-Rule-Sync drops into. It is NOT the paid product. It defines
// a stable rule-store record (the vault rule + a content-addressed id + a
// logical clock + an updated-at), and a deterministic, no-rule-lost merge of two
// divergent vaults. The git transport (essaim sync) and this merge leave a clean
// seam where a real CRDT/Team-Sync layer plugs in later (see Merger/CRDT seam).
//
// Design invariants (docs/specs/2026-06-24-rule-sync-primitive.md):
//   - The Markdown vault stays the SINGLE source of truth. Sync metadata rides
//     ALONG in frontmatter (cid/lamport/updated_at), it does not replace the vault
//     with a database.
//   - The content address (cid) hashes DURABLE content only — never the logical
//     clock or updated_at — so two machines that converge on the same content
//     agree on the cid regardless of clock skew. This is the dedup/CRDT anchor.
//   - Merge is deterministic and loses NO rule: every key present on either side
//     survives; conflicts resolve by a total order (Lamport clock, then
//     updated_at, then cid) so any two machines reach the same result.
package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Record is one rule as it participates in sync: the durable vault content plus
// the sync transport metadata. It is intentionally a flat, transport-friendly
// projection of internal/rules.Rule — the sync layer never depends on the hot
// path, and the hot path never learns about sync.
type Record struct {
	// Identity is the merge KEY: the rule's stable id / filename stem. Two
	// machines editing "the same rule" must agree on this. It is NOT part of the
	// content hash (a rename keeps the cid).
	Identity string

	// --- durable content (participates in the content address) ----------------
	Title  string
	Body   string
	Kind   string
	Status string

	// --- sync transport metadata (does NOT participate in the content address) -
	// Lamport is the logical clock: a monotonic counter bumped on every local
	// edit. It is the primary conflict tiebreak and the field a future CRDT
	// (e.g. a per-field LWW-register or an RGA over the body) widens into a
	// vector/dotted clock. UpdatedAt is the wall-clock RFC-3339 secondary
	// tiebreak (clocks can tie across machines; wall time rarely does).
	Lamport   int64
	UpdatedAt string

	// RemoteOrigin marks a record pulled from a remote and quarantined. Stamped by
	// quarantineRemote and written to the _inbox draft frontmatter
	// (remote_origin: true) so the lifecycle sweep refuses to auto-promote it — the
	// quarantine wall must survive a title-collision reinforce attack (P1). Not
	// part of the content address (provenance/transport metadata).
	RemoteOrigin bool

	// CredentialFlagged marks a quarantined remote rule whose durable content
	// tripped the credential gate: quarantineRemote REDACTED the secret from the
	// Body/Title and set this flag (P2-5). It surfaces to the human reviewer (a
	// `credential_redacted: true` frontmatter marker) so a stripped secret is
	// visible, and it guarantees a remote secret is NEVER persisted plaintext in
	// the local _inbox/. Not part of the content address (provenance/transport
	// metadata; the redaction already changed the content the cid hashes).
	CredentialFlagged bool
}

// ContentID returns the content address of the record's DURABLE content as
// "sha256:<hex>". It is deterministic and stable: identical content on any
// machine yields the identical id, and it is independent of the Identity key and
// of all sync transport metadata (Lamport/UpdatedAt). This is the dedup key and
// the anchor a CRDT merge keys per-field state on.
func (r Record) ContentID() string {
	// A length-prefixed, field-tagged canonical form so no two distinct field
	// layouts can collide (e.g. title="a" body="b" vs title="ab" body=""). The
	// tags + explicit separators make the pre-image unambiguous.
	var b strings.Builder
	writeField(&b, "title", r.Title)
	writeField(&b, "body", r.Body)
	writeField(&b, "kind", r.Kind)
	writeField(&b, "status", r.Status)
	sum := sha256.Sum256([]byte(b.String()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// writeField appends a tagged, length-prefixed field to the canonical pre-image.
func writeField(b *strings.Builder, name, val string) {
	b.WriteString(name)
	b.WriteByte('=')
	// Length-prefix the value so an embedded delimiter can never forge a boundary.
	b.WriteString(itoa(len(val)))
	b.WriteByte(':')
	b.WriteString(val)
	b.WriteByte('\n')
}

// itoa is a tiny base-10 formatter (avoids pulling in strconv for one call and
// keeps the canonical form locale-independent).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// Key returns the merge key for this record: the Identity when set, else the
// ContentID (so an id-less rule still has a stable, content-derived key and two
// identical bodies dedup instead of duplicating).
func (r Record) Key() string {
	if id := strings.TrimSpace(r.Identity); id != "" {
		return id
	}
	return r.ContentID()
}
