package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"oikos/internal/config"
)

// `oikos init` seeds the vault + the demo rule, records the vault in config, and
// prints the copy-paste prompt with the undeniable before/after.
func TestRunInitSeedsAndPrintsDemo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("OIKOS_CONFIG", filepath.Join(home, "config.json"))
	vault := filepath.Join(home, "vault")

	var out bytes.Buffer
	if err := runInit([]string{"--vault", vault}, &out); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	s := out.String()
	for _, want := range []string{"create a Card component", "card.js", "Card.js"} {
		if !strings.Contains(s, want) {
			t.Fatalf("init output missing %q:\n%s", want, s)
		}
	}
	c, _ := config.Load()
	if c.VaultDir != vault {
		t.Fatalf("init must record the vault in config: got %q want %q", c.VaultDir, vault)
	}
}

// `oikos init` is idempotent — running it twice leaves exactly one starter rule.
func TestRunInitIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("OIKOS_CONFIG", filepath.Join(home, "config.json"))
	vault := filepath.Join(home, "vault")

	var out bytes.Buffer
	if err := runInit([]string{"--vault", vault}, &out); err != nil {
		t.Fatalf("runInit 1: %v", err)
	}
	if err := runInit([]string{"--vault", vault}, &out); err != nil {
		t.Fatalf("runInit 2: %v", err)
	}
	// Count .md files in the vault — must be exactly one.
	matches, _ := filepath.Glob(filepath.Join(vault, "*.md"))
	if len(matches) != 1 {
		t.Fatalf("init is not idempotent: %d rule files in vault, want 1: %v", len(matches), matches)
	}
}

// With no --vault and a config already naming a vault, `oikos init` seeds INTO
// the configured vault (it does not silently create a second one).
func TestRunInitUsesConfiguredVault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("OIKOS_CONFIG", filepath.Join(home, "config.json"))
	vault := filepath.Join(home, "configured-vault")

	// Pre-seed the config with a vault.
	if err := config.Save(config.Config{VaultDir: vault}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	var out bytes.Buffer
	if err := runInit(nil, &out); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(vault, "*.md"))
	if len(matches) != 1 {
		t.Fatalf("init must seed into the configured vault; found %d .md files: %v", len(matches), matches)
	}
}
