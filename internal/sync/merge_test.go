package sync

import (
	"reflect"
	"sort"
	"testing"
)

// keys extracts the merge keys of a result set, sorted, for stable assertions.
func keys(recs []Record) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Key())
	}
	sort.Strings(out)
	return out
}

// find returns the record with the given key, or a zero Record + false.
func find(recs []Record, key string) (Record, bool) {
	for _, r := range recs {
		if r.Key() == key {
			return r, true
		}
	}
	return Record{}, false
}

// TestMergeUnionLosesNoRule is the core no-rule-lost guarantee: every key on
// EITHER side survives the merge.
func TestMergeUnionLosesNoRule(t *testing.T) {
	local := []Record{
		{Identity: "a", Title: "A", Body: "a-body", Lamport: 1},
		{Identity: "b", Title: "B", Body: "b-body", Lamport: 1},
	}
	remote := []Record{
		{Identity: "b", Title: "B", Body: "b-body", Lamport: 1}, // same
		{Identity: "c", Title: "C", Body: "c-body", Lamport: 1}, // remote-only
	}
	merged := Merge(local, remote)
	got := keys(merged)
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge dropped a rule: got keys %v, want %v", got, want)
	}
}

// TestMergeConflictHigherLamportWins asserts deterministic LWW by the logical
// clock: the higher-Lamport edit of the same key wins.
func TestMergeConflictHigherLamportWins(t *testing.T) {
	local := []Record{{Identity: "a", Title: "A", Body: "old", Lamport: 1, UpdatedAt: "2026-06-20T00:00:00Z"}}
	remote := []Record{{Identity: "a", Title: "A", Body: "new", Lamport: 5, UpdatedAt: "2026-06-19T00:00:00Z"}}

	merged := Merge(local, remote)
	if len(merged) != 1 {
		t.Fatalf("conflict on one key should yield one record, got %d", len(merged))
	}
	r, _ := find(merged, "a")
	if r.Body != "new" {
		t.Fatalf("higher-Lamport edit must win: got body %q, want \"new\"", r.Body)
	}
}

// TestMergeLamportTieBreaksOnUpdatedAt asserts the secondary tiebreak: equal
// Lamport → the newer UpdatedAt wins.
func TestMergeLamportTieBreaksOnUpdatedAt(t *testing.T) {
	local := []Record{{Identity: "a", Body: "older", Lamport: 3, UpdatedAt: "2026-06-20T00:00:00Z"}}
	remote := []Record{{Identity: "a", Body: "newer", Lamport: 3, UpdatedAt: "2026-06-24T00:00:00Z"}}

	merged := Merge(local, remote)
	r, _ := find(merged, "a")
	if r.Body != "newer" {
		t.Fatalf("equal Lamport must tiebreak on newer UpdatedAt: got %q, want \"newer\"", r.Body)
	}
}

// TestMergeFullTieBreaksOnContentIDDeterministically asserts the final tiebreak:
// equal Lamport AND equal UpdatedAt → resolve by the (deterministic) ContentID,
// so two machines ALWAYS pick the same winner regardless of argument order.
func TestMergeFullTieBreaksOnContentIDDeterministically(t *testing.T) {
	a := Record{Identity: "k", Body: "alpha", Lamport: 2, UpdatedAt: "2026-06-24T00:00:00Z"}
	b := Record{Identity: "k", Body: "bravo", Lamport: 2, UpdatedAt: "2026-06-24T00:00:00Z"}

	// Both argument orders must converge on the identical winner (commutativity).
	m1, _ := find(Merge([]Record{a}, []Record{b}), "k")
	m2, _ := find(Merge([]Record{b}, []Record{a}), "k")
	if m1.Body != m2.Body {
		t.Fatalf("merge is not order-independent on a full tie: %q vs %q", m1.Body, m2.Body)
	}
}

// TestMergeIsCommutativeAcrossAllKeys asserts the whole-vault result is
// order-independent (same set, same winners) when swapping local/remote — the
// property any future CRDT layer must also hold.
func TestMergeIsCommutativeAcrossAllKeys(t *testing.T) {
	local := []Record{
		{Identity: "a", Body: "a1", Lamport: 1},
		{Identity: "b", Body: "b2", Lamport: 9},
	}
	remote := []Record{
		{Identity: "a", Body: "a9", Lamport: 9},
		{Identity: "b", Body: "b1", Lamport: 1},
		{Identity: "c", Body: "c1", Lamport: 1},
	}
	fwd := Merge(local, remote)
	rev := Merge(remote, local)

	sortByKey := func(rs []Record) {
		sort.Slice(rs, func(i, j int) bool { return rs[i].Key() < rs[j].Key() })
	}
	sortByKey(fwd)
	sortByKey(rev)
	if !reflect.DeepEqual(fwd, rev) {
		t.Fatalf("merge is not commutative:\n fwd=%+v\n rev=%+v", fwd, rev)
	}
	// And the winning bodies are the higher-Lamport ones on each conflicted key.
	ra, _ := find(fwd, "a")
	rb, _ := find(fwd, "b")
	if ra.Body != "a9" {
		t.Fatalf("key a should resolve to the Lamport-9 edit a9, got %q", ra.Body)
	}
	if rb.Body != "b2" {
		t.Fatalf("key b should resolve to the Lamport-9 edit b2, got %q", rb.Body)
	}
}

// TestMergeIsIdempotent asserts merging a vault with itself is a no-op (a sync
// with no divergence changes nothing).
func TestMergeIsIdempotent(t *testing.T) {
	v := []Record{
		{Identity: "a", Body: "a", Lamport: 4},
		{Identity: "b", Body: "b", Lamport: 7},
	}
	merged := Merge(v, v)
	if len(merged) != 2 {
		t.Fatalf("self-merge must not duplicate: got %d records, want 2", len(merged))
	}
	ra, _ := find(merged, "a")
	rb, _ := find(merged, "b")
	if ra.Body != "a" || rb.Body != "b" {
		t.Fatalf("self-merge altered content: %+v", merged)
	}
}

// TestMergeResultIsDeterministicallyOrdered asserts the returned slice is in a
// stable, key-sorted order (so a write-back produces byte-stable output and the
// git diff is minimal/deterministic).
func TestMergeResultIsDeterministicallyOrdered(t *testing.T) {
	local := []Record{{Identity: "c"}, {Identity: "a"}}
	remote := []Record{{Identity: "b"}}
	merged := Merge(local, remote)
	got := make([]string, len(merged))
	for i, r := range merged {
		got[i] = r.Key()
	}
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge result not key-sorted: got %v, want %v", got, want)
	}
}
