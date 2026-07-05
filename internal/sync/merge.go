package sync

import "sort"

// Merge deterministically merges two vaults (local, remote) into one, losing NO
// rule: every key present on either side survives. When the same key is edited
// on both sides, the conflict resolves by a TOTAL ORDER so any two machines
// reach the identical result regardless of which side is "local":
//
//  1. higher Lamport (logical clock) wins  — the primary last-writer-wins signal
//  2. tie → newer UpdatedAt (RFC-3339 lexical compare; RFC-3339 sorts correctly)
//  3. tie → larger ContentID (deterministic, content-derived) — the final
//     order-independent tiebreak so the result is commutative
//
// The returned slice is sorted by Key so a write-back is byte-stable and the git
// diff stays minimal.
//
// CRDT SEAM: this is a per-record last-writer-wins register keyed on a Lamport
// clock — the simplest correct CRDT-shaped merge. A future paid Team-Sync layer
// replaces `less` with a richer conflict resolver (per-field LWW-registers, or
// an RGA/text-CRDT over Body for character-level merge) WITHOUT changing this
// function's contract: take two record sets, return one deterministic, lossless
// union. Callers (oikos sync, the git transport) depend only on that contract.
func Merge(local, remote []Record) []Record {
	winners := make(map[string]Record, len(local)+len(remote))

	consider := func(r Record) {
		k := r.Key()
		cur, ok := winners[k]
		if !ok || less(cur, r) {
			winners[k] = r
		}
	}
	for _, r := range local {
		consider(r)
	}
	for _, r := range remote {
		consider(r)
	}

	out := make([]Record, 0, len(winners))
	for _, r := range winners {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// less reports whether a should LOSE to b (i.e. b is the winner) under the total
// order. It is a strict, total, deterministic order over conflicting records so
// Merge is commutative and associative.
func less(a, b Record) bool {
	if a.Lamport != b.Lamport {
		return a.Lamport < b.Lamport
	}
	if a.UpdatedAt != b.UpdatedAt {
		// RFC-3339 timestamps sort correctly lexically; a non-timestamp string
		// still yields a deterministic order (we only need totality here).
		return a.UpdatedAt < b.UpdatedAt
	}
	// Final, content-derived, order-independent tiebreak.
	return a.ContentID() < b.ContentID()
}
