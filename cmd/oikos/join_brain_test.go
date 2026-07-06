package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"oikos/internal/config"
)

// `oikos join` with --brain-endpoint/--brain-key-file also records the Brain join
// (one command sets up both bus and brain). --no-verify skips the live check.
func TestRunJoinRecordsBrain(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OIKOS_CONFIG", filepath.Join(dir, "config.json"))
	kf := filepath.Join(dir, "b.key")
	_ = os.WriteFile(kf, []byte("bk\n"), 0o600)

	var out bytes.Buffer
	err := runJoin([]string{
		"--endpoint", "https://bus.example.com/aibus/events",
		"--brain-endpoint", "https://brain.example.com/api/brain-<zone>",
		"--brain-key-file", kf,
		"--no-verify",
	}, &out)
	if err != nil {
		t.Fatalf("runJoin: %v", err)
	}
	c, _ := config.Load()
	if c.Brain == nil || c.Brain.URL != "https://brain.example.com/api/brain-<zone>" || c.Brain.KeyFile != kf {
		t.Fatalf("brain join not recorded: %+v", c.Brain)
	}
	if c.Bus == nil {
		t.Fatal("bus join should also be recorded")
	}
}
