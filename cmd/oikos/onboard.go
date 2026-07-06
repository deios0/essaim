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

	"oikos/internal/brain"
	"oikos/internal/bus"
	"oikos/internal/config"
)

// runOnboard is the ONE-command setup for a new participant: join the bus (and/or
// a Brain rule store), pull the zone's Brain rules, and emit everything into the
// native file(s) — in a single command. A project may be in the bus, in the
// Brain, or both (at least one). Bridge is intentionally NOT part of this — it is
// wired separately as an MCP, and may be used on any project.
//
// It is TRANSACTIONAL: every network validation (bus key, Brain pull) runs FIRST,
// and persistent state (config + the _brain mirror) is written ONCE, only after
// all validations succeed. The saved config reflects EXACTLY the requested
// end-state — bus iff --endpoint, Brain iff --brain-endpoint — so a rerun cleanly
// migrates a project between bus/Brain/both, and a transient failure never leaves
// a half-configured install or repoints an existing vault.
//
//	oikos onboard [--endpoint <bus> --key-file <k>] \
//	              [--brain-endpoint <b> --brain-key-file <bk>] \
//	              [--zone <z>] [--project <p>] [--vault <dir>] \
//	              --file <tool>=<path> [--file ...]
func runOnboard(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("onboard", flag.ContinueOnError)
	fs.SetOutput(out)
	endpoint := fs.String("endpoint", "", "bus endpoint URL (omit for a Brain-only project)")
	keyFile := fs.String("key-file", "", "bus zone key file")
	zone := fs.String("zone", "", "optional zone nickname (server enforces the real zone from the key)")
	brainEndpoint := fs.String("brain-endpoint", "", "Brain (rule-store) URL (omit for a bus-only project)")
	brainKeyFile := fs.String("brain-key-file", "", "Brain zone key file")
	project := fs.String("project", "", "project tag for Brain rules (default: current directory name)")
	vaultFlag := fs.String("vault", "", "vault directory (default: OIKOS_VAULT / configured vault / ~/oikos-vault)")
	noVerify := fs.Bool("no-verify", false, "skip the live key check (offline join)")
	var fileFlags stringSlice
	fs.Var(&fileFlags, "file", "native-file target as name=path (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	busURL := strings.TrimSpace(*endpoint)
	brainURL := strings.TrimSpace(*brainEndpoint)
	brainKF := strings.TrimSpace(*brainKeyFile)
	zoneLabel := strings.TrimSpace(*zone)
	// Per-project flexibility: a project may be in the bus, in the Brain, or both.
	if busURL == "" && brainURL == "" {
		return fmt.Errorf("oikos onboard: give at least one of --endpoint (bus) or --brain-endpoint (rule store)")
	}

	// Resolve native-file targets up front (before any network work or commit).
	// onboard REQUIRES an explicit, fully-valid --file set: it must not fall back to
	// the global config/env target list (which can hold OTHER projects' files, so
	// this project's zone rules would land in unrelated files), and every entry must
	// parse (a partially-malformed list must not silently drop a mistyped target).
	// Absolute paths so a later emit/serve from another directory keeps the file.
	if len(fileFlags) == 0 {
		return fmt.Errorf("oikos onboard: at least one --file name=path is required (e.g. --file claude-code=./CLAUDE.md)")
	}
	type fileTarget struct{ name, path string }
	var fileTools []fileTarget
	for _, f := range fileFlags {
		name, path, ok := strings.Cut(strings.TrimSpace(f), "=")
		name, path = strings.TrimSpace(name), strings.TrimSpace(path)
		if !ok || name == "" || path == "" {
			return fmt.Errorf("oikos onboard: malformed --file %q (want name=path, e.g. claude-code=./CLAUDE.md)", f)
		}
		if abs, aerr := filepath.Abs(path); aerr == nil {
			path = abs
		}
		fileTools = append(fileTools, fileTarget{name: name, path: path})
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Resolve the vault (absolute — a relative path saved verbatim would resolve
	// differently when serve/sync run from another directory). Create the dir now
	// (harmless on later failure); config.VaultDir is only PERSISTED at the end.
	vault := firstNonEmpty(strings.TrimSpace(*vaultFlag), strings.TrimSpace(os.Getenv("OIKOS_VAULT")), strings.TrimSpace(cfg.VaultDir))
	if vault == "" {
		if home, herr := os.UserHomeDir(); herr == nil {
			vault = filepath.Join(home, "oikos-vault")
		}
	}
	if vault == "" {
		return fmt.Errorf("oikos onboard: could not resolve a vault directory — pass --vault")
	}
	if abs, aerr := filepath.Abs(vault); aerr == nil {
		vault = abs
	}
	if err := os.MkdirAll(vault, 0o755); err != nil {
		return err
	}

	// ---- VALIDATE (network) FIRST — persist nothing until all of this passes ----

	var busJoin *config.BusJoin
	if busURL != "" {
		busZone := zoneLabel
		if !*noVerify {
			key := strings.TrimSpace(os.Getenv("AIBUS_KEY"))
			if key == "" {
				if kf := strings.TrimSpace(*keyFile); kf != "" {
					k, kerr := bus.LoadKey(kf)
					if kerr != nil {
						return kerr
					}
					key = k
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			client := bus.New(bus.Endpoint{URL: busURL, Key: key})
			if verr := client.Verify(ctx); verr != nil {
				cancel()
				return verr
			}
			if sz, zerr := client.Zone(ctx); zerr == nil && sz != "" {
				busZone = sz // server truth over the client label
			}
			cancel()
		}
		busJoin = &config.BusJoin{URL: busURL, Zone: busZone, KeyFile: absPathOrRaw(strings.TrimSpace(*keyFile))}
	}

	var brainJoin *config.BrainJoin
	var brainRules []brain.Rule
	if brainURL != "" {
		key := ""
		if brainKF != "" {
			k, kerr := brain.LoadKey(brainKF)
			if kerr != nil {
				return kerr
			}
			key = k
		}
		if env := strings.TrimSpace(os.Getenv("BRAIN_KEY")); env != "" && brainKF == "" {
			key = env
		}
		proj := strings.TrimSpace(*project)
		if proj == "" {
			if wd, werr := os.Getwd(); werr == nil {
				proj = filepath.Base(wd)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		rs, perr := brain.New(brain.Endpoint{URL: brainURL, Key: key, Zone: zoneLabel}).Pull(ctx, proj)
		cancel()
		if perr != nil {
			return perr
		}
		brainRules = rs
		brainJoin = &config.BrainJoin{URL: brainURL, Zone: zoneLabel, KeyFile: absPathOrRaw(brainKF)}
	}

	// ---- COMMIT — everything validated; do the file side, THEN persist config ----

	// The _brain mirror mirrors EXACTLY the requested Brain (or is cleared when the
	// project has no Brain), so a rerun never leaves another project's zone rules.
	if err := brain.WriteVault(vault, brainRules); err != nil {
		return err
	}

	// Emit into the EXACT --file targets (never the global wired-tool list — this
	// project's zone rules must not land in another project's files). This is the
	// first write; do it BEFORE saving the join config so a refused/unwritable path
	// fails the whole onboard cleanly, leaving no half-saved join to retry against.
	emitArgs := []string{"--vault", vault}
	for _, t := range fileTools {
		emitArgs = append(emitArgs, "--file", t.name+"="+t.path)
	}
	if err := runEmit(emitArgs, out); err != nil {
		return err
	}

	// Emit succeeded → persist the validated end-state. Reload first so a concurrent
	// change during the network phase is not clobbered by a stale snapshot; write
	// EXACTLY this onboard's membership (bus iff --endpoint, Brain iff
	// --brain-endpoint) so a rerun cleanly migrates a project between bus/Brain/both.
	// Native-file targets are intentionally NOT auto-wired here — persistent wiring
	// (with its channel/identity normalization + unwire support) is `oikos wire`.
	cfg, err = config.Load()
	if err != nil {
		return err
	}
	cfg.VaultDir = vault
	cfg.Bus = busJoin
	cfg.Brain = brainJoin
	if err := config.Save(cfg); err != nil {
		return err
	}

	if busJoin != nil {
		z := busJoin.Zone
		if z == "" {
			z = "the zone your key enforces"
		}
		fmt.Fprintf(out, "oikos: joined bus %s (%s, server-enforced).\n", busURL, z)
	}
	if brainJoin != nil {
		fmt.Fprintf(out, "oikos: pulled %d Brain zone rule(s) into the vault mirror.\n", len(brainRules))
	}
	fmt.Fprintln(out, "oikos: onboarded. To keep these files updated automatically, run `oikos wire <tool>`.")
	fmt.Fprintln(out, "(Bridge, if you use it, is wired separately as an MCP — it can be used on any project.)")
	return nil
}

// absPathOrRaw returns the absolute form of a non-empty path (so a stored key-file
// path stays valid when a later command runs from another directory); an empty
// string, or a path that cannot be absolutized, is returned unchanged.
func absPathOrRaw(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
