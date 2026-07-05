package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"oikos/internal/config"
	"oikos/internal/heal"
	"oikos/internal/wire"
)

// End-to-end through the serve wiring path: a wired Continue tool whose config
// file gets clobbered by an "IDE update" is healed back to the oikos proxy. This
// exercises HealTargets → heal.Watcher exactly as `oikos serve` wires them.
func TestSelfHealEndToEndReappliesProxy(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "continue-config.json")
	if err := os.WriteFile(cfg, []byte(`{"models":[{"apiBase":"`+wire.ProxyBaseURL+`/v1"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	c := config.Config{WiredTools: []config.WiredTool{{Name: "continue", Channel: wire.ChannelBaseURL}}}
	targets := wire.HealTargets(c, map[string]string{"continue": cfg})
	if len(targets) != 1 {
		t.Fatalf("expected 1 heal target, got %d", len(targets))
	}

	w := heal.New(targets)
	// We drive CheckOnce directly to prove the heal deterministically; the timed
	// fsnotify path is covered (with -race) in internal/heal.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Close()

	// IDE update factory-resets the base_url to a VENDOR DEFAULT (what a real
	// Continue/VSCode update does when it restores its OpenAI provider) — this is
	// oikos's own clobbered value, so it must be healed.
	if err := os.WriteFile(cfg, []byte(`{"models":[{"apiBase":"https://api.openai.com/v1"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Force a synchronous heal pass (proves the wiring; the timed loop is tested
	// in internal/heal).
	if n, err := w.CheckOnce(); err != nil || n != 1 {
		t.Fatalf("CheckOnce healed=%d err=%v, want 1 heal", n, err)
	}
	b, _ := os.ReadFile(cfg)
	if !strings.Contains(string(b), wire.ProxyBaseURL) {
		t.Fatalf("self-heal did not re-point Continue at the oikos proxy:\n%s", b)
	}

	// And the converse, end-to-end: a base_url the user DELIBERATELY set (their own
	// gateway, not a vendor default) must be LEFT ALONE — the serve wiring must
	// never stomp it. This is the ship-blocker guarantee at the integration seam.
	userSet := `{"models":[{"apiBase":"https://my-gateway.internal/v1"}]}`
	if err := os.WriteFile(cfg, []byte(userSet), 0o644); err != nil {
		t.Fatal(err)
	}
	if n, err := w.CheckOnce(); err != nil || n != 0 {
		t.Fatalf("a deliberate user override must not be healed; healed=%d err=%v", n, err)
	}
	if b, _ := os.ReadFile(cfg); string(b) != userSet {
		t.Fatalf("self-heal stomped a user-set base_url:\n got %s\nwant %s", b, userSet)
	}
}
