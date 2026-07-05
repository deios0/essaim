package server

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"oikos/internal/inject"
	"oikos/internal/rules"
)

// interceptDeadline is the BASE in-mem budget for the WHOLE request-side
// transform (strip + match + render + splice + serialize). On overrun the
// transform fails open and the ORIGINAL bytes are forwarded verbatim (spec §5.1,
// §5.2). 15ms suits the typical (<KB..few-hundred-KB) body; multi-MB bodies get a
// modestly-scaled budget via scaleDeadline (P2-1) so a large resent history still
// injects instead of failing open exactly when the session is biggest.
const interceptDeadline = 15 * time.Millisecond

// perMByteDeadline is the extra budget granted per megabyte of request body
// (P2-1). A multi-MB body genuinely takes a few ms more to parse+splice; without
// this the flat base fails open (zero injection) past ~8MB. It is tiny next to the
// upstream RTT (hundreds of ms), so scaling it up never delays the client
// meaningfully — it only prevents a silent no-injection on big sessions.
const perMByteDeadline = 6 * time.Millisecond

// maxInterceptDeadline caps the scaled budget so a pathological body can never
// stall the request path beyond a modest ceiling (P2-1). ~100ms is still an order
// of magnitude below a normal upstream first-token latency.
const maxInterceptDeadline = 100 * time.Millisecond

// scaleDeadline returns the intercept budget for a body of bodyLen bytes: the base
// plus perMByteDeadline for each whole megabyte, capped at maxInterceptDeadline
// (P2-1). Small bodies keep the base exactly. It is pure and monotonic in bodyLen.
func scaleDeadline(base time.Duration, bodyLen int) time.Duration {
	if bodyLen <= 0 {
		return base
	}
	mb := bodyLen >> 20 // whole megabytes
	dl := base + time.Duration(mb)*perMByteDeadline
	if dl > maxInterceptDeadline {
		return maxInterceptDeadline
	}
	if dl < base {
		return base // overflow guard
	}
	return dl
}

// degradedWindow is how long /health reports degraded:true after the last
// degrading event (ErrDeadline / ErrPanic / ErrOverflow) (spec §5.5).
const degradedWindow = 60 * time.Second

// Build-side error sentinels (spec §5.5). Only ErrDeadline/ErrPanic/ErrOverflow
// mark the server degraded; ErrIndexEmpty/ErrNoMatch are honest misses.
var (
	errDeadline = errors.New("oikos: intercept deadline exceeded")
	errPanic    = errors.New("oikos: intercept panic recovered")
)

// injector holds the rule store + bloat-guard config + degraded state. It is the
// server's handle on the B1 mechanic. A nil store (no OIKOS_VAULT) means no
// rules ⇒ no injection, cleanly.
type injector struct {
	store *rules.Store
	cfg   rules.GuardConfig

	// injectUnsupported, when non-nil and true for the request's model, means the
	// resolved target would 400 on an extra leading instruction message (B1 v1.1
	// A-2.3 static config, primary source). Injection is SKIPPED entirely:
	// verbatim origBody forwarded, no strip, no capture, degraded=false. nil ⇒
	// every target is assumed to accept the injected element (the common case).
	injectUnsupported func(model string) bool

	// deadline overrides interceptDeadline (0 ⇒ the const). Test seam.
	deadline time.Duration
	// buildHook, when non-nil, is invoked at the START of buildOnce inside the
	// deadline+recover scope. Test seam to force a slow build (deadline) or a
	// panic (recover) without touching production paths.
	buildHook func()

	mu         sync.Mutex
	degradedAt time.Time
	now        func() time.Time
}

// NewWithInjection constructs a Server with the B1 injection layer wired from
// the environment (OIKOS_VAULT + bloat-guard knobs). When OIKOS_VAULT is unset
// the injector is nil and the proxy is a pure verbatim pass-through. The
// fsnotify watcher runs for the lifetime of ctx.
func NewWithInjection(ctx context.Context, addr string) (*Server, error) {
	s := New(addr)
	in, err := newInjector(ctx)
	if err != nil {
		return nil, err
	}
	s.SetInjector(in)
	return s, nil
}

// newInjector constructs an injector from the environment. OIKOS_VAULT selects
// the vault dir (empty ⇒ no rules); OIKOS_EAGER_BYTES/OIKOS_TOP_K/
// OIKOS_MATCH_FLOOR tune the bloat guard. It loads the initial index and starts
// the fsnotify watcher (off the request path). Returns nil (not an error) when
// no vault is configured, so the proxy degrades cleanly to pass-through.
func newInjector(ctx context.Context) (*injector, error) {
	vault := os.Getenv("OIKOS_VAULT")
	if vault == "" {
		return nil, nil // no vault → no injection (clean skip)
	}
	store, err := rules.NewStore(vault)
	if err != nil {
		return nil, err
	}
	if err := store.Watch(ctx); err != nil {
		return nil, err
	}
	inj := &injector{
		store:             store,
		cfg:               rules.GuardConfigFromEnv(),
		injectUnsupported: injectUnsupportedFromEnv(),
		now:               time.Now,
	}
	return inj, nil
}

// injectUnsupportedFromEnv builds the static "this model would 400 on an extra
// instruction message" predicate from OIKOS_INJECT_UNSUPPORTED_MODELS — a
// comma-separated list of case-insensitive model-id prefixes (e.g. a strict
// local upstream's model). It mirrors the per-base_url/per-model
// `inject_unsupported` recorded at wire time (B1 v1.1 A-2.3 static config,
// primary). Empty/unset ⇒ nil (no model is skipped).
func injectUnsupportedFromEnv() func(string) bool {
	raw := strings.TrimSpace(os.Getenv("OIKOS_INJECT_UNSUPPORTED_MODELS"))
	if raw == "" {
		return nil
	}
	var prefixes []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			prefixes = append(prefixes, p)
		}
	}
	if len(prefixes) == 0 {
		return nil
	}
	return func(model string) bool {
		m := strings.ToLower(strings.TrimSpace(model))
		// Normalize a leading vendor/ segment (A-2.2 glob discipline).
		if i := strings.LastIndexByte(m, '/'); i >= 0 {
			m = m[i+1:]
		}
		for _, p := range prefixes {
			if strings.HasPrefix(m, p) {
				return true
			}
		}
		return false
	}
}

// newInjectorWithStore wraps an already-built rule store in an injector with the
// given guard config. Used by tests to construct a hermetic injector without the
// environment/watcher. now defaults to time.Now.
func newInjectorWithStore(store *rules.Store, cfg rules.GuardConfig) *injector {
	return &injector{store: store, cfg: cfg, now: time.Now}
}

// markDegraded records a degrading event for the sticky /health window.
func (in *injector) markDegraded() {
	in.mu.Lock()
	in.degradedAt = in.now()
	in.mu.Unlock()
}

// degraded reports whether the server is within the sticky degraded window OR
// the rule store is serving a stale (last-good) index after a transient FS error
// (P1-4). Either condition means injection is not running on the freshest vault.
func (in *injector) degraded() bool {
	if in == nil {
		return false
	}
	if in.store != nil && in.store.Degraded() {
		return true
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.degradedAt.IsZero() {
		return false
	}
	return in.now().Sub(in.degradedAt) < degradedWindow
}

// rulesIndexed reports the current number of indexed rules (for /health).
func (in *injector) rulesIndexed() int {
	if in == nil || in.store == nil {
		return 0
	}
	return in.store.Index().Len()
}

// watcherDegraded reports whether the vault's poll fallback has had to re-index a
// change fsnotify never delivered (P0-4) — i.e. inotify is dead on this
// filesystem (WSL2 /mnt/c, network FS) and live reload is running on the slower
// poll path. Surfaced on /health so a silently-degraded watcher is observable.
func (in *injector) watcherDegraded() bool {
	if in == nil || in.store == nil {
		return false
	}
	return in.store.WatcherDegraded()
}

// Store exposes the rule store so cmd/oikos can register the NativeFileEmitter's
// onSwap callback (the seam the Store already exposes) and read the vault dir.
// nil when no vault is configured.
func (s *Server) Store() *rules.Store {
	if s.inj == nil {
		return nil
	}
	return s.inj.store
}

// VaultDir returns the configured vault directory ("" when no vault).
func (s *Server) VaultDir() string {
	if s.inj == nil || s.inj.store == nil {
		return ""
	}
	return s.inj.store.Dir()
}

// safeBuild runs the ENTIRE request-side transform under ONE deadline AND ONE
// recover (spec §5.1, F-8/F-9). On success it returns the rewritten body + the
// capture snapshot. On ErrIndexEmpty/ErrNoMatch it returns (origBody, snapshot,
// honest-miss-error) — NOT degraded. On ErrDeadline/ErrPanic it returns
// (origBody, zero-snapshot, degrading-error) so the caller forwards verbatim and
// marks degraded. The returned body is ALWAYS safe to forward.
func (in *injector) safeBuild(ctx context.Context, origBody []byte) (body []byte, snap inject.Snapshot, err error) {
	if in == nil || in.store == nil {
		return origBody, inject.Snapshot{}, rules.ErrIndexEmpty
	}
	ix := in.store.Index()
	if ix.Len() == 0 {
		return origBody, inject.Snapshot{}, rules.ErrIndexEmpty
	}

	dl := in.deadline
	if dl == 0 {
		// Scale the base with body size (P2-1): a multi-MB body gets a modestly
		// larger budget so it still injects instead of failing open. A test-set
		// in.deadline is an explicit override and is used verbatim (not scaled).
		dl = scaleDeadline(interceptDeadline, len(origBody))
	}
	dctx, cancel := context.WithTimeout(ctx, dl)
	defer cancel()

	type result struct {
		body []byte
		snap inject.Snapshot
		err  error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- result{err: errPanic}
			}
		}()
		// buildOnce is cancellation-aware: it checks dctx.Err() between phases so it
		// STOPS parsing/matching the (possibly multi-MB) body the moment the deadline
		// fires — no detached CPU burn after safeBuild has already failed open (P2-1).
		b, s, e := in.buildOnce(dctx, origBody, ix)
		done <- result{body: b, snap: s, err: e}
	}()

	select {
	case <-dctx.Done():
		// Deadline (or client cancel) hit BEFORE the transform finished →
		// fail-open to verbatim original bytes, degraded.
		return origBody, inject.Snapshot{}, errDeadline
	case res := <-done:
		if res.err != nil {
			// buildOnce returned an honest-miss (ErrNoMatch) or panic sentinel.
			if errors.Is(res.err, errPanic) {
				return origBody, inject.Snapshot{}, errPanic
			}
			// OUR intercept deadline firing is a DEGRADING event — forward verbatim +
			// mark degraded (same as the <-dctx.Done() branch).
			if errors.Is(res.err, context.DeadlineExceeded) {
				return origBody, inject.Snapshot{}, errDeadline
			}
			// The PARENT request context being canceled means the CLIENT disconnected,
			// not that the server was slow — forward verbatim but do NOT mark the
			// server degraded for a client hangup (gemini review).
			if errors.Is(res.err, context.Canceled) {
				return origBody, inject.Snapshot{}, res.err
			}
			// ErrNoMatch / ErrSkip → forward the (possibly stripped) body honestly.
			return res.body, res.snap, res.err
		}
		return res.body, res.snap, nil
	}
}

// buildOnce is the pure transform: match over the last user message → bloat
// guard → inject.Build (strip+splice). It returns the rewritten body + snapshot,
// or (origBody, snapshot/zero, ErrNoMatch/ErrSkip) on an honest miss. It does NO
// I/O and NO mutation of durable state (spec §7).
//
// It is CANCELLATION-AWARE (P2-1): it checks ctx.Err() between the expensive
// phases (parse → match → build), so when safeBuild's deadline fires it STOPS
// scanning the (possibly multi-MB) body instead of burning CPU detached from the
// already-failed-open request. On cancellation it returns the VERBATIM origBody +
// the ctx error (safeBuild maps that to the degrading errDeadline). The checks sit
// only at phase boundaries, so the fast path pays a single, negligible ctx.Err().
func (in *injector) buildOnce(ctx context.Context, origBody []byte, ix *rules.Index) ([]byte, inject.Snapshot, error) {
	if in.buildHook != nil {
		in.buildHook() // test seam: force a slow build (deadline) or a panic
	}

	// Phase gate 0: if the deadline already fired (e.g. buildHook simulated a slow
	// build), bail before any parse work — verbatim, ctx error.
	if err := ctx.Err(); err != nil {
		return origBody, inject.Snapshot{}, err
	}

	// SKIP-on-unsupported gate (B1 v1.1 A-2.3 / A-2.5): runs BEFORE any strip or
	// wire mutation. If the target model would 400 on an extra leading
	// instruction message, forward the VERBATIM origBody (including any stale
	// oikos block — strip is NOT run), enqueue no capture, classify NOT degraded.
	// Never turn a working request into a 400. The model is read with object-level
	// key discipline (A-2.1 / F-V7), so a `"model"` substring inside user content
	// can't trigger a false skip.
	if in.injectUnsupported != nil {
		if model := inject.Model(origBody); in.injectUnsupported(model) {
			return origBody, inject.Snapshot{}, inject.ErrInjectUnsupported
		}
	}

	// P0-1: parse the (possibly multi-MB) body ONCE. The match query AND the
	// strip/splice both read this single parse — the prior code parsed the body
	// twice (LastUserMessage + Build), doubling the hot-path scan cost on large
	// bodies and silently overrunning the 15ms deadline (fail-open, no injection).
	parsed := inject.Parse(origBody)
	query := capMatchQuery(parsed.LastUser())

	// Phase gate 1: after the (expensive) parse, before the match. If the deadline
	// fired during parse, stop here — do not also run the match+build.
	if err := ctx.Err(); err != nil {
		return origBody, inject.Snapshot{}, err
	}

	var matched []rules.Rule
	var ids []string
	if query != "" {
		res, err := ix.MatchAndGuard(query, in.cfg)
		if err == nil {
			matched = res.Kept
			ids = make([]string, len(matched))
			for i, r := range matched {
				ids[i] = r.ID
			}
		}
		// ErrNoMatch/ErrIndexEmpty → matched stays nil; inject.Build still strips
		// a stale block (empty-match → stripped-only).
	}

	// Phase gate 2: after the match, before the build (strip+splice). If the
	// deadline fired during the match, stop before rewriting the big body.
	if err := ctx.Err(); err != nil {
		return origBody, inject.Snapshot{}, err
	}

	out, err := parsed.Build(matched, ids)
	if errors.Is(err, inject.ErrSkip) {
		// No messages array / empty → forward verbatim, no capture.
		return origBody, inject.Snapshot{}, inject.ErrSkip
	}
	if err != nil {
		return origBody, inject.Snapshot{}, err
	}
	if len(matched) == 0 {
		return out.Body, out.Snapshot, rules.ErrNoMatch
	}
	return out.Body, out.Snapshot, nil
}

// maxMatchQueryBytes bounds how much of the last user message is fed to the
// lexical match. An agent session can send a multi-MB last message (resent
// history + pasted files); tokenizing all of it (a word+trigram alloc per
// 3-rune window) overruns the intercept deadline → the request fails open with
// ZERO injection exactly when the session is largest (P1). Build() still injects
// into the full body; only the MATCH query is capped.
const maxMatchQueryBytes = 16384

// capMatchQuery bounds q to maxMatchQueryBytes, keeping BOTH ends: the HEAD (an
// initial instruction) and the TAIL (the final question), where a distinctive
// match term realistically sits — only the bulk middle is dropped. It slices on
// UTF-8 rune boundaries WITHOUT materializing a []rune of the whole (possibly
// multi-MB) string: it only inspects the bytes near each cut, so the cap itself
// is O(budget), not O(len) — never re-introducing the very O(N) blow-up it exists
// to prevent (gemini review). Short queries pass through unchanged.
func capMatchQuery(q string) string {
	if len(q) <= maxMatchQueryBytes {
		return q
	}
	half := maxMatchQueryBytes / 2
	// Head: [0, headEnd), backed up from ~half to the start of a rune.
	headEnd := half
	for headEnd > 0 && !utf8.RuneStart(q[headEnd]) {
		headEnd--
	}
	// Tail: [tailStart, len), advanced from ~half-before-end to the start of a rune.
	tailStart := len(q) - half
	for tailStart < len(q) && !utf8.RuneStart(q[tailStart]) {
		tailStart++
	}
	// Join with a space so tokens at the cut can't fuse across the gap.
	return q[:headEnd] + " " + q[tailStart:]
}
