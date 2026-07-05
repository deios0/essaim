// Package initcmd implements `oikos init`: the forced first-run demo that gives
// a brand-new user a GUARANTEED "aha" on their very first request instead of a
// probabilistic one.
//
// It (1) ensures a vault exists and is recorded in the config, then (2) seeds a
// single, well-formed STARTER rule that VISIBLY overrides a model default —
// kebab-case React component filenames — and (3) prints a copy-paste test prompt
// plus the exact before/after so the difference is undeniable:
//
//	"create a Card component"  →  card.js   (WITH oikos)
//	                              Card.js   (a stock model's default)
//
// The rule is deliberately specific (it names React/component/create/kebab-case)
// so it clears the similarity floor for the canonical prompt yet never
// false-injects into unrelated requests.
package initcmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"oikos/internal/config"
	"oikos/internal/rules"
)

// warnOut is where Run emits best-effort warnings (e.g. a failed _inbox/.gitignore
// seed that must NOT abort init). It is a package var so a test can capture the
// warning and prove init still succeeds. Production writes to stderr.
var warnOut io.Writer = os.Stderr

// starterRuleFilename is the on-disk name of the seeded demo rule.
const starterRuleFilename = "react-kebab-case-components.md"

// AhaQuery is the canonical prompt the demo tells the user to paste — the one
// the starter rule is tuned to override on the first try.
const AhaQuery = "create a Card component"

// starterRule is the seeded demo rule. It is a fully-formed Obsidian-style note
// (durable YAML frontmatter + Markdown body) so it parses back through the real
// loader and enters the matchable index as a `live` rule — injectable through the
// proxy AND emitted by the native-file path (`oikos emit`): init's quickstart
// sends the user straight to emit, and only `status: live` clears that stricter
// gate — `active` would greet the first-run user with an empty block. The
// body is dense with the query's distinctive words (create / Card / component /
// React / file) so it clears the similarity floor for "create a Card component"
// while staying specific enough not to false-inject elsewhere.
const starterRule = `---
id: react-kebab-case-components
title: React component files are kebab-case
kind: standard
scope: project
status: live
confidence: 0.95
weight: 0.95
criticality: "5"
timeless: true
---
When you create a React component, name the component's file in kebab-case, not
PascalCase. Creating a component called Card means the file is card.js (or
card.tsx) — never Card.js. This applies to every React component file you
create: a Card component goes in card.js, a UserMenu component goes in
user-menu.js, a NavBar component goes in nav-bar.js. Keep the exported component
name PascalCase (export function Card), but the file that contains it is always
kebab-case.
`

// Options configures Run.
type Options struct {
	// VaultDir is where the vault is created. Empty ⇒ Run derives a sensible
	// default under the user's home (~/oikos-vault).
	VaultDir string
}

// Result is what Run produced, for the caller to print.
type Result struct {
	VaultDir string // the vault that now holds the demo rule
	RulePath string // the seeded starter rule file
	Created  bool   // true if the vault dir was created by this run
	DemoText string // the copy-paste prompt + before/after for the user
}

// Run performs the forced first-run demo: ensure a vault, seed the starter rule,
// persist the vault in the config, and return the demo text. It is idempotent —
// re-running never duplicates the rule or the config entry.
func Run(opts Options) (Result, error) {
	vault := opts.VaultDir
	if vault == "" {
		var err error
		vault, err = defaultVaultDir()
		if err != nil {
			return Result{}, err
		}
	}
	abs, err := filepath.Abs(vault)
	if err != nil {
		return Result{}, err
	}
	vault = abs

	created := false
	if info, statErr := os.Stat(vault); statErr != nil {
		if !os.IsNotExist(statErr) {
			return Result{}, statErr
		}
		if mkErr := os.MkdirAll(vault, 0o755); mkErr != nil {
			return Result{}, mkErr
		}
		created = true
	} else if !info.IsDir() {
		return Result{}, fmt.Errorf("oikos init: %s exists but is not a directory", vault)
	}

	if err := writeStarterRule(vault); err != nil {
		return Result{}, err
	}

	// P2-B: seed _inbox/.gitignore via the SAME M3 mechanism the extractor/hot
	// store use. If the user ran `oikos init` inside a git work-tree, this keeps
	// ephemeral drafts and the reinforce-counter sidecar out of their commits. It
	// is best-effort: a non-fatal error here (e.g. a read-only FS) must not stop a
	// successful demo seed — the rule above is already written.
	//
	// P2-fix: this was documented best-effort but ACTUALLY aborted init on failure.
	// Now it truly is best-effort — a warning is logged (to warnOut, stderr in
	// production) and init continues. The _inbox/.gitignore is a git-hygiene nicety,
	// not a prerequisite for the demo rule the user came for.
	if _, err := rules.EnsureInboxDir(vault); err != nil {
		fmt.Fprintf(warnOut, "oikos init: could not create %s/_inbox (git-ignore hygiene skipped, "+
			"drafts may show up in git status): %v\n", vault, err)
	}

	// Record the vault in the config so `oikos serve` injects from it without any
	// further setup. Update loads the existing config (preserving whatever else is
	// there), sets the vault, and saves — all under the config mutex so a concurrent
	// wire/setup can't clobber this write (P2).
	if err := config.Update(func(c *config.Config) error {
		c.VaultDir = vault
		return nil
	}); err != nil {
		return Result{}, err
	}

	return Result{
		VaultDir: vault,
		RulePath: filepath.Join(vault, starterRuleFilename),
		Created:  created,
		DemoText: demoText(vault),
	}, nil
}

// writeStarterRule writes the demo rule into dir. It is idempotent: it always
// (over)writes the SAME single file, so re-running never produces a duplicate.
func writeStarterRule(dir string) error {
	path := filepath.Join(dir, starterRuleFilename)
	return os.WriteFile(path, []byte(starterRule), 0o644)
}

// defaultVaultDir is ~/oikos-vault — a cross-platform home-anchored default (no
// hostname/machine-id derivation, matching config.Path's portability stance).
func defaultVaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "oikos-vault"), nil
}

// demoText is the undeniable before/after the user pastes to feel the override.
// It names the exact prompt, the WITH-oikos result, and the stock-model result,
// so the difference is guaranteed-visible on request #1.
func demoText(vault string) string {
	return fmt.Sprintf(`oikos is set up. One starter rule is live in your vault:
  %s

Now write it into your tool's native rules file — NO proxy needed:

  ┌────────────────────────────────────────────────────────────────┐
  │  oikos emit --file claude-code=./CLAUDE.md --file codex=./AGENTS.md │
  └────────────────────────────────────────────────────────────────┘

Then ask your AI:

  ┌────────────────────────────────────────────┐
  │  %-42s│
  └────────────────────────────────────────────┘

  WITH the rule  →  card.js     ✅  (kebab-case, the rule won)
  without it     →  Card.js         (the model's stock default)

That's the whole idea: you teach ONE correction (React files are kebab-case) and
oikos keeps it written into every tool's AGENTS.md / CLAUDE.md — a static file you
maintain by hand can't do that. Re-run `+"`oikos emit`"+` anytime to refresh.

Optional: run `+"`oikos serve`"+` (live mode) to capture corrections in real time.

Tip: a rule fires when it shares words with your request, so write the vocabulary
of the questions it should catch INTO the rule (a "use PostgreSQL" rule should say
"database" too). More: docs/writing-rules.md
`, vault, AhaQuery)
}
