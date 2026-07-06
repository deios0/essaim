package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"essaim/internal/config"
)

// `essaim onboard` is the one-command setup: join bus (+optional Brain), pull the
// zone's Brain rules, and emit into the native file(s) — all in one go. Verifies
// the whole chain against fake bus+brain servers writing a real AGENTS.md.
func TestRunOnboardChainsJoinPullEmit(t *testing.T) {
	bus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"events":[{"id":9,"zone":"business","kind":"x"}]}`))
	}))
	defer bus.Close()
	brain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"rules":[{"id":"r1","title":"Use PG","body":"always postgres"}]}`))
	}))
	defer brain.Close()

	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))
	t.Setenv("ESSAIM_VAULT", filepath.Join(dir, "vault"))
	_ = os.MkdirAll(filepath.Join(dir, "vault"), 0o755)
	kf := filepath.Join(dir, "k")
	_ = os.WriteFile(kf, []byte("key\n"), 0o600)
	target := filepath.Join(dir, "AGENTS.md")
	_ = os.WriteFile(target, []byte("# mine\n"), 0o644)

	var out bytes.Buffer
	err := runOnboard([]string{
		"--endpoint", bus.URL, "--key-file", kf,
		"--brain-endpoint", brain.URL, "--brain-key-file", kf,
		"--project", "eodhd",
		"--file", "codex=" + target,
	}, &out)
	if err != nil {
		t.Fatalf("runOnboard: %v", err)
	}
	got, _ := os.ReadFile(target)
	s := string(got)
	if !strings.Contains(s, "essaim:rules:begin") {
		t.Fatalf("target has no essaim block:\n%s", s)
	}
	if !strings.Contains(s, "always postgres") {
		t.Fatalf("brain zone rule not emitted into the target:\n%s", s)
	}
	if !strings.Contains(s, "# mine") {
		t.Fatalf("user content not preserved:\n%s", s)
	}
}

// onboard without a bus endpoint errors (nothing to set up).
func TestRunOnboardRequiresEndpoint(t *testing.T) {
	t.Setenv("ESSAIM_CONFIG", filepath.Join(t.TempDir(), "c.json"))
	var out bytes.Buffer
	if err := runOnboard([]string{"--file", "codex=/tmp/x"}, &out); err == nil {
		t.Fatal("onboard with no --endpoint returned nil; want an error")
	}
}

// Per-project flexibility: a project can be in the Brain but NOT the bus. onboard
// with only --brain-endpoint (no --endpoint) must join Brain, pull, and emit.
func TestRunOnboardBrainOnly(t *testing.T) {
	brain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"rules":[{"id":"r1","title":"Rule","body":"brain only rule"}]}`))
	}))
	defer brain.Close()

	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))
	t.Setenv("ESSAIM_VAULT", filepath.Join(dir, "vault"))
	_ = os.MkdirAll(filepath.Join(dir, "vault"), 0o755)
	kf := filepath.Join(dir, "k")
	_ = os.WriteFile(kf, []byte("key\n"), 0o600)
	target := filepath.Join(dir, "AGENTS.md")
	_ = os.WriteFile(target, []byte("# mine\n"), 0o644)

	var out bytes.Buffer
	err := runOnboard([]string{
		"--brain-endpoint", brain.URL, "--brain-key-file", kf,
		"--project", "p", "--file", "codex=" + target,
	}, &out)
	if err != nil {
		t.Fatalf("brain-only onboard: %v", err)
	}
	got, _ := os.ReadFile(target)
	if !strings.Contains(string(got), "brain only rule") {
		t.Fatalf("brain-only rule not emitted:\n%s", string(got))
	}
}

// onboard with NEITHER bus nor brain errors.
func TestRunOnboardRequiresBusOrBrain(t *testing.T) {
	t.Setenv("ESSAIM_CONFIG", filepath.Join(t.TempDir(), "c.json"))
	var out bytes.Buffer
	if err := runOnboard([]string{"--file", "codex=/tmp/x"}, &out); err == nil {
		t.Fatal("onboard with no bus and no brain returned nil; want an error")
	}
}

// P1 regression: on a FRESH machine (no vault configured, no ~/essaim-vault),
// bus-only onboard must still create+record a vault and emit without failing.
func TestRunOnboardFreshMachineNoVault(t *testing.T) {
	bus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"events":[{"id":1,"zone":"business"}]}`))
	}))
	defer bus.Close()

	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))
	t.Setenv("HOME", dir)        // ~/essaim-vault resolves under the temp HOME
	t.Setenv("ESSAIM_VAULT", "") // nothing preconfigured
	kf := filepath.Join(dir, "k")
	_ = os.WriteFile(kf, []byte("k\n"), 0o600)
	target := filepath.Join(dir, "AGENTS.md")
	_ = os.WriteFile(target, []byte("# mine\n"), 0o644)

	var out bytes.Buffer
	if err := runOnboard([]string{"--endpoint", bus.URL, "--key-file", kf, "--file", "codex=" + target}, &out); err != nil {
		t.Fatalf("fresh-machine bus-only onboard failed: %v", err)
	}
	c, _ := config.Load()
	if c.VaultDir == "" {
		t.Fatal("onboard did not record config.VaultDir on a fresh machine")
	}
	if _, err := os.Stat(c.VaultDir); err != nil {
		t.Fatalf("vault dir not created: %v", err)
	}
}

// P2b regression: a bus-only onboard must CLEAR a stale _brain mirror left by a
// previous Brain-backed onboard, so it never emits another project's zone rules.
func TestRunOnboardBusOnlyClearsStaleBrainMirror(t *testing.T) {
	bus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"events":[]}`))
	}))
	defer bus.Close()

	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))
	vault := filepath.Join(dir, "vault")
	t.Setenv("ESSAIM_VAULT", vault)
	_ = os.MkdirAll(vault, 0o755)
	// Simulate a stale mirror from a previous Brain onboard.
	_ = os.MkdirAll(filepath.Join(vault, "_brain"), 0o755)
	_ = os.WriteFile(filepath.Join(vault, "_brain", "stale.md"), []byte("---\nid: x\nstatus: live\n---\nstale zone rule"), 0o644)
	kf := filepath.Join(dir, "k")
	_ = os.WriteFile(kf, []byte("k\n"), 0o600)
	target := filepath.Join(dir, "AGENTS.md")
	_ = os.WriteFile(target, []byte("# mine\n"), 0o644)

	var out bytes.Buffer
	if err := runOnboard([]string{"--endpoint", bus.URL, "--key-file", kf, "--file", "codex=" + target}, &out); err != nil {
		t.Fatalf("bus-only onboard: %v", err)
	}
	if _, err := os.Stat(filepath.Join(vault, "_brain")); !os.IsNotExist(err) {
		t.Fatal("stale _brain mirror not cleared on bus-only onboard")
	}
	got, _ := os.ReadFile(target)
	if strings.Contains(string(got), "stale zone rule") {
		t.Fatalf("bus-only onboard emitted a stale Brain rule:\n%s", string(got))
	}
}

// P2 regression: a relative --vault must be persisted as an ABSOLUTE path.
func TestRunOnboardPersistsAbsoluteVault(t *testing.T) {
	brainS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"rules":[]}`))
	}))
	defer brainS.Close()
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))
	t.Chdir(dir)
	kf := filepath.Join(dir, "k")
	_ = os.WriteFile(kf, []byte("k\n"), 0o600)
	target := filepath.Join(dir, "AGENTS.md")
	_ = os.WriteFile(target, []byte("# m\n"), 0o644)
	var out bytes.Buffer
	if err := runOnboard([]string{"--brain-endpoint", brainS.URL, "--brain-key-file", kf, "--vault", "relvault", "--file", "codex=" + target}, &out); err != nil {
		t.Fatalf("onboard: %v", err)
	}
	c, _ := config.Load()
	if !filepath.IsAbs(c.VaultDir) {
		t.Fatalf("VaultDir not absolute: %q", c.VaultDir)
	}
}

// P2 regression: a FAILED Brain-only onboard (bad key / unreachable) must leave
// NO stored Brain join.
func TestRunOnboardFailedBrainLeavesNoJoin(t *testing.T) {
	brainS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer brainS.Close()
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))
	t.Setenv("ESSAIM_VAULT", filepath.Join(dir, "vault"))
	kf := filepath.Join(dir, "k")
	_ = os.WriteFile(kf, []byte("bad\n"), 0o600)
	var out bytes.Buffer
	if err := runOnboard([]string{"--brain-endpoint", brainS.URL, "--brain-key-file", kf, "--file", "codex=/tmp/x"}, &out); err == nil {
		t.Fatal("failed Brain onboard returned nil; want error")
	}
	c, _ := config.Load()
	if c.Brain != nil {
		t.Fatalf("failed Brain onboard left a stored join: %+v", c.Brain)
	}
}

// P2 regression: bus-only onboard clears a previously-stored Brain join.
func TestRunOnboardBusOnlyClearsStoredBrainJoin(t *testing.T) {
	busS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"events":[]}`))
	}))
	defer busS.Close()
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))
	t.Setenv("ESSAIM_VAULT", filepath.Join(dir, "vault"))
	_ = os.MkdirAll(filepath.Join(dir, "vault"), 0o755)
	_ = config.Save(config.Config{Brain: &config.BrainJoin{URL: "http://old", KeyFile: "/k"}})
	kf := filepath.Join(dir, "k")
	_ = os.WriteFile(kf, []byte("k\n"), 0o600)
	target := filepath.Join(dir, "AGENTS.md")
	_ = os.WriteFile(target, []byte("# m\n"), 0o644)
	var out bytes.Buffer
	if err := runOnboard([]string{"--endpoint", busS.URL, "--key-file", kf, "--file", "codex=" + target}, &out); err != nil {
		t.Fatalf("bus-only onboard: %v", err)
	}
	c, _ := config.Load()
	if c.Brain != nil {
		t.Fatalf("bus-only onboard did not clear the stored Brain join: %+v", c.Brain)
	}
}

// onboard emits into the exact --file target (first write). Continuous
// maintenance is a separate `essaim wire` step, so onboard does NOT auto-wire
// (avoids reimplementing wire's channel/identity normalization).
func TestRunOnboardEmitsIntoFileTarget(t *testing.T) {
	busS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"events":[]}`))
	}))
	defer busS.Close()
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))
	t.Setenv("ESSAIM_VAULT", filepath.Join(dir, "vault"))
	_ = os.MkdirAll(filepath.Join(dir, "vault"), 0o755)
	kf := filepath.Join(dir, "k")
	_ = os.WriteFile(kf, []byte("k\n"), 0o600)
	target := filepath.Join(dir, "AGENTS.md")
	_ = os.WriteFile(target, []byte("# m\n"), 0o644)
	var out bytes.Buffer
	if err := runOnboard([]string{"--endpoint", busS.URL, "--key-file", kf, "--file", "codex=" + target}, &out); err != nil {
		t.Fatalf("onboard: %v", err)
	}
	got, _ := os.ReadFile(target)
	if !strings.Contains(string(got), "essaim:rules:begin") {
		t.Fatalf("onboard did not emit the block into the target:\n%s", string(got))
	}
	if !strings.Contains(string(got), "# m") {
		t.Fatalf("user content not preserved:\n%s", string(got))
	}
}

// P2 regression: a malformed --file fails EARLY (before any join/commit).
func TestRunOnboardMalformedFileFailsEarly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))
	var out bytes.Buffer
	err := runOnboard([]string{"--endpoint", "http://unused", "--file", "no-equals-sign"}, &out)
	if err == nil {
		t.Fatal("malformed --file returned nil; want an early error")
	}
	c, _ := config.Load()
	if c.Bus != nil {
		t.Fatal("malformed --file still wrote a bus join (should fail before any commit)")
	}
}

// Key-file paths must be stored ABSOLUTE so later bus/brain commands from another
// directory can still read them.
func TestRunOnboardAbsolutizesKeyFile(t *testing.T) {
	busS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"events":[]}`))
	}))
	defer busS.Close()
	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))
	t.Setenv("ESSAIM_VAULT", filepath.Join(dir, "vault"))
	_ = os.MkdirAll(filepath.Join(dir, "vault"), 0o755)
	t.Chdir(dir)
	_ = os.WriteFile(filepath.Join(dir, "z.key"), []byte("k\n"), 0o600)
	target := filepath.Join(dir, "AGENTS.md")
	_ = os.WriteFile(target, []byte("# m\n"), 0o644)
	var out bytes.Buffer
	if err := runOnboard([]string{"--endpoint", busS.URL, "--key-file", "z.key", "--file", "codex=" + target}, &out); err != nil {
		t.Fatalf("onboard: %v", err)
	}
	c, _ := config.Load()
	if c.Bus == nil || !filepath.IsAbs(c.Bus.KeyFile) {
		t.Fatalf("bus key-file not absolute: %+v", c.Bus)
	}
}
