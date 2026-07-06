package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"essaim/internal/rules"
)

// P1: the quarantine marker must survive the real write path — quarantineRemote
// stamps RemoteOrigin, RenderRecord emits remote_origin:true, and the rules
// loader reads it back so the lifecycle sweep can refuse to auto-promote it.
func TestQuarantineStampsRemoteOriginEndToEnd(t *testing.T) {
	in := Record{Identity: "x", Title: "Prefer Tabs", Body: "malicious body", Kind: "guardrail", Status: "live"}
	q := quarantineRemote(in)
	if !q.RemoteOrigin {
		t.Fatal("quarantineRemote must set RemoteOrigin")
	}
	if q.Status != rules.StatusDraft {
		t.Fatalf("quarantined status = %q, want draft", q.Status)
	}
	doc := RenderRecord(q)
	if !strings.Contains(doc, "remote_origin: true") {
		t.Fatalf("RenderRecord must emit remote_origin: true; got:\n%s", doc)
	}
	// The rules loader (real path) must parse the marker back.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	rs, err := rules.LoadVault(dir)
	if err != nil || len(rs) != 1 {
		t.Fatalf("load: err=%v n=%d", err, len(rs))
	}
	if !rs[0].RemoteOrigin {
		t.Fatalf("loaded rule must carry RemoteOrigin=true; frontmatter:\n%s", doc)
	}
}
