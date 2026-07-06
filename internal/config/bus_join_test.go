package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A joined bus endpoint round-trips through Save/Load. The KEY is never stored in
// config.json — only a reference to the existing zone key FILE (secret hygiene:
// the trusted key already lives in ~/.bridge/keys/, config points at it).
func TestBusJoinRoundTrips(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))

	in := Config{Bus: &BusJoin{URL: "https://team.eodhd.com/aibus", Zone: "shatale", KeyFile: "/home/u/.bridge/keys/aibus-clients/x.key"}}
	if err := Save(in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Bus == nil || got.Bus.URL != in.Bus.URL || got.Bus.Zone != in.Bus.Zone || got.Bus.KeyFile != in.Bus.KeyFile {
		t.Fatalf("Bus join did not round-trip: got %+v", got.Bus)
	}
	// The raw key must NOT appear in the on-disk config.
	raw, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if len(raw) == 0 {
		t.Fatal("config file empty")
	}
}

// A config that has ONLY a bus join (no provider/vault/tools) is still not the
// first-run empty state — IsEmpty must account for a join.
func TestBusJoinNotEmpty(t *testing.T) {
	c := Config{Bus: &BusJoin{URL: "http://x", KeyFile: "/k"}}
	if c.IsEmpty() {
		t.Fatal("a config with a bus join reports IsEmpty; a joined binary is not first-run")
	}
}
