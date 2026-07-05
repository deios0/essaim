// Package server implements the oikosd loopback HTTP proxy.
//
// The server binds 127.0.0.1:4141 and exposes:
//
//	GET  /health                always open, no auth
//	POST /v1/chat/completions   verbatim streaming pass-through
//	POST /v1/completions         plain forward/bypass (no injection)
//	GET  /v1/models             upstream model list, cached 60s
//
// Construction touches no disk (the purity invariant). Upstream resolution and
// the loopback-token gate are wired in lazily by the caller.
package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"syscall"
	"time"

	"oikos/internal/secret"
	"oikos/internal/upstream"
)

// Server is the oikosd proxy. The zero value is not usable; construct with New.
type Server struct {
	addr string
	mux  *http.ServeMux

	// token, when non-empty, engages the requireToken middleware that gates
	// /v1/* behind an "Authorization: Bearer <token>" check. Empty (the
	// default) means single-user-host trust: no gate. /health is always open.
	token string

	// provider resolves the BYOK upstream (OpenRouter or local LLM). It is guarded
	// by providerMu because the P0-3 hot-reload path (a /setup POST) can replace it
	// concurrently with in-flight requests reading it.
	providerMu sync.RWMutex
	provider   upstream.Provider

	// httpClient is the server-owned client used for ALL upstream calls (relay
	// and models). Its transport sets DisableCompression so the upstream
	// Content-Encoding is preserved byte-for-byte (no Accept-Encoding injection,
	// no auto-decompress) — required for the verbatim-relay invariant. It has a
	// ResponseHeaderTimeout (not a Client.Timeout, which would kill legitimate
	// long SSE streams).
	httpClient *http.Client

	// upstreamBaseOverride is a test seam: when set, relay targets this base
	// URL instead of the resolved upstream's BaseURL.
	upstreamBaseOverride string

	// models response cache (Task 6).
	models modelsCache
	// now is injectable for deterministic cache-expiry tests; defaults to
	// time.Now.
	now func() time.Time

	// inj is the B1 injection layer (rule store + bloat guard + degraded state).
	// nil ⇒ no vault configured ⇒ pass-through (no injection), the clean default.
	inj *injector

	// captureSink consumes finished Captures off the response path (M3). nil ⇒
	// no capture (the M2 default — pure verbatim relay). Set via SetCaptureSink.
	captureSink CaptureSink
	// capture holds the /health-surfaced capture meters.
	capture captureCounters

	// fileEmitTools is the set of tool identities wired to the NativeFileEmitter
	// channel (one channel per tool, §5.3). When a request's tool identity is in
	// this set, shouldProxyInject returns false so the proxy never double-injects
	// with the native-file path. Populated via SetFileEmitTools.
	fileEmitTools map[string]bool

	// setupSecret is the credential store the /setup POST handler writes a pasted
	// BYOK key into (the key never lands in the config file). nil ⇒ setup can
	// still record the provider choice but cannot store a key. Set via
	// SetSecretStore.
	setupSecret secret.Store
	// setupDetect is the local-LLM probe the /setup page uses to pre-select the
	// zero-key path. nil ⇒ no local LLM reported. Set via SetSetupDetect.
	setupDetect func() (kind string, present bool)
	// setupValidateKey is the live key-validation seam (P0-1): the /setup/model
	// POST calls it BEFORE persisting an OpenRouter key, so a bad key is rejected
	// loudly instead of being silently saved (no silent green). nil ⇒ the default
	// real validator (GET {upstream}/v1/models with the key) is used. The call
	// targets ONLY the user's chosen upstream — never a new phone-home.
	setupValidateKey func(ctx context.Context, key string) error
	// onProviderUpdate is invoked after a successful /setup/model POST so the
	// running proxy re-resolves its provider/key WITHOUT a restart (P0-3
	// hot-reload). nil ⇒ no hot-reload (the proxy picks up the change on the next
	// restart, the pre-P0-3 behaviour). Set via SetOnProviderUpdate.
	onProviderUpdate func()
}

type modelsCache struct {
	mu   sync.Mutex
	at   time.Time
	body []byte
	code int
}

// New constructs a Server with all routes registered. It creates no files.
func New(addr string) *Server {
	s := &Server{
		addr:     addr,
		mux:      http.NewServeMux(),
		now:      time.Now,
		provider: &upstream.SingleUpstream{}, // lazy default: no nil-deref before SetProvider
		httpClient: &http.Client{
			Transport: &http.Transport{
				// Preserve upstream Content-Encoding verbatim: do not inject
				// Accept-Encoding: gzip and do not auto-decompress.
				DisableCompression:    true,
				ResponseHeaderTimeout: 30 * time.Second,
				DialContext: (&net.Dialer{
					Timeout: 10 * time.Second,
				}).DialContext,
				IdleConnTimeout: 90 * time.Second,
			},
			// No Client.Timeout: a hard deadline would kill legitimate long SSE
			// streams. Upstream-stall protection is the per-call
			// ResponseHeaderTimeout above.
		},
	}
	s.mux.HandleFunc("/health", s.health)
	s.mux.HandleFunc("/v1/chat/completions", s.chatCompletions)
	s.mux.HandleFunc("/v1/completions", s.completions)
	s.mux.HandleFunc("/v1/models", s.modelsHandler)
	s.registerSetupRoutes()
	return s
}

// SetToken engages the loopback bearer-token gate on /v1/* (amendment 1:
// opt-in via --require-token). An empty token leaves the gate disabled.
func (s *Server) SetToken(token string) { s.token = token }

// SetInjector wires the B1 injection layer. A nil injector leaves the proxy as a
// pure verbatim pass-through (no vault, no rules). Used by cmd/oikos to enable
// injection when OIKOS_VAULT is set, and by tests to inject a configured store.
func (s *Server) SetInjector(in *injector) { s.inj = in }

// SetProvider sets the upstream provider used to resolve the BYOK target. It is
// safe to call concurrently with in-flight requests (the P0-3 hot-reload path).
func (s *Server) SetProvider(p upstream.Provider) {
	s.providerMu.Lock()
	s.provider = p
	s.providerMu.Unlock()
}

// currentProvider returns the live provider under the read lock — the single
// read seam the request handlers use so a concurrent SetProvider (hot-reload) is
// race-clean.
func (s *Server) currentProvider() upstream.Provider {
	s.providerMu.RLock()
	p := s.provider
	s.providerMu.RUnlock()
	return p
}

// SetKeyValidator overrides the live key-validation used by /setup/model
// (P0-1). Test seam: the default validator does a real GET {upstream}/v1/models
// with the pasted key; tests inject a deterministic stand-in.
func (s *Server) SetKeyValidator(fn func(ctx context.Context, key string) error) {
	s.setupValidateKey = fn
}

// SetOnProviderUpdate registers a callback invoked after a successful
// /setup/model POST so the running proxy re-resolves its provider/key live,
// without a process restart (P0-3 hot-reload). cmd/oikos wires this to re-read
// the credential store and call SetProvider.
func (s *Server) SetOnProviderUpdate(fn func()) { s.onProviderUpdate = fn }

// SetCaptureSink wires the M3 response-side capture sink (off the hot path). A
// nil sink leaves the proxy as a pure verbatim relay (the M2 default).
func (s *Server) SetCaptureSink(sink CaptureSink) { s.captureSink = sink }

// SetFileEmitTools records which tool identities are wired to the
// NativeFileEmitter channel, so the proxy stays out of those tools' context
// (one channel per tool, §5.3).
func (s *Server) SetFileEmitTools(tools map[string]bool) { s.fileEmitTools = tools }

// SetUpstreamBase overrides the resolved upstream's base URL for ALL upstream
// calls (chat, completions, models). Empty leaves the provider-resolved base in
// place. This is how the demo (and any "point oikos at a specific OpenAI-
// compatible endpoint" use) targets a fixed upstream.
func (s *Server) SetUpstreamBase(base string) { s.upstreamBaseOverride = base }

// Handler returns the http.Handler for the server, wrapping the mux in the
// loopback-token middleware. When no token is set the middleware is a
// pass-through, so /v1/* is open under single-user-host trust.
func (s *Server) Handler() http.Handler {
	return s.requireToken(s.mux)
}

// Listen binds the configured address. On EADDRINUSE it returns a loud,
// actionable error instead of silently falling back to an ephemeral port.
func (s *Server) Listen() (net.Listener, error) {
	l, err := net.Listen("tcp", s.addr)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, errors.New("oikos: port " + s.addr + " is already in use — stop the other process or run with --auto-port")
		}
		return nil, err
	}
	return l, nil
}
