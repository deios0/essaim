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
// the raw key. For a trusted user the key-file is an existing zone key. The zone
// is informational; the server derives and enforces the real zone from the key.
// An optional --brain-endpoint/--brain-key-file also joins a zone rule store in
// the same command.
//
//	oikos join --endpoint https://bus.example.com/aibus/events \
//	           --key-file ~/.config/oikos/keys/zone.key \
//	           [--brain-endpoint https://brain.example.com/api/brain-<zone> \
//	            --brain-key-file ~/.config/oikos/keys/brain.key]
//
// AIBUS_URL / AIBUS_KEY env always override the stored values at use time (the
// off-tailnet / wrong-zone escape hatch), so a join is a convenience default,
// never a hardcode.
func runJoin(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	fs.SetOutput(out)
	endpoint := fs.String("endpoint", "", "bus endpoint URL (or set AIBUS_URL)")
	zoneFlag := fs.String("zone", "", "optional zone nickname (informational; the server enforces the real zone from the key)")
	keyFile := fs.String("key-file", "", "path to the existing zone key file (the raw key is never stored in config)")
	noVerify := fs.Bool("no-verify", false, "skip the live key check (offline/air-gapped join)")
	brainEndpoint := fs.String("brain-endpoint", "", "optional Brain (rule-store) URL — also join a zone rule store")
	brainKeyFile := fs.String("brain-key-file", "", "path to the Brain zone key file (raw key never stored)")
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
	//
	// The stored zone is the SERVER-ENFORCED one (read off an event the key may
	// see), NOT the user's --zone label — the label can never misrepresent the
	// real zone the server derives from the key. --zone is only a fallback nickname
	// used when the zone has no events yet to confirm it (or under --no-verify).
	zone := strings.TrimSpace(*zoneFlag)
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
		client := bus.New(bus.Endpoint{URL: url, Key: key})
		if err := client.Verify(ctx); err != nil {
			return err
		}
		if serverZone, err := client.Zone(ctx); err == nil && serverZone != "" {
			zone = serverZone // server truth overrides the client-supplied label
		}
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg.Bus = &config.BusJoin{URL: url, Zone: zone, KeyFile: kf}

	// Optional Brain (rule-store) join in the same command. Stored the same way —
	// key-file reference only, zone informational (server enforces from the key).
	brainURL := strings.TrimSpace(*brainEndpoint)
	if brainURL != "" {
		cfg.Brain = &config.BrainJoin{URL: brainURL, Zone: zone, KeyFile: strings.TrimSpace(*brainKeyFile)}
	}

	if err := config.Save(cfg); err != nil {
		return err
	}
	z := cfg.Bus.Zone
	if z == "" {
		z = "the zone your key enforces"
	}
	fmt.Fprintf(out, "oikos: joined %s (%s, server-enforced by your key). `oikos leave` to disconnect.\n", url, z)
	if brainURL != "" {
		fmt.Fprintf(out, "oikos: also joined Brain %s — run `oikos brain pull` then `oikos emit` to include your zone's rules.\n", brainURL)
	}
	return nil
}

// runLeave implements `oikos leave`: clears the bus membership. oikos is back to
// white/local — no bus, no socket.
func runLeave(args []string, out io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.Bus == nil && cfg.Brain == nil {
		fmt.Fprintln(out, "oikos: not joined to any bus.")
		return nil
	}
	cfg.Bus = nil
	cfg.Brain = nil
	if err := config.Save(cfg); err != nil {
		return err
	}
	fmt.Fprintln(out, "oikos: left the bus. oikos is local-only again.")
	return nil
}
