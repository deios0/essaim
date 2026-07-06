// Command oikos is the oikosd local proxy daemon.
//
// Subcommands:
//
//	oikos version            print the build version
//	oikos emit               regenerate your AGENTS.md (+ CLAUDE.md / GEMINI.md)
//	                         from the vault, ON DEMAND, with NO proxy running. The
//	                         standalone way to keep your tool's native rules file
//	                         current from your correction-learned vault:
//	                           oikos emit --vault <dir> --file claude-code=./CLAUDE.md
//	                         Targets/vault default from the persisted config; the
//	                         output is byte-identical to the block the live proxy
//	                         maintains. This is the first-class path — the proxy is
//	                         opt-in "live mode" for continuous, per-request capture.
//	oikos serve              run the loopback proxy on 127.0.0.1:4141 (opt-in
//	                         "live mode": real-time correction capture + per-request
//	                         injection for tools you point at the proxy)
//	oikos serve --require-token
//	                         additionally gate /v1/* behind a 0600 loopback
//	                         bearer token (opt-in; default is single-user-host
//	                         trust). /health is always open.
//	oikos wire <tool>        point a tool (cursor / claude-code / continue / …)
//	                         at the oikos proxy through the correct channel.
//	oikos unwire <tool>      cleanly undo `oikos wire <tool>` — restore the tool's
//	                         original config byte-exact (idempotent).
//	oikos init               forced first-run demo: seed a vault + a starter rule
//	                         that visibly overrides a model default (React
//	                         component files → kebab-case), then print a
//	                         copy-paste prompt + the before/after so the first
//	                         request is a guaranteed "aha".
//	oikos sync --remote <url>
//	                         push/pull the rule vault to YOUR git remote and
//	                         merge it deterministically (no rule lost). Optional,
//	                         local, $0 — the M4 sync primitive the future paid
//	                         Team-Rule-Sync drops into.
//	oikos join --endpoint <url> --key-file <path> [--zone <z>] [--no-verify]
//	                         OPT-IN: connect oikos to a bus. Live-validates the key
//	                         against the endpoint, then persists the membership
//	                         (endpoint + zone + key-FILE reference — never the raw
//	                         key). The server derives+enforces the real zone from
//	                         the key. AIBUS_URL/AIBUS_KEY env override the stored
//	                         values at use time. Default-off: no join, no socket.
//	oikos leave              disconnect from the bus; oikos is local-only again.
//	oikos bus                show join status; when joined, do one live poll to
//	                         confirm the connection reaches the bus in its zone.
//
// On first run (no config yet) `oikos serve` prints the one-screen setup URL
// http://127.0.0.1:4141/setup, where a non-technical user picks a model (a
// cloud key or an auto-detected local LLM), a vault, and which tools to wire.
//
// The proxy is BYOK and verbatim: it forwards /v1/chat/completions,
// /v1/completions, and /v1/models to the resolved upstream (OpenRouter via
// key, or an auto-detected local Ollama/LM Studio) and relays streaming
// responses byte-for-byte. It phones home to nothing.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"oikos/internal/auth"
	"oikos/internal/config"
	"oikos/internal/emit"
	"oikos/internal/extract"
	"oikos/internal/heal"
	"oikos/internal/initcmd"
	"oikos/internal/learn"
	"oikos/internal/rules"
	"oikos/internal/secret"
	"oikos/internal/server"
	"oikos/internal/upstream"
	"oikos/internal/wire"
)

var version = "0.0.0-dev"

// keyringService is the OS-credential-store service name oikos stores its
// loopback token and BYOK keys under.
const keyringService = "oikos"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "serve":
		if err := serve(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "wire":
		if err := runWire(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "unwire":
		if err := runUnwire(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "init":
		if err := runInit(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "emit":
		if err := runEmit(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "sync":
		if err := runSync(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "join":
		if err := runJoin(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "leave":
		if err := runLeave(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "bus":
		if err := runBus(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "brain":
		if err := runBrain(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "onboard":
		if err := runOnboard(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: oikos <serve|emit|wire|unwire|init|sync|join|leave|bus|brain|onboard|version>")
}

// runInit implements `oikos init` — the forced first-run demo. It ensures a
// vault, seeds the kebab-case React starter rule, records the vault in the
// config, and prints a copy-paste prompt + the exact before/after so a brand-new
// user gets a GUARANTEED override on request #1.
//
//	oikos init                 # vault at ~/oikos-vault (or the configured one)
//	oikos init --vault <path>  # seed the demo into <path>
func runInit(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(out)
	vaultFlag := fs.String("vault", "", "vault directory to create/seed (default: the configured vault, else ~/oikos-vault)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// If no --vault and the config already names one, seed into it (idempotent).
	vault := *vaultFlag
	if vault == "" {
		if c, err := config.Load(); err == nil && c.VaultDir != "" {
			vault = c.VaultDir
		}
	}

	res, err := initcmd.Run(initcmd.Options{VaultDir: vault})
	if err != nil {
		return err
	}
	fmt.Fprintln(out, res.DemoText)
	return nil
}

// firstRunBanner is the prominent message `oikos serve` prints when no config
// exists yet. It LEADS with the wedge pitch (server.WedgePitch — the same exact
// sentence the /setup page hero renders, one source of truth) and then points the
// user at the one-screen setup page.
func firstRunBanner() string {
	return "\n" +
		"  " + server.WedgePitch + "\n" +
		"\n" +
		"  ┌──────────────────────────────────────────────────────────┐\n" +
		"  │  oikos is running in optional \"live mode\" (the proxy).     │\n" +
		"  │                                                            │\n" +
		"  │  The first-class path needs no proxy:                      │\n" +
		"  │      ▸  oikos init     seed a vault + a starter rule       │\n" +
		"  │      ▸  oikos emit     write your AGENTS.md from the vault  │\n" +
		"  │                                                            │\n" +
		"  │  Want real-time correction capture? Finish setup here:     │\n" +
		"  │      ▸  http://127.0.0.1:4141/setup                        │\n" +
		"  └──────────────────────────────────────────────────────────┘\n"
}

// serve runs the proxy. It is loud/blocking on a port conflict and never
// silently falls back to an ephemeral port.
func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	requireToken := fs.Bool("require-token", false,
		"gate /v1/* behind a 0600 loopback bearer token (opt-in; default is single-user-host trust)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	const addr = "127.0.0.1:4141"

	// Load the persisted first-run config (the /setup UI writes it). A missing
	// file is not an error — it just means first run. Config values seed the
	// environment-driven knobs the injection layer reads at construction, but the
	// environment ALWAYS wins (env override is the documented final word).
	cfg, _ := config.Load()
	if cfg.VaultDir != "" && os.Getenv("OIKOS_VAULT") == "" {
		_ = os.Setenv("OIKOS_VAULT", cfg.VaultDir)
	}
	// P2-3: the native-file wired tools are resolved DIRECTLY from the config below
	// (nativeFileToolsForServe), NOT round-tripped through the comma-delimited
	// OIKOS_NATIVE_FILE_TOOLS env var. That round-trip silently dropped a tool whose
	// path contained a comma (Windows paths, quoted dirs). The env var is still
	// honoured as an explicit external override, but the config path — which is the
	// common case — no longer passes through comma-joined strings.

	// The injection layer watches the vault for the process lifetime; tie the
	// fsnotify watcher to a context cancelled on shutdown. The context is also
	// cancelled on SIGINT/SIGTERM (signal.NotifyContext) so a Ctrl-C / systemd stop
	// triggers the GRACEFUL shutdown sequence below (drain in-flight relays via
	// http.Server.Shutdown, THEN stop the learner/emitter/heal watcher by cancelling
	// this ctx). Without this, the old `http.Serve` blocked forever and a signal
	// hard-killed the process mid-relay, truncating in-flight responses and dropping
	// queued captures (P2-1).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The learner must keep processing captures WHILE the HTTP server drains
	// in-flight relays on shutdown — otherwise captures produced during the drain
	// are accepted into the queue but never processed (codex review). So the async
	// learner runs on a SEPARATE context that serveGraceful cancels only AFTER
	// srv.Shutdown returns, not at signal receipt.
	workerCtx, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()

	s, err := server.NewWithInjection(ctx, addr)
	if err != nil {
		return fmt.Errorf("oikos: could not initialize injection layer: %w", err)
	}

	// Resolve the BYOK upstream lazily from the credential store / env.
	store := secret.EnvOrKeyring{
		Keyring: secret.Keyring{Service: keyringService},
		EnvVars: map[string]string{"openrouter-key": "OIKOS_OPENROUTER_KEY"},
	}

	// OIKOS_UPSTREAM_BASE points oikos at a specific OpenAI-compatible upstream (a
	// local proxy, a self-hosted gateway, or the injection demo's fake upstream).
	// It is the FINAL word on WHERE traffic goes — a fixed upstream — so a missing
	// key/local-LLM never 401s when an explicit upstream is configured. We read it
	// once; resolveProvider honours it on every (re)resolution.
	upstreamBase := os.Getenv("OIKOS_UPSTREAM_BASE")
	if upstreamBase != "" {
		s.SetUpstreamBase(upstreamBase)
	}

	// resolveProvider installs the matching provider on the running server from the
	// CURRENT persisted choice + credential store. It is called once at startup AND
	// again after a successful /setup POST (P0-3 hot-reload) so a key/provider set
	// via the browser takes effect WITHOUT a restart. The decision is the pure
	// resolveProviderChoice below.
	resolveProvider := func() {
		key, _ := store.Get("openrouter-key")
		cfgNow, _ := config.Load()
		s.SetProvider(resolveProviderChoice(upstreamBase != "", cfgNow.Provider, key))
	}
	resolveProvider()

	// Wire the first-run setup surface: the /setup POST handlers persist a pasted
	// key into the SAME credential store the BYOK resolution reads, and the page
	// pre-selects the zero-key path when a local LLM is already running.
	s.SetSecretStore(store)
	s.SetSetupDetect(detectLocalLLM)
	// P0-3: after a successful /setup/model POST, re-resolve the provider from the
	// store + config so the running proxy uses the new key/provider live (no restart).
	s.SetOnProviderUpdate(resolveProvider)

	// M3: wire the off-path learning loop + the NativeFileEmitter when a vault is
	// configured. The capture sink is async (drop-on-full) so the response path is
	// never blocked; the emitter writes the live block into wired native files.
	if vault := s.VaultDir(); vault != "" {
		l := learn.New(vault, extractConfigFromEnv())
		l.Start(workerCtx.Done()) // outlives the HTTP drain (stopped after Shutdown)
		s.SetCaptureSink(l)

		if tools := nativeFileToolsForServe(cfg); len(tools) > 0 {
			em := emit.New(rules.GuardConfigFromEnv(), tools)
			names := make(map[string]bool, len(tools))
			for _, t := range tools {
				names[t.Name] = true
			}
			s.SetFileEmitTools(names) // one channel per tool: proxy stays out
			// RESPECTS-UNWIRE (P1): the emitter snapshots its wired tools once here at
			// boot, but the user may `oikos unwire <tool>` on the RUNNING daemon. Give it
			// the SAME live wired-tools view heal gets (mtime-cached read of config.json)
			// so it stops re-injecting a tool's block the moment unwire removes it — no
			// restart. Undeterminable config ⇒ fail toward emitting (keeps guarding).
			em.SetLiveTools(wire.LiveWiredTools())
			// Surface per-target emit failures (P1): the daemon OnIndexSwap path used to
			// discard the ([]TargetResult, error), so a wired tool's native file could go
			// permanently stale with zero signal. Log each failed/refused target to stderr.
			em.SetOnEmitProblem(func(r emit.TargetResult) {
				if r.Name == "" {
					fmt.Fprintf(os.Stderr, "oikos: native-file emit failed: %v\n", r.Err)
					return
				}
				reason := "refused (credential-bearing path)"
				if r.Err != nil {
					reason = r.Err.Error()
				}
				fmt.Fprintf(os.Stderr, "oikos: native-file emit to %q (%s) did not apply: %s\n",
					r.NativeFile, r.Name, reason)
			})
			if store := s.Store(); store != nil {
				store.SetOnSwap(em.OnIndexSwap) // emit on every index swap (off the hot path)
				em.OnIndexSwap(store.Index())   // emit the initial block now
			}
		}
	}

	// Config-drift self-heal: when base_url tools are wired, watch their IDE config
	// files and re-apply the oikos proxy base_url ONLY if an IDE update clobbers
	// oikos's own value (a vendor-default reset or a dropped key). A base_url a user
	// deliberately set to something else is left alone — the watcher never fights
	// the user. This is STRICTLY off the request hot path — it runs in the watcher's
	// own goroutine doing only LOCAL file I/O (no network, no proxy involvement) —
	// and is opt-in: it starts only when there is a wired base_url tool with a
	// config file present.
	//
	// Two disable controls: OIKOS_NO_HEAL=1 turns it off at boot, and a LIVE
	// kill-switch flag file (heal.disabled next to the config) turns it off WITHOUT
	// a restart — `touch` it to pause healing, `rm` it to resume. `oikos unwire`
	// also drops a tool's target.
	if !envTruthy(os.Getenv("OIKOS_NO_HEAL")) {
		startSelfHeal(ctx)
	}

	// Amendment 1: the loopback token gate is OFF by default and engaged only
	// when --require-token is passed. /health is always open regardless.
	//
	// P1-6b: route the token through EnvOrKeyring (the SAME env-first fallback the
	// BYOK key uses) so a headless box with no OS Secret Service (WSL/server) can
	// supply OIKOS_LOOPBACK_TOKEN instead of crashing on go-keyring. When neither
	// the env var nor the keyring works, auth returns ErrKeyringUnavailable, which
	// we surface as a clean one-line error and exit — never a go-keyring panic.
	if *requireToken {
		tokenStore := secret.EnvOrKeyring{
			Keyring: secret.Keyring{Service: keyringService},
			EnvVars: map[string]string{"loopback-token": "OIKOS_LOOPBACK_TOKEN"},
		}
		token, err := auth.LoadOrCreateToken(tokenStore)
		if err != nil {
			return err // already a clear, actionable one-liner (ErrKeyringUnavailable)
		}
		s.SetToken(token)
		fmt.Fprintln(os.Stderr, "oikos: --require-token enabled; /v1/* requires the loopback bearer token")
	}

	l, err := s.Listen()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "oikos %s listening on http://%s\n", version, addr)

	// First-run detection: no persisted config AND no key/local-LLM steering via
	// env ⇒ the user has nothing set up. Print the one-screen setup URL
	// prominently (the front door). Headless boxes still get a clear, copyable URL
	// — we never auto-open a browser from a non-interactive process.
	if cfg.IsEmpty() && os.Getenv("OIKOS_VAULT") == "" {
		fmt.Fprint(os.Stderr, firstRunBanner())
	}

	// P2-1: serve with GRACEFUL shutdown. When ctx is cancelled (SIGINT/SIGTERM),
	// http.Server.Shutdown stops accepting new connections and DRAINS in-flight
	// requests up to shutdownGrace — so a relay in progress finishes streaming
	// instead of being truncated, and the capture it produces is enqueued AND
	// processed. serveGraceful stops the learner worker (stopWorkers) only AFTER the
	// HTTP drain completes, so captures produced during the drain are not dropped.
	return serveGraceful(ctx, l, s.Handler(), stop)
}

// shutdownGrace bounds how long Shutdown waits for in-flight requests to finish
// before the process exits anyway. A relay to a slow upstream should get a real
// window to complete; a wedged connection must not hang shutdown forever.
const shutdownGrace = 25 * time.Second

// serveGraceful runs an http.Server on l and shuts it down gracefully when ctx is
// cancelled (the signal.NotifyContext in serve). It returns nil on a clean
// shutdown, or the Serve error if the server failed for a reason OTHER than a
// deliberate shutdown. stopSignals releases the signal notification so a SECOND
// Ctrl-C during the drain restores the default (immediate-exit) behavior — a user
// hammering Ctrl-C because a drain is slow gets the hard kill they're asking for.
//
// It is a package function (not inlined) so a test can drive the full
// start→cancel→drain lifecycle against a real listener without a signal.
func serveGraceful(ctx context.Context, l net.Listener, h http.Handler, stopSignals func()) error {
	srv := &http.Server{Handler: h}

	serveErr := make(chan error, 1)
	go func() {
		// Serve returns ErrServerClosed once Shutdown is called; that is the CLEAN
		// path, not a failure.
		serveErr <- srv.Serve(l)
	}()

	select {
	case err := <-serveErr:
		// The server stopped on its own (listener died / fatal error) before any
		// shutdown signal. Surface it — ErrServerClosed shouldn't happen here (no
		// Shutdown yet) but is treated as clean if it does.
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		// Signal received. Stop catching further signals so a second Ctrl-C hard-kills
		// (the user asked twice). Then drain in-flight requests up to shutdownGrace.
		stopSignals()
		fmt.Fprintf(os.Stderr, "oikos: shutting down (draining in-flight requests, up to %s)…\n", shutdownGrace)

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			// Drain timed out (a wedged connection) — force-close the rest so the
			// process can exit. This is the ONLY path that abandons a relay, and only
			// after a full grace window.
			fmt.Fprintf(os.Stderr, "oikos: graceful drain did not finish within %s (%v); closing remaining connections\n", shutdownGrace, err)
			_ = srv.Close()
		}
		// Reap the Serve goroutine's return (ErrServerClosed after Shutdown/Close).
		<-serveErr
		return nil
	}
}

// startSelfHeal arms the config-drift self-heal watcher for the wired base_url
// tools, off the request hot path. It loads the persisted config, builds the
// heal targets (only base_url tools with a present, repairable config file), and
// starts the watcher bound to ctx. It is best-effort: a watcher start failure is
// logged and ignored — the proxy must still serve even if self-heal can't arm.
// With nothing to watch it is a silent no-op.
func startSelfHeal(ctx context.Context) {
	cfg, err := config.Load()
	if err != nil {
		return // no readable config ⇒ nothing wired ⇒ nothing to heal
	}
	// A successful heal re-points a tool the user may have deliberately reverted to
	// the vendor default; surface it on stderr with the exact undo command so it is
	// never a silent fight (P1). The sink fires only on an actual heal, not on a
	// healthy/user-overridden config.
	targets := wire.HealTargetsWithSink(cfg, nil, func(tool, path string) {
		fmt.Fprintln(os.Stderr, wire.HealNotice(tool, path))
	})
	if len(targets) == 0 {
		return
	}
	w := heal.New(targets)
	// Surface heal-pass errors (chiefly an unparseable IDE config) on stderr so an
	// un-healable config the user thinks oikos is guarding is VISIBLE, not silently
	// swallowed by the background watcher (P1).
	w.SetOnError(func(err error) {
		fmt.Fprintf(os.Stderr, "oikos: config-drift self-heal could not heal a config: %v\n", err)
	})
	// RESPECTS-UNWIRE (P1): the heal targets are built once here at boot, but the
	// user may `oikos unwire <tool>` on the RUNNING daemon. Give the watcher a live
	// view of the wired-tools set (mtime-cached read of config.json) so it stops
	// healing a tool the moment unwire removes it — no restart, no fighting the undo.
	w.SetLiveTools(wire.LiveWiredTools())
	// Live kill-switch: a flag file next to the config disables healing without a
	// restart (checked on every pass). Best-effort — if we can't resolve the config
	// dir we simply run without a live switch (OIKOS_NO_HEAL still works at boot).
	if flag := healDisableFlagPath(); flag != "" {
		w.SetDisableFlagPath(flag)
	}
	if err := w.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "oikos: config-drift self-heal could not start (continuing without it): %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "oikos: config-drift self-heal watching %d IDE config file(s) "+
		"(pause anytime: touch %s)\n", len(targets), healDisableFlagPath())
}

// healDisableFlagPath is the live kill-switch file. While it exists, the self-heal
// watcher heals nothing (no restart needed). It sits beside the persisted config
// (same directory as config.Path), so it travels with the user's oikos state and
// needs no extra configuration. Returns "" if the config path can't be resolved.
func healDisableFlagPath() string {
	p, err := config.Path()
	if err != nil || p == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(p), "heal.disabled")
}

// envTruthy reports whether an env value means "on" (1/true/yes).
func envTruthy(v string) bool {
	v = strings.TrimSpace(v)
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

// nativeFileToolsFromConfig returns the persisted native-file wired tools as
// emit.Tool targets, read DIRECTLY off the config (no comma-delimited string in
// between). base_url tools have no native file and are skipped. A comma in a
// NativeFile path (a Windows path, a quoted dir) is carried through intact —
// the pre-P2-3 code joined these with commas into OIKOS_NATIVE_FILE_TOOLS and the
// re-parse dropped the tool after the comma.
func nativeFileToolsFromConfig(cfg config.Config) []emit.Tool {
	var out []emit.Tool
	for _, t := range cfg.WiredTools {
		if t.Channel == "native_file" && t.NativeFile != "" {
			out = append(out, emit.Tool{Name: t.Name, NativeFile: t.NativeFile})
		}
	}
	return out
}

// nativeFileToolsForServe resolves the native-file emit targets for `oikos serve`
// with precedence: an explicit OIKOS_NATIVE_FILE_TOOLS env override wins (the
// documented external knob, legacy `name=path,name=path` form), else the wired
// tools are taken DIRECTLY from the persisted config. Resolving from the config
// avoids the comma round-trip entirely, so a path containing a comma survives
// (P2-3). Empty ⇒ no native-file emission.
func nativeFileToolsForServe(cfg config.Config) []emit.Tool {
	if env := nativeFileToolsFromEnv(); len(env) > 0 {
		return env
	}
	return nativeFileToolsFromConfig(cfg)
}

// resolveProviderChoice is the pure provider-selection decision shared by startup
// and the P0-3 hot-reload path. Precedence:
//  1. fixedUpstream (OIKOS_UPSTREAM_BASE set) → a FIXED upstream (no real dial);
//     carry the key so an authenticated fixed gateway still gets Authorization.
//  2. cfgProvider == "local" → force the keyless local-detection path EVEN IF an
//     OpenRouter key happens to be stored — honour the user's "local" choice and
//     never silently route it to the cloud.
//  3. otherwise → the key if one is present, else keyless local detection.
func resolveProviderChoice(fixedUpstream bool, cfgProvider, key string) upstream.Provider {
	switch {
	case fixedUpstream:
		return &upstream.SingleUpstream{
			Key:    key,
			Detect: func(string) bool { return true }, // no real dial: upstream is fixed
		}
	case cfgProvider == "local":
		return &upstream.SingleUpstream{} // keyless: local detection only
	case key != "":
		return &upstream.SingleUpstream{Key: key}
	default:
		return &upstream.SingleUpstream{}
	}
}

// detectLocalLLM probes the two supported local backends (Ollama :11434, then
// LM Studio :1234) for the /setup page so it can pre-select the zero-key path.
// It reuses the upstream provider's resolver so detection logic lives in ONE
// place. A keyless SingleUpstream resolves to a local LLM when one is reachable.
func detectLocalLLM() (string, bool) {
	u, err := (&upstream.SingleUpstream{}).Select(context.Background())
	if err != nil {
		return "", false
	}
	switch u.Kind {
	case "ollama", "lmstudio":
		return u.Kind, true
	default:
		return "", false
	}
}

// extractConfigFromEnv builds the T2 extract gate config from the environment
// (M3-R6). T2 is DEFAULT OFF: OIKOS_EXTRACT_ALLOW_CLOUD must be a truthy value
// to enable it, AND a Gate must be wired. In v1 the cloud Gate is intentionally
// left nil (no metered-API caller is shipped here — local-preferred T2 + the
// real Gate wiring land in a follow-up); so even with allow_cloud=true, T2 is a
// no-op until a Gate is configured. This keeps "no silent cloud upload" true by
// construction. OIKOS_EXTRACT_COST_CAP sets the visible per-day cost cap.
func extractConfigFromEnv() extract.Config {
	cfg := extract.Config{}
	if v := strings.TrimSpace(os.Getenv("OIKOS_EXTRACT_ALLOW_CLOUD")); v != "" {
		cfg.AllowCloud = v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
	}
	if v := os.Getenv("OIKOS_EXTRACT_COST_CAP"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.CostCapPerDay = f
		}
	}
	return cfg
}

// nativeFileToolsFromEnv parses OIKOS_NATIVE_FILE_TOOLS — a comma-separated list
// of `name=path` pairs (e.g. `claude-code=./CLAUDE.md,codex=./AGENTS.md`) — into
// the wired NativeFileEmitter targets (opt-in per tool, §5.4 B-6). Empty/unset ⇒
// no native-file emission (the default).
func nativeFileToolsFromEnv() []emit.Tool {
	raw := strings.TrimSpace(os.Getenv("OIKOS_NATIVE_FILE_TOOLS"))
	if raw == "" {
		return nil
	}
	var out []emit.Tool
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, path, ok := strings.Cut(pair, "=")
		name, path = strings.TrimSpace(name), strings.TrimSpace(path)
		if !ok || name == "" || path == "" {
			continue
		}
		out = append(out, emit.Tool{Name: name, NativeFile: path})
	}
	return out
}
