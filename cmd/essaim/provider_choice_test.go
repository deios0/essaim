package main

import (
	"context"
	"testing"

	"essaim/internal/upstream"
)

// resolveProviderChoice precedence (P0-3 review fixes #1 and #2):
//   - a "local" config choice must NOT be routed to the cloud even if a key is
//     stored (#1);
//   - a fixed upstream (ESSAIM_UPSTREAM_BASE) must be honoured on every resolution,
//     not just startup, so a hot-reload doesn't clobber it into ErrNoBackend (#2).
func TestResolveProviderChoice(t *testing.T) {
	// #1: provider=local with a stored key → keyless (local) upstream, NOT the key.
	p := resolveProviderChoice(false, "local", "sk-or-leftover")
	su, ok := p.(*upstream.SingleUpstream)
	if !ok {
		t.Fatalf("want *SingleUpstream, got %T", p)
	}
	if su.Key != "" {
		t.Fatalf("a 'local' choice must NOT carry the stored key (no silent cloud route), got Key=%q", su.Key)
	}

	// #2: a fixed upstream is honoured and never returns ErrNoBackend even with no
	// key/local LLM — the fixed-upstream provider resolves immediately.
	pf := resolveProviderChoice(true, "", "")
	suf := pf.(*upstream.SingleUpstream)
	if _, err := suf.Select(context.Background()); err != nil {
		t.Fatalf("fixed-upstream provider must resolve without a key/local LLM, got err: %v", err)
	}
	// And a fixed upstream WITH a key carries it (authenticated gateway).
	pfk := resolveProviderChoice(true, "openrouter", "sk-or-x").(*upstream.SingleUpstream)
	if pfk.Key != "sk-or-x" {
		t.Fatalf("fixed upstream must carry the key for an authenticated gateway, got %q", pfk.Key)
	}

	// Normal openrouter path: a key present → keyed upstream.
	pk := resolveProviderChoice(false, "openrouter", "sk-or-y").(*upstream.SingleUpstream)
	if pk.Key != "sk-or-y" {
		t.Fatalf("openrouter with a key must carry it, got %q", pk.Key)
	}

	// Nothing chosen, no key → keyless (the clean first-run default).
	pd := resolveProviderChoice(false, "", "").(*upstream.SingleUpstream)
	if pd.Key != "" {
		t.Fatalf("first-run default must be keyless, got %q", pd.Key)
	}
}
