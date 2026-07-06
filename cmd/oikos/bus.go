package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"oikos/internal/bus"
	"oikos/internal/config"
)

// runBus implements `oikos bus`: report join status and, when joined, do one live
// poll to confirm the stored join actually reaches the bus in its zone. On a
// not-joined binary it prints the status and opens NO socket (default-off is
// observable). Env AIBUS_URL/AIBUS_KEY override the stored endpoint/key.
func runBus(_ []string, out io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	stored := bus.Endpoint{}
	if cfg.Bus != nil {
		stored = bus.Endpoint{URL: cfg.Bus.URL, Zone: cfg.Bus.Zone}
		if cfg.Bus.KeyFile != "" {
			if k, err := bus.LoadKey(cfg.Bus.KeyFile); err == nil {
				stored.Key = k
			}
		}
	}
	ep, ok := bus.Resolve(os.Getenv, stored)
	if !ok {
		fmt.Fprintln(out, "oikos: not joined to any bus. `oikos join --endpoint <url> --key-file <path>` to connect.")
		return nil
	}
	zone := ep.Zone
	if zone == "" {
		zone = "(zone from key)"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	evs, _, err := bus.New(ep).Poll(ctx, 1<<62) // huge cursor: connectivity check, no backlog
	if err != nil {
		return fmt.Errorf("oikos bus: joined %s (%s) but the bus is unreachable or the key was rejected: %w", ep.URL, zone, err)
	}
	fmt.Fprintf(out, "oikos: joined %s, zone %s — connection live (%d recent event(s)).\n", ep.URL, zone, len(evs))
	return nil
}
