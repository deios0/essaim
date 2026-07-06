package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// installPS1 resolves scripts/install.ps1 relative to this test file.
func installPS1(t *testing.T) string {
	t.Helper()
	// cmd/essaim → repo root is two levels up.
	p, err := filepath.Abs(filepath.Join("..", "..", "scripts", "install.ps1"))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("install.ps1 not found at %s: %v", p, err)
	}
	return p
}

func ps1Body(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(installPS1(t))
	if err != nil {
		t.Fatalf("read install.ps1: %v", err)
	}
	return string(b)
}

// pwshPath finds a PowerShell executable (pwsh on cross-platform, powershell on
// older Windows), or "" when none is available.
func pwshPath() string {
	for _, name := range []string{"pwsh", "powershell"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// runPS1DryRun executes the installer in dry-run mode under PowerShell when one
// is available; skips otherwise (parity with install_sh_test's no-sh skip). The
// static-invariant tests below always run, so the script's purity is checked on
// every host even without PowerShell.
func runPS1DryRun(t *testing.T, env ...string) string {
	t.Helper()
	pwsh := pwshPath()
	if pwsh == "" {
		t.Skip("no PowerShell (pwsh/powershell) available; static invariants still checked")
	}
	cmd := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-File", installPS1(t))
	cmd.Env = append(os.Environ(), "ESSAIM_INSTALL_DRYRUN=1")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.ps1 dry-run failed: %v\n%s", err, out)
	}
	return string(out)
}

func TestPS1DryRunResolvesWindowsAmd64URL(t *testing.T) {
	out := runPS1DryRun(t, "ESSAIM_OS=windows", "ESSAIM_ARCH=amd64", "ESSAIM_VERSION=latest")
	if !strings.Contains(out, "essaim_windows_amd64.exe") {
		t.Fatalf("dry-run should resolve the windows/amd64 .exe asset:\n%s", out)
	}
	if !strings.Contains(out, "/latest/download/") {
		t.Fatalf("dry-run latest should use the /latest/download/ path:\n%s", out)
	}
	if !strings.Contains(out, "127.0.0.1:4141/setup") {
		t.Fatalf("dry-run should print the optional live-mode setup URL:\n%s", out)
	}
	if !strings.Contains(out, "essaim emit") {
		t.Fatalf("dry-run should lead with the standalone emit step (essaim emit):\n%s", out)
	}
}

func TestPS1DryRunArm64AndPinnedVersion(t *testing.T) {
	out := runPS1DryRun(t, "ESSAIM_OS=windows", "ESSAIM_ARCH=arm64", "ESSAIM_VERSION=v9.9.9")
	if !strings.Contains(out, "essaim_windows_arm64.exe") {
		t.Fatalf("dry-run should resolve the windows/arm64 asset:\n%s", out)
	}
	if !strings.Contains(out, "/download/v9.9.9/") {
		t.Fatalf("a pinned version should use /download/<version>/:\n%s", out)
	}
}

// TestPS1DryRunWritesNothing asserts the purity invariant: a dry run touches no
// filesystem state even when an install dir is provided.
func TestPS1DryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "would-be-install")
	// OS/ARCH are forced so the test is host-independent: PROCESSOR_ARCHITECTURE
	// is unset on the Linux CI host where pwsh runs cross-platform.
	_ = runPS1DryRun(t, "ESSAIM_OS=windows", "ESSAIM_ARCH=amd64", "ESSAIM_INSTALL_DIR="+target)
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("dry run must not create the install dir %s (stat err=%v)", target, err)
	}
}

// --- static-source invariants (always run, even with no PowerShell) ----------

// TestPS1NeverElevates asserts the no-admin purity invariant by source: the
// installer must not run elevated, install a service, or write to a machine-wide
// location. This is the .ps1 analog of install.sh's "never sudos" invariant.
func TestPS1NeverElevates(t *testing.T) {
	body := strings.ToLower(ps1Body(t))
	forbidden := []string{
		"start-process", // could be used to relaunch elevated
		"runas",         // explicit elevation verb
		"-verb",         // Start-Process -Verb RunAs
		"new-service",   // service install
		"set-service",   // service control
		"sc.exe",        // service control CLI
		"install-windowsfeature",
		"requireadministrator", // a #Requires -RunAsAdministrator directive
	}
	for _, f := range forbidden {
		if strings.Contains(body, f) {
			t.Fatalf("install.ps1 must never elevate / install a service; found %q", f)
		}
	}
}

// TestPS1IsProxyAware asserts corporate-proxy awareness: the script must consult
// HTTPS_PROXY/HTTP_PROXY and pass a -Proxy to its web requests.
func TestPS1IsProxyAware(t *testing.T) {
	body := ps1Body(t)
	if !strings.Contains(body, "HTTPS_PROXY") || !strings.Contains(body, "HTTP_PROXY") {
		t.Fatal("install.ps1 must honor HTTPS_PROXY/HTTP_PROXY for corp-proxy hosts")
	}
	if !strings.Contains(body, "Proxy") {
		t.Fatal("install.ps1 must pass a -Proxy to Invoke-WebRequest")
	}
}

// TestPS1SupportsDryRun asserts the offline-testable dry-run gate exists.
func TestPS1SupportsDryRun(t *testing.T) {
	if !strings.Contains(ps1Body(t), "ESSAIM_INSTALL_DRYRUN") {
		t.Fatal("install.ps1 must support ESSAIM_INSTALL_DRYRUN (offline-testable)")
	}
}

// TestPS1UsesNoAdminLocalAppData asserts the default install dir is the no-admin
// %LOCALAPPDATA%\Programs\essaim location, not a machine-wide Program Files dir.
func TestPS1UsesNoAdminLocalAppData(t *testing.T) {
	body := ps1Body(t)
	if !strings.Contains(body, "LOCALAPPDATA") {
		t.Fatal("install.ps1 default install dir must be under %LOCALAPPDATA% (no admin)")
	}
	if !strings.Contains(body, "Programs") {
		t.Fatal("install.ps1 should place the binary under a Programs subdir")
	}
	if strings.Contains(strings.ToLower(body), "program files") {
		t.Fatal("install.ps1 must not target Program Files (that needs admin)")
	}
}

// TestPS1VerifiesChecksumWhenPresent asserts SHA256 verification is wired (and
// fatal on a real mismatch), mirroring install.sh's verify_checksum.
func TestPS1VerifiesChecksumWhenPresent(t *testing.T) {
	body := ps1Body(t)
	if !strings.Contains(body, "SHA256SUMS") {
		t.Fatal("install.ps1 must verify against SHA256SUMS when present")
	}
	if !strings.Contains(body, "Get-FileHash") {
		t.Fatal("install.ps1 must compute the file hash via Get-FileHash")
	}
	if !strings.Contains(strings.ToLower(body), "checksum mismatch") {
		t.Fatal("install.ps1 must refuse to install on a checksum mismatch")
	}
}
