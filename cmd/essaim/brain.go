package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"essaim/internal/brain"
	"essaim/internal/config"
)

// runBrain implements `essaim brain <pull>`: pull the zone's shared rules from a
// joined Brain into the vault's managed _brain/ mirror, so the next `essaim emit`
// writes them into your native files alongside your own rules. Network + opt-in;
// the offline `essaim emit` stays network-free.
//
//	essaim brain pull [--project <p>] [--vault <dir>]
func runBrain(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "pull" {
		return fmt.Errorf("usage: essaim brain pull [--project <p>] [--vault <dir>]")
	}
	fs := flag.NewFlagSet("brain pull", flag.ContinueOnError)
	fs.SetOutput(out)
	project := fs.String("project", "", "project tag to resolve rules for (default: the current directory name)")
	vaultFlag := fs.String("vault", "", "vault directory to mirror into (default: ESSAIM_VAULT / configured vault)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// Resolve the Brain endpoint+key: env overrides, then the stored join.
	url := strings.TrimSpace(os.Getenv("BRAIN_URL"))
	key := strings.TrimSpace(os.Getenv("BRAIN_KEY"))
	zone := ""
	if cfg.Brain != nil {
		if url == "" {
			url = cfg.Brain.URL
		}
		zone = cfg.Brain.Zone
		if key == "" && cfg.Brain.KeyFile != "" {
			if k, err := brain.LoadKey(cfg.Brain.KeyFile); err == nil {
				key = k
			}
		}
	}
	if url == "" {
		return fmt.Errorf("essaim brain: not joined to a Brain — run `essaim join --brain-endpoint <url> --brain-key-file <path>` (or set BRAIN_URL/BRAIN_KEY)")
	}

	// Resolve the vault: flag, env, config, else ~/essaim-vault (same as emit).
	vault := strings.TrimSpace(*vaultFlag)
	if vault == "" {
		vault = strings.TrimSpace(os.Getenv("ESSAIM_VAULT"))
	}
	if vault == "" {
		vault = strings.TrimSpace(cfg.VaultDir)
	}
	if vault == "" {
		if home, err := os.UserHomeDir(); err == nil {
			vault = filepath.Join(home, "essaim-vault")
		}
	}
	if vault == "" {
		return fmt.Errorf("essaim brain: no vault — pass --vault, set ESSAIM_VAULT, or run `essaim init`")
	}

	proj := strings.TrimSpace(*project)
	if proj == "" {
		if wd, err := os.Getwd(); err == nil {
			proj = filepath.Base(wd)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rs, err := brain.New(brain.Endpoint{URL: url, Key: key, Zone: zone}).Pull(ctx, proj)
	if err != nil {
		return err
	}
	if err := brain.WriteVault(vault, rs); err != nil {
		return err
	}
	z := zone
	if z == "" {
		z = "your zone"
	}
	fmt.Fprintf(out, "essaim: pulled %d %s rule(s) into the vault mirror. Run `essaim emit` to write them into your files.\n", len(rs), z)
	return nil
}
