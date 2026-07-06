package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// installScript resolves scripts/install.sh relative to this test file.
func installScript(t *testing.T) string {
	t.Helper()
	// cmd/essaim → repo root is two levels up.
	p, err := filepath.Abs(filepath.Join("..", "..", "scripts", "install.sh"))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("install.sh not found at %s: %v", p, err)
	}
	return p
}

func runInstallDryRun(t *testing.T, env ...string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is a POSIX sh script; not run on windows CI")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no POSIX sh available")
	}
	cmd := exec.Command("sh", installScript(t))
	cmd.Env = append(os.Environ(), "ESSAIM_INSTALL_DRYRUN=1")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh dry-run failed: %v\n%s", err, out)
	}
	return string(out)
}

func TestInstallDryRunResolvesLinuxAmd64URL(t *testing.T) {
	out := runInstallDryRun(t, "ESSAIM_OS=linux", "ESSAIM_ARCH=amd64", "ESSAIM_VERSION=latest")
	if !strings.Contains(out, "essaim_linux_amd64") {
		t.Fatalf("dry-run should resolve the linux/amd64 asset:\n%s", out)
	}
	if !strings.Contains(out, "/latest/download/") {
		t.Fatalf("dry-run latest should use the /latest/download/ path:\n%s", out)
	}
	if !strings.Contains(out, "127.0.0.1:4141/setup") {
		t.Fatalf("dry-run should print the optional live-mode setup URL:\n%s", out)
	}
	// Repositioning: the emitter path (essaim emit → AGENTS.md, no proxy) is the
	// first-class next step the installer leads with.
	if !strings.Contains(out, "essaim emit") {
		t.Fatalf("dry-run should lead with the standalone emit step (essaim emit):\n%s", out)
	}
}

func TestInstallDryRunPinnedVersionURL(t *testing.T) {
	out := runInstallDryRun(t, "ESSAIM_OS=darwin", "ESSAIM_ARCH=arm64", "ESSAIM_VERSION=v9.9.9")
	if !strings.Contains(out, "essaim_darwin_arm64") {
		t.Fatalf("dry-run should resolve the darwin/arm64 asset:\n%s", out)
	}
	if !strings.Contains(out, "/download/v9.9.9/") {
		t.Fatalf("a pinned version should use /download/<version>/:\n%s", out)
	}
}

func TestInstallDryRunWindowsAddsExe(t *testing.T) {
	out := runInstallDryRun(t, "ESSAIM_OS=windows", "ESSAIM_ARCH=amd64")
	if !strings.Contains(out, "essaim_windows_amd64.exe") {
		t.Fatalf("windows asset must carry the .exe extension:\n%s", out)
	}
}

// TestInstallDryRunWritesNothing asserts the purity invariant: a dry run touches
// no filesystem state even when an install dir is provided.
func TestInstallDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "would-be-install")
	_ = runInstallDryRun(t, "ESSAIM_INSTALL_DIR="+target)
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("dry run must not create the install dir %s (stat err=%v)", target, err)
	}
}
