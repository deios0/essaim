package server

import (
	"os"
	"path/filepath"
	"testing"
)

// A fresh serve with NO key and NO request must create ZERO files under the
// data dir. This guards the purity invariant against a regression that would
// write state in the construction path.
func TestPurityNoFilesWithoutKeyOrUse(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OIKOS_DATA_DIR", dir)

	_ = New("127.0.0.1:4141") // construct only; no key, no request

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("purity violated: %d files created at %s", len(entries), filepath.Clean(dir))
	}
}
