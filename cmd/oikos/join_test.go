package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"oikos/internal/config"
)

// `oikos join` persists the bus membership (endpoint + zone + key-file ref) so a
// subsequent run is joined. The raw key is never stored — only the key-file path.
func TestRunJoinPersistsMembership(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OIKOS_CONFIG", filepath.Join(dir, "config.json"))

	var out bytes.Buffer
	// --no-verify: this test asserts persistence, not the live key check (that is
	// covered by TestRunJoinValidKeyPersists / TestRunJoinRejectsBadKeyNoPersist).
	err := runJoin([]string{
		"--endpoint", "https://bus.example.com/aibus/events",
		"--zone", "team",
		"--key-file", "/home/u/.config/oikos/keys/x.key",
		"--no-verify",
	}, &out)
	if err != nil {
		t.Fatalf("runJoin: %v", err)
	}
	c, _ := config.Load()
	if c.Bus == nil || c.Bus.URL != "https://bus.example.com/aibus/events" || c.Bus.Zone != "team" {
		t.Fatalf("join not persisted: %+v", c.Bus)
	}
	if c.Bus.KeyFile != "/home/u/.config/oikos/keys/x.key" {
		t.Errorf("key-file not persisted: %q", c.Bus.KeyFile)
	}
}

// `oikos join` without an endpoint is an error (nothing to join).
func TestRunJoinRequiresEndpoint(t *testing.T) {
	t.Setenv("OIKOS_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	var out bytes.Buffer
	if err := runJoin([]string{"--zone", "team"}, &out); err == nil {
		t.Fatal("runJoin with no --endpoint returned nil; want an error")
	}
}

// `oikos leave` clears the membership; a left binary is back to no-bus.
func TestRunLeaveClearsMembership(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OIKOS_CONFIG", filepath.Join(dir, "config.json"))
	_ = config.Save(config.Config{Bus: &config.BusJoin{URL: "http://x", KeyFile: "/k"}})

	var out bytes.Buffer
	if err := runLeave(nil, &out); err != nil {
		t.Fatalf("runLeave: %v", err)
	}
	c, _ := config.Load()
	if c.Bus != nil {
		t.Fatalf("leave did not clear the join: %+v", c.Bus)
	}
	if !strings.Contains(strings.ToLower(out.String()), "left") {
		t.Errorf("leave output %q should confirm leaving", out.String())
	}
}
