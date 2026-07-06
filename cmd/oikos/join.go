package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"oikos/internal/bus"
	"oikos/internal/config"
)

// runJoin implements `oikos join`: OPT-IN, the only way oikos ever talks to a
// bus. It persists the membership (endpoint + zone + key-file reference) — never
// the raw key. For a trusted user the key-file is an existing zone key from
// your key-mint step (e.g. ~/.config/oikos/keys/aibus-clients/<x>.key). The zone is
// informational; the server derives and enforces the real zone from the key.
//
//	oikos join --endpoint https://bus.example.com/aibus/events \
//	           --zone team --key-file ~/.config/oikos/keys/x.key
//
// AIBUS_URL / AIBUS_KEY env always override the stored values at use time (the
// off-tailnet / wrong-zone escape hatch), so a join is a convenience default,
// never a hardcode.
func runJoin(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	fs.SetOutput(out)
	endpoint := fs.String("endpoint", "", "bus endpoint URL (or set AIBUS_URL)")
	zone := fs.String("zone", "", "zone label (informational; the server enforces the real zone from the key)")
	keyFile := fs.String("key-file", "", "path to the existing zone key file (the raw key is never stored in config)")
	noVerify := fs.Bool("no-verify", false, "skip the live key check (offline/air-gapped join)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	url := strings.TrimSpace(*endpoint)
	if url == "" {
		return fmt.Errorf("oikos join: --endpoint is required (the bus URL from your invite / your own bus)")
	}
	kf := strings.TrimSpace(*keyFile)

	// Live-validate the key against the endpoint BEFORE persisting, so a rejected
	// or unreachable credential never becomes a stored (broken) join. The key is
	// loaded from its file (or AIBUS_KEY env) only for the check; the raw key is
	// never written to config — only the file path is. --no-verify opts out for an
	// offline join.
	if !*noVerify {
		key := strings.TrimSpace(os.Getenv("AIBUS_KEY"))
		if key == "" && kf != "" {
			k, err := bus.LoadKey(kf)
			if err != nil {
				return err
			}
			key = k
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := bus.New(bus.Endpoint{URL: url, Key: key}).Verify(ctx); err != nil {
			return err
		}
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg.Bus = &config.BusJoin{URL: url, Zone: strings.TrimSpace(*zone), KeyFile: kf}
	if err := config.Save(cfg); err != nil {
		return err
	}
	z := cfg.Bus.Zone
	if z == "" {
		z = "(zone from key)"
	}
	fmt.Fprintf(out, "oikos: joined %s as %s. `oikos leave` to disconnect.\n", url, z)
	return nil
}

// runLeave implements `oikos leave`: clears the bus membership. oikos is back to
// white/local — no bus, no socket.
func runLeave(args []string, out io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.Bus == nil {
		fmt.Fprintln(out, "oikos: not joined to any bus.")
		return nil
	}
	cfg.Bus = nil
	if err := config.Save(cfg); err != nil {
		return err
	}
	fmt.Fprintln(out, "oikos: left the bus. oikos is local-only again.")
	return nil
}
