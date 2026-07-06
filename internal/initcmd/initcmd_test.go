package initcmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"essaim/internal/config"
	"essaim/internal/rules"
)

// The canonical "aha" query a brand-new user is told to paste.
const ahaQuery = "create a Card component"

// Run on a clean machine must: create a vault, persist it to config, and seed
// exactly one well-formed starter rule. This is the day-1 setup the forced demo
// hangs off of.
func TestRunSeedsVaultAndStarterRule(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(home, "config.json"))
	vault := filepath.Join(home, "essaim-vault")

	res, err := Run(Options{VaultDir: vault})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Vault created.
	if info, err := os.Stat(vault); err != nil || !info.IsDir() {
		t.Fatalf("vault dir was not created at %s: %v", vault, err)
	}
	// Config records the vault.
	c, _ := config.Load()
	if c.VaultDir != vault {
		t.Fatalf("config vault_dir = %q, want %q", c.VaultDir, vault)
	}
	// Exactly one starter rule file on disk.
	if res.RulePath == "" {
		t.Fatal("Run must report the seeded rule path")
	}
	if _, err := os.Stat(res.RulePath); err != nil {
		t.Fatalf("seeded rule file missing: %v", err)
	}
	// The demo output must hand the user a copy-paste prompt + a clear
	// before/after so the override is undeniable, AND a one-line hint that rules
	// fire on shared vocabulary (so the user's NEXT rule actually injects) pointing
	// at the full writing-rules guide.
	for _, want := range []string{ahaQuery, "card.js", "Card.js", "kebab-case", "database", "docs/writing-rules.md"} {
		if !strings.Contains(res.DemoText, want) {
			t.Fatalf("demo output must contain %q\n--- output ---\n%s", want, res.DemoText)
		}
	}
}

// P2-B: when `essaim init` seeds a vault, it must drop _inbox/.gitignore (the SAME
// M3 mechanism rules.EnsureInboxDir uses) so a user who runs init INSIDE a git
// work-tree never accidentally commits ephemeral drafts / the reinforce-counter
// sidecar. The marker must ignore everything under _inbox/ except itself.
func TestRunSeedsInboxGitignore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(home, "config.json"))
	vault := filepath.Join(home, "essaim-vault")

	if _, err := Run(Options{VaultDir: vault}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	gi := filepath.Join(vault, "_inbox", ".gitignore")
	body, err := os.ReadFile(gi)
	if err != nil {
		t.Fatalf("essaim init must seed %s so a git-tracked vault stays clean: %v", gi, err)
	}
	s := string(body)
	if !strings.Contains(s, "*") || !strings.Contains(s, "!.gitignore") {
		t.Fatalf("_inbox/.gitignore must ignore all but itself; got:\n%s", s)
	}
}

// P2-fix: EnsureInboxDir failure must be truly best-effort — `essaim init` must
// STILL succeed (seed the vault, the starter rule, and record the config) even
// when the _inbox/.gitignore hygiene step can't run. We force the failure by
// pre-creating vault/_inbox as a FILE, so os.MkdirAll("_inbox") errors ("not a
// directory") on every OS without relying on permissions. The old code returned
// that error and aborted init; the fix logs a warning and continues.
func TestRunSucceedsWhenInboxCreationFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(home, "config.json"))
	vault := filepath.Join(home, "essaim-vault")
	if err := os.MkdirAll(vault, 0o755); err != nil {
		t.Fatalf("prep vault: %v", err)
	}
	// Occupy the _inbox path with a FILE so EnsureInboxDir's MkdirAll fails.
	if err := os.WriteFile(filepath.Join(vault, "_inbox"), []byte("x"), 0o644); err != nil {
		t.Fatalf("prep _inbox file: %v", err)
	}

	// Capture the best-effort warning so we can assert it was surfaced (not silent).
	var warn bytes.Buffer
	prev := warnOut
	warnOut = &warn
	t.Cleanup(func() { warnOut = prev })

	res, err := Run(Options{VaultDir: vault})
	if err != nil {
		t.Fatalf("Run must SUCCEED despite an inbox-creation failure, got: %v", err)
	}

	// The demo still seeded the starter rule (the thing the user came for).
	if _, err := os.Stat(res.RulePath); err != nil {
		t.Fatalf("starter rule must still be seeded when inbox creation fails: %v", err)
	}
	// The vault is still recorded in the config.
	c, _ := config.Load()
	if c.VaultDir != vault {
		t.Fatalf("config vault_dir = %q, want %q (config must persist despite inbox failure)", c.VaultDir, vault)
	}
	// The failure was surfaced as a warning, not swallowed silently.
	if !strings.Contains(warn.String(), "_inbox") {
		t.Fatalf("expected a best-effort warning mentioning _inbox, got: %q", warn.String())
	}
}

// The seeded rule must be WELL-FORMED (parses back through the real loader with
// the durable frontmatter intact) — not just a text blob.
func TestStarterRuleParsesWellFormed(t *testing.T) {
	dir := t.TempDir()
	if err := writeStarterRule(dir); err != nil {
		t.Fatalf("writeStarterRule: %v", err)
	}
	rs, err := rules.LoadVault(dir)
	if err != nil {
		t.Fatalf("LoadVault: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("want exactly 1 starter rule, got %d", len(rs))
	}
	r := rs[0]
	if strings.TrimSpace(r.Title) == "" {
		t.Fatal("starter rule must have a title")
	}
	if strings.TrimSpace(r.Body) == "" {
		t.Fatal("starter rule must have a body")
	}
	// It must be injectable (status active/live/legacy — never a draft).
	if got := rules.InjectableRules(rs); len(got) != 1 {
		t.Fatalf("starter rule must be injectable, InjectableRules kept %d", len(got))
	}
}

// THE PROOF: the seeded rule, run through the REAL rules index + bloat guard,
// actually clears the similarity floor and would inject for the canonical aha
// query "create a Card component". If this passes, the user's first request is a
// GUARANTEED override, not a probabilistic one.
func TestStarterRuleInjectsForAhaQuery(t *testing.T) {
	dir := t.TempDir()
	if err := writeStarterRule(dir); err != nil {
		t.Fatalf("writeStarterRule: %v", err)
	}
	rs, err := rules.LoadVault(dir)
	if err != nil {
		t.Fatalf("LoadVault: %v", err)
	}
	ix := rules.BuildIndex(rules.InjectableRules(rs))

	// Use the SAME default guard config the proxy uses (floor 0.60, top-k 10).
	res, err := ix.MatchAndGuard(ahaQuery, rules.GuardConfig{})
	if err != nil {
		t.Fatalf("the starter rule must inject for %q, but MatchAndGuard returned: %v", ahaQuery, err)
	}
	if len(res.Kept) != 1 {
		t.Fatalf("want the starter rule kept (1), got %d", len(res.Kept))
	}
	// The rendered+wrapped block must carry the kebab-case directive — this is
	// the exact text the model sees that flips Card.js → card.js.
	block := rules.WrapBlock(rules.RenderBody(res.Kept))
	if !strings.Contains(strings.ToLower(block), "kebab") {
		t.Fatalf("the injected block must instruct kebab-case file naming:\n%s", block)
	}
}

// Belt-and-suspenders against a false-inject regression: a totally unrelated
// query must NOT drag the React rule in (proving the rule is specific, not a
// catch-all that would injure unrelated requests).
func TestStarterRuleDoesNotInjectForUnrelatedQuery(t *testing.T) {
	dir := t.TempDir()
	if err := writeStarterRule(dir); err != nil {
		t.Fatalf("writeStarterRule: %v", err)
	}
	rs, _ := rules.LoadVault(dir)
	ix := rules.BuildIndex(rules.InjectableRules(rs))

	_, err := ix.MatchAndGuard("what time is the dentist appointment tomorrow", rules.GuardConfig{})
	if err == nil {
		t.Fatal("the React starter rule must NOT inject for an unrelated query (false-inject regression)")
	}
}

// THE EMIT-PATH PROOF: init's printed quickstart sends a brand-new user
// straight to `essaim emit` (the proxy-less path). The seeded starter rule must
// therefore clear the native-block gate (isLive ∧ isEager via EmitEager) — or
// the FIRST thing a new user sees is an empty fenced block flatly
// contradicting init's "One starter rule is live in your vault" message
// (first-run bug, 2026-07-06).
func TestStarterRuleIsEmittedIntoNativeBlock(t *testing.T) {
	dir := t.TempDir()
	if err := writeStarterRule(dir); err != nil {
		t.Fatalf("writeStarterRule: %v", err)
	}
	rs, err := rules.LoadVault(dir)
	if err != nil {
		t.Fatalf("LoadVault: %v", err)
	}
	ix := rules.BuildIndex(rules.InjectableRules(rs))
	res, err := ix.EmitEager(rules.GuardConfig{})
	if err != nil {
		t.Fatalf("EmitEager: %v", err)
	}
	if len(res.Kept) != 1 {
		t.Fatalf("starter rule must be emitted by the native-file path, EmitEager kept %d — a fresh `essaim init` + `essaim emit` must not produce an empty block", len(res.Kept))
	}
}
