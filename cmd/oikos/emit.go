package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"oikos/internal/config"
	"oikos/internal/emit"
	"oikos/internal/rules"
)

// stringSlice is a repeatable string flag (`--file a=b --file c=d`).
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// runEmit implements `oikos emit` — the STANDALONE NativeFileEmitter path. It
// regenerates the ranked, LIVE-only oikos block from the vault and writes it into
// each wired native instruction file (AGENTS.md / CLAUDE.md / GEMINI.md) ON
// DEMAND, with NO proxy running. It is the first-class way to keep your AGENTS.md
// current from your correction-learned vault: the proxy's "live mode" does this
// continuously, `oikos emit` does it once.
//
//	oikos emit                              # vault + native files from config
//	oikos emit --vault <dir>               # override the vault
//	oikos emit --file <name>=<path> [...]  # explicit native-file target(s)
//
// Target resolution (first non-empty wins): --file flags, then the
// OIKOS_NATIVE_FILE_TOOLS env (`name=path,name=path`), then the persisted
// config's native_file wired tools. Vault resolution (first non-empty wins):
// --vault, OIKOS_VAULT, the persisted config's vault, else ~/oikos-vault.
//
// It is the SAME emitter the daemon uses (one ranked source, one fenced block,
// idempotent, atomic, backup-on-first-write, credential-scrubbing) — so a file
// kept current by `oikos emit` is byte-identical to one the live proxy maintains.
//
// Credential scrubbing is the shared lexicon predicate (extract.RedactCredentials
// / ContainsCredential): every armor-bearing key (PEM/PGP private-key block, lone
// BEGIN/END marker), every keyed/separator secret (`api_key=…`, `password: …`),
// every prefixed token (AWS AKIA…, sk-/sk_live_/whsec_, gh*_/github_pat_/glpat-,
// xox*-, AIza…, JWT), a URL-embedded user:pass@, a space-separated bearer, AND a
// context-gated markerless AWS *secret* access key (40-char base64 adjacent to
// `aws`+secret/access/key) are dropped/redacted. NOT claimed: a fully markerless
// secret with NO recognizable format or context (e.g. a bare high-entropy blob a
// user pastes with no key words) is intentionally NOT gated — a blind
// length+entropy heuristic was reverted as net-negative (it dropped legit rules);
// see docs/decisions/2026-06-24-p1d-privatekeybody-reverted.md.
func runEmit(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("emit", flag.ContinueOnError)
	fs.SetOutput(out)
	vaultFlag := fs.String("vault", "", "vault directory to emit from (default: OIKOS_VAULT, else the configured vault, else ~/oikos-vault)")
	var fileFlags stringSlice
	fs.Var(&fileFlags, "file", "a native-file target as name=path (repeatable; e.g. --file claude-code=./CLAUDE.md)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, _ := config.Load()

	vault := resolveEmitVault(*vaultFlag, cfg)
	if vault == "" {
		return fmt.Errorf("oikos emit: no vault configured (set --vault, OIKOS_VAULT, or run `oikos init`)")
	}
	if info, err := os.Stat(vault); err != nil || !info.IsDir() {
		return fmt.Errorf("oikos emit: vault %q is not a readable directory", vault)
	}

	tools, err := resolveEmitTools(fileFlags, cfg)
	if err != nil {
		return err
	}
	if len(tools) == 0 {
		return fmt.Errorf("oikos emit: no native-file targets — pass --file name=path, set OIKOS_NATIVE_FILE_TOOLS, " +
			"or wire a tool's native file (e.g. `oikos wire claude-code`)")
	}

	// Build the index from the vault exactly as the daemon does (status-filtered
	// {active,live}, BM25 + concept augment). NewStore does a single rebuild; we
	// never start the watcher — this is a one-shot regeneration.
	store, err := rules.NewStore(vault)
	if err != nil {
		return fmt.Errorf("oikos emit: could not load vault: %w", err)
	}

	em := emit.New(rules.GuardConfigFromEnv(), tools)
	em.SetDebounce(0) // synchronous: write now, then exit
	block, results, err := em.EmitNowWithResults(store.Index())
	if err != nil {
		return fmt.Errorf("oikos emit: %w", err)
	}

	// Per-target summary reflecting the ACTUAL outcome (review follow-up #4): never
	// claim "wrote N rules" for a target that was skipped, refused, or failed.
	liveCount := liveRuleCount(block)
	failed := 0
	for _, r := range results {
		switch r.Status {
		case emit.StatusWritten:
			fmt.Fprintf(out, "oikos: wrote %d live rule(s) into %s (%s)\n", liveCount, r.NativeFile, r.Name)
		case emit.StatusSkipped:
			fmt.Fprintf(out, "oikos: %s (%s) already up to date — skipped (no change)\n", r.NativeFile, r.Name)
		case emit.StatusRefused:
			fmt.Fprintf(out, "oikos: REFUSED %s (%s) — the target path contains a credential pattern; not written\n", r.NativeFile, r.Name)
		case emit.StatusFailed:
			failed++
			fmt.Fprintf(out, "oikos: FAILED to write %s (%s): %v\n", r.NativeFile, r.Name, r.Err)
		}
	}
	if liveCount == 0 {
		fmt.Fprintln(out, "oikos: note — the vault has no live rules yet, so the block is empty "+
			"(it will fill in as you teach corrections). The fenced region is in place and will update on the next emit.")
	}
	if failed > 0 {
		return fmt.Errorf("oikos emit: %d of %d target(s) failed to write", failed, len(results))
	}
	return nil
}

// resolveEmitVault applies the precedence: --vault, OIKOS_VAULT, config vault,
// then ~/oikos-vault (the same default `oikos init` uses), absolutized.
func resolveEmitVault(vaultFlag string, cfg config.Config) string {
	v := strings.TrimSpace(vaultFlag)
	if v == "" {
		v = strings.TrimSpace(os.Getenv("OIKOS_VAULT"))
	}
	if v == "" {
		v = strings.TrimSpace(cfg.VaultDir)
	}
	if v == "" {
		if home, err := os.UserHomeDir(); err == nil {
			v = filepath.Join(home, "oikos-vault")
		}
	}
	if v == "" {
		return ""
	}
	if abs, err := filepath.Abs(v); err == nil {
		return abs
	}
	return v
}

// resolveEmitTools applies the precedence "first non-empty wins": --file flags,
// then OIKOS_NATIVE_FILE_TOOLS, then the persisted config's native_file wired
// tools. A SET-but-malformed OIKOS_NATIVE_FILE_TOOLS (non-empty, but no valid
// `name=path` pair) does NOT fall through to config (review follow-up #3): the env
// var was non-empty, so it wins, and an unusable value is an explicit error rather
// than a surprising silent write to the config target.
func resolveEmitTools(fileFlags stringSlice, cfg config.Config) ([]emit.Tool, error) {
	if len(fileFlags) > 0 {
		return parseFilePairs(fileFlags), nil
	}
	if raw := strings.TrimSpace(os.Getenv("OIKOS_NATIVE_FILE_TOOLS")); raw != "" {
		// Env is non-empty → it wins. Use it if it parses; error if it does not.
		if env := nativeFileToolsFromEnv(); len(env) > 0 {
			return env, nil
		}
		return nil, fmt.Errorf("oikos emit: OIKOS_NATIVE_FILE_TOOLS=%q is malformed "+
			"(expected comma-separated name=path pairs, e.g. claude-code=./CLAUDE.md); "+
			"refusing to fall through to the configured target", raw)
	}
	var out []emit.Tool
	for _, t := range cfg.WiredTools {
		if t.Channel == "native_file" && t.NativeFile != "" {
			out = append(out, emit.Tool{Name: t.Name, NativeFile: t.NativeFile})
		}
	}
	return out, nil
}

// parseFilePairs parses repeated `name=path` --file values into emit.Tool
// targets. Malformed pairs (no `=`, empty name/path) are skipped.
func parseFilePairs(pairs []string) []emit.Tool {
	var out []emit.Tool
	for _, p := range pairs {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		name, path, ok := strings.Cut(p, "=")
		name, path = strings.TrimSpace(name), strings.TrimSpace(path)
		if !ok || name == "" || path == "" {
			continue
		}
		out = append(out, emit.Tool{Name: name, NativeFile: path})
	}
	return out
}

// liveRuleCount counts the rule bullets in an emitted block (for the human-facing
// summary). RenderBody renders each kept rule as a "- " bullet line inside the
// fence; an empty block has none. It is a display heuristic only.
func liveRuleCount(block string) int {
	n := 0
	for _, ln := range strings.Split(block, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "- ") {
			n++
		}
	}
	return n
}
