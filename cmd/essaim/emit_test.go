package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"essaim/internal/config"
)

// liveRule is a minimal, well-formed live rule the standalone emitter must rank
// into the native-file block. It carries the distinctive body words so a reader
// can assert the block contains the rule's text.
const liveRule = `---
id: use-postgres
title: Use Postgres
kind: guardrail
weight: 0.95
confidence: 0.95
status: live
---
Always use the PostgreSQL database, never MySQL.
`

func seedVault(t *testing.T) string {
	t.Helper()
	vault := t.TempDir()
	if err := os.WriteFile(filepath.Join(vault, "use-postgres.md"), []byte(liveRule), 0o644); err != nil {
		t.Fatalf("seed vault: %v", err)
	}
	return vault
}

// `essaim emit --vault <v> --file claude-code=<path>` regenerates the ranked LIVE
// block into the target native file WITHOUT a proxy running — the standalone path.
func TestRunEmitStandaloneWritesBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(home, "config.json"))
	t.Setenv("ESSAIM_VAULT", "")
	t.Setenv("ESSAIM_NATIVE_FILE_TOOLS", "")

	vault := seedVault(t)
	target := filepath.Join(home, "AGENTS.md")
	// Pre-existing user content the emitter must preserve.
	if err := os.WriteFile(target, []byte("# My Project\n\nUser content essaim must never touch.\n"), 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	var out bytes.Buffer
	if err := runEmit([]string{"--vault", vault, "--file", "claude-code=" + target}, &out); err != nil {
		t.Fatalf("runEmit: %v", err)
	}

	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	got := string(raw)
	for _, want := range []string{
		"<!-- essaim:rules:begin",
		"Always use the PostgreSQL database, never MySQL.",
		"User content essaim must never touch.", // user content preserved
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("emitted file missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(out.String(), target) {
		t.Fatalf("emit output should name the file it wrote; got:\n%s", out.String())
	}
}

// With no --file flag and no ESSAIM_NATIVE_FILE_TOOLS, `essaim emit` falls back to
// the persisted config's native_file wired tools.
func TestRunEmitUsesConfiguredTools(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(home, "config.json"))
	t.Setenv("ESSAIM_VAULT", "")
	t.Setenv("ESSAIM_NATIVE_FILE_TOOLS", "")

	vault := seedVault(t)
	target := filepath.Join(home, "CLAUDE.md")

	// Persist a config that names the vault and a native_file tool.
	cfg := config.Config{VaultDir: vault}
	cfg = cfg.UpsertTool(config.WiredTool{Name: "claude-code", Channel: "native_file", NativeFile: target})
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	var out bytes.Buffer
	if err := runEmit(nil, &out); err != nil {
		t.Fatalf("runEmit: %v", err)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !strings.Contains(string(raw), "Always use the PostgreSQL database, never MySQL.") {
		t.Fatalf("config-driven emit did not write the live block:\n%s", raw)
	}
}

// `essaim emit` with no native-file targets at all is a clear, non-crashing error
// (nothing to write into) rather than a silent no-op.
func TestRunEmitNoTargetsErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(home, "config.json"))
	t.Setenv("ESSAIM_VAULT", "")
	t.Setenv("ESSAIM_NATIVE_FILE_TOOLS", "")

	vault := seedVault(t)
	var out bytes.Buffer
	err := runEmit([]string{"--vault", vault}, &out)
	if err == nil {
		t.Fatalf("emit with no targets must error, got nil (output: %q)", out.String())
	}
}

// Pre-public hardening (review follow-up #3): "first non-empty wins". A SET but
// malformed ESSAIM_NATIVE_FILE_TOOLS must NOT silently fall through to the config
// target — the env var was non-empty, so it WINS; since it parses to no valid
// target, that is an explicit error, not a silent config write.
func TestRunEmitMalformedEnvErrorsNotFallthrough(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(home, "config.json"))
	t.Setenv("ESSAIM_VAULT", "")

	vault := seedVault(t)
	cfgTarget := filepath.Join(home, "CONFIG_TARGET.md")

	// A config target exists and WOULD be used if env fell through.
	cfg := config.Config{VaultDir: vault}
	cfg = cfg.UpsertTool(config.WiredTool{Name: "claude-code", Channel: "native_file", NativeFile: cfgTarget})
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	// Non-empty but malformed env (no `name=path` pair parses).
	t.Setenv("ESSAIM_NATIVE_FILE_TOOLS", "garbage")

	var out bytes.Buffer
	err := runEmit([]string{"--vault", vault}, &out)
	if err == nil {
		t.Fatalf("malformed ESSAIM_NATIVE_FILE_TOOLS must error (first non-empty wins), got nil (output: %q)", out.String())
	}
	if !strings.Contains(err.Error(), "ESSAIM_NATIVE_FILE_TOOLS") {
		t.Fatalf("error should name the offending env var; got: %v", err)
	}
	// And it must NOT have silently written the config target.
	if _, statErr := os.Stat(cfgTarget); statErr == nil {
		t.Fatalf("malformed env must not fall through to the config target (%s was written)", cfgTarget)
	}
}

// A WELL-FORMED ESSAIM_NATIVE_FILE_TOOLS still wins over config (the happy path of
// "first non-empty wins" — proves the malformed case is the only one that errors).
func TestRunEmitWellFormedEnvWinsOverConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(home, "config.json"))
	t.Setenv("ESSAIM_VAULT", "")

	vault := seedVault(t)
	envTarget := filepath.Join(home, "ENV_TARGET.md")
	cfgTarget := filepath.Join(home, "CONFIG_TARGET.md")

	cfg := config.Config{VaultDir: vault}
	cfg = cfg.UpsertTool(config.WiredTool{Name: "claude-code", Channel: "native_file", NativeFile: cfgTarget})
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	t.Setenv("ESSAIM_NATIVE_FILE_TOOLS", "codex="+envTarget)

	var out bytes.Buffer
	if err := runEmit([]string{"--vault", vault}, &out); err != nil {
		t.Fatalf("runEmit: %v", err)
	}
	if _, statErr := os.Stat(envTarget); statErr != nil {
		t.Fatalf("env target should have been written: %v", statErr)
	}
	if _, statErr := os.Stat(cfgTarget); statErr == nil {
		t.Fatalf("config target must NOT be written when env wins")
	}
}

// Pre-public hardening (review follow-up #4): the runEmit summary must reflect the
// ACTUAL per-target outcome. A target whose path itself contains a credential
// pattern is REFUSED (B-7 path refusal) and must be reported as refused — NOT
// "wrote N rules".
func TestRunEmitSummaryReportsRefusedTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(home, "config.json"))
	t.Setenv("ESSAIM_VAULT", "")
	t.Setenv("ESSAIM_NATIVE_FILE_TOOLS", "")

	vault := seedVault(t)
	// A path that trips the credential pattern (an embedded AKIA access key id).
	badPath := filepath.Join(home, "AKIAIOSFODNN7EXAMPLE.md")

	var out bytes.Buffer
	// runEmit returns nil (refusal is not a write failure), but the summary must
	// say REFUSED and must NOT claim it wrote rules.
	_ = runEmit([]string{"--vault", vault, "--file", "codex=" + badPath}, &out)
	s := out.String()
	if !strings.Contains(s, "REFUSED") {
		t.Fatalf("summary must report the refused target; got:\n%s", s)
	}
	if strings.Contains(s, "wrote") {
		t.Fatalf("summary must NOT claim it wrote a refused target; got:\n%s", s)
	}
	if _, statErr := os.Stat(badPath); statErr == nil {
		t.Fatalf("a credential-pathed target must never be written")
	}
}

// `essaim emit` is idempotent: a second run over an unchanged vault leaves the
// target byte-identical (it must not append a second block or duplicate content).
func TestRunEmitIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(home, "config.json"))
	t.Setenv("ESSAIM_VAULT", "")
	t.Setenv("ESSAIM_NATIVE_FILE_TOOLS", "")

	vault := seedVault(t)
	target := filepath.Join(home, "AGENTS.md")

	var out bytes.Buffer
	args := []string{"--vault", vault, "--file", "codex=" + target}
	if err := runEmit(args, &out); err != nil {
		t.Fatalf("runEmit 1: %v", err)
	}
	first, _ := os.ReadFile(target)
	if err := runEmit(args, &out); err != nil {
		t.Fatalf("runEmit 2: %v", err)
	}
	second, _ := os.ReadFile(target)
	if !bytes.Equal(first, second) {
		t.Fatalf("emit is not idempotent:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}
