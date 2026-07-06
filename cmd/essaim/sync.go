package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"essaim/internal/config"
	syncpkg "essaim/internal/sync"
)

// runSync implements `essaim sync`: it pushes/pulls the rule vault to the user's
// own git remote and merges it deterministically (no rule lost). It is OPTIONAL,
// local, and $0 — the only egress is to the user's own remote, there is no essaim
// server. It is the M4 sync PRIMITIVE the future paid Team-Rule-Sync drops into.
//
//	essaim sync --remote git@github.com:you/rules.git
//	essaim sync --remote <url> --branch main --vault <dir>
//
// The vault defaults to the configured vault (the /setup choice) or ESSAIM_VAULT;
// the remote can be supplied with --remote or ESSAIM_SYNC_REMOTE.
func runSync(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(out)
	remote := fs.String("remote", "", "git remote URL to sync the vault to (or set ESSAIM_SYNC_REMOTE)")
	branch := fs.String("branch", "main", "remote branch")
	vault := fs.String("vault", "", "vault directory to sync (default: the configured vault / ESSAIM_VAULT)")
	message := fs.String("message", "essaim sync: rule vault", "commit message")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Resolve the remote: flag wins, then env.
	rem := strings.TrimSpace(*remote)
	if rem == "" {
		rem = strings.TrimSpace(os.Getenv("ESSAIM_SYNC_REMOTE"))
	}
	if rem == "" {
		return fmt.Errorf("essaim sync: no git remote — pass --remote <url> or set ESSAIM_SYNC_REMOTE\n" +
			"  e.g. essaim sync --remote git@github.com:you/essaim-rules.git")
	}

	// Resolve the vault: flag wins, then ESSAIM_VAULT, then the persisted config.
	vaultDir := strings.TrimSpace(*vault)
	if vaultDir == "" {
		vaultDir = strings.TrimSpace(os.Getenv("ESSAIM_VAULT"))
	}
	if vaultDir == "" {
		if cfg, err := config.Load(); err == nil {
			vaultDir = strings.TrimSpace(cfg.VaultDir)
		}
	}
	if vaultDir == "" {
		return fmt.Errorf("essaim sync: no vault directory — pass --vault <dir>, set ESSAIM_VAULT,\n" +
			"  or choose a vault in the /setup screen first")
	}

	res, err := syncpkg.Sync(syncpkg.Options{
		VaultDir:  vaultDir,
		RemoteURL: rem,
		Branch:    *branch,
		Message:   *message,
	})
	if err != nil {
		return err
	}
	if res.Pushed {
		fmt.Fprintf(out, "essaim: synced %d rules with %s (pushed local changes)\n", res.Merged, rem)
	} else {
		fmt.Fprintf(out, "essaim: synced %d rules with %s (already up to date)\n", res.Merged, rem)
	}
	// P0 quarantine: incoming remote rules are NOT auto-applied — they land in
	// _inbox/ as drafts the user must explicitly review/accept. Surface the count
	// so the user knows there is inbound to review (a remote rule never silently
	// becomes a live, injectable rule).
	if res.Quarantined > 0 {
		fmt.Fprintf(out, "essaim: %d incoming remote rule(s) quarantined as drafts in _inbox/ "+
			"— review and accept before they take effect (none are active until you do)\n", res.Quarantined)
	}
	return nil
}
