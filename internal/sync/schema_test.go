package sync

import (
	"strings"
	"testing"
)

func TestContentIDIsDeterministicAndStable(t *testing.T) {
	r := Record{
		Identity: "no-force-push-main",
		Title:    "Never force-push main",
		Body:     "Do not `git push --force` to the main branch.",
		Kind:     "guardrail",
		Status:   "active",
	}
	id1 := r.ContentID()
	id2 := r.ContentID()
	if id1 != id2 {
		t.Fatalf("ContentID not deterministic: %q vs %q", id1, id2)
	}
	if !strings.HasPrefix(id1, "sha256:") {
		t.Fatalf("ContentID should be a sha256: address, got %q", id1)
	}
	// 7 ("sha256:") + 64 hex.
	if len(id1) != len("sha256:")+64 {
		t.Fatalf("ContentID has unexpected length: %q (len %d)", id1, len(id1))
	}
}

func TestContentIDChangesWithDurableContentOnly(t *testing.T) {
	base := Record{Identity: "r1", Title: "T", Body: "B", Kind: "rule", Status: "active"}

	bodyChanged := base
	bodyChanged.Body = "B2"
	if base.ContentID() == bodyChanged.ContentID() {
		t.Fatal("ContentID must change when the body changes")
	}

	titleChanged := base
	titleChanged.Title = "T2"
	if base.ContentID() == titleChanged.ContentID() {
		t.Fatal("ContentID must change when the title changes")
	}

	// The logical clock and updated_at are sync transport metadata, NOT content;
	// they must NOT perturb the content address (so two machines that converge on
	// the same content agree on the cid regardless of clock skew).
	clockChanged := base
	clockChanged.Lamport = 999
	clockChanged.UpdatedAt = "2026-06-24T00:00:00Z"
	if base.ContentID() != clockChanged.ContentID() {
		t.Fatal("ContentID must NOT change when only the logical clock / updated_at change")
	}
}

func TestContentIDIgnoresIdentityField(t *testing.T) {
	// Identity is the merge KEY (filename/id), not part of the content hash —
	// the cid addresses the *content*, so a rename with identical content keeps
	// the same cid. (Identity drives merge keying; cid drives dedup/CRDT.)
	a := Record{Identity: "old-name", Title: "T", Body: "B", Kind: "rule", Status: "active"}
	b := Record{Identity: "new-name", Title: "T", Body: "B", Kind: "rule", Status: "active"}
	if a.ContentID() != b.ContentID() {
		t.Fatal("ContentID must address content, independent of the Identity key")
	}
}

func TestKeyPrefersIdentityThenFallsBackToContent(t *testing.T) {
	withID := Record{Identity: "my-rule", Title: "T", Body: "B"}
	if withID.Key() != "my-rule" {
		t.Fatalf("Key with an Identity should be the Identity, got %q", withID.Key())
	}
	noID := Record{Title: "T", Body: "B"}
	if noID.Key() != noID.ContentID() {
		t.Fatalf("Key without an Identity should fall back to the ContentID, got %q", noID.Key())
	}
}
