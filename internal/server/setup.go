package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"oikos/internal/config"
	"oikos/internal/secret"
	"oikos/internal/wire"
)

var (
	errEmptyKey      = errors.New("an OpenRouter key is required when provider is \"openrouter\"")
	errNoSecretStore = errors.New("oikos: no credential store available to hold the key")
)

// WedgePitch is the ONE differentiating sentence oikos leads with everywhere:
// the /setup page hero and the `oikos serve` first-run banner both render this
// exact text, so the lead message can never drift between surfaces.
//
// The pitch RIDES the AGENTS.md standard rather than competing with it: a static
// AGENTS.md gives "one rule → all tools" for free, but you hand-write it and it
// never learns. oikos's reason-to-exist is that it AUTO-WRITES and MAINTAINS that
// file from your AI corrections — the preference you teach once stays current in
// every tool's native rules file. Kept in the indie-dev voice (no
// compliance/governance/enterprise framing — oikos is free for everyone).
const WedgePitch = "oikos auto-writes & keeps your AGENTS.md current from your AI corrections — teach a preference once, it stays live in every tool."

// setupHTML is the ONE self-contained first-run page (spec §1). It is embedded
// at build time so the binary ships it — no external network resources, no
// template engine, no JS framework. Loopback-only.
//
//go:embed setup.html
var setupHTML string

// setupPageHTML is the served page: the embedded template with the {{WEDGE_PITCH}}
// placeholder resolved to the WedgePitch constant (single source of truth, so the
// pitch text can never drift from the CLI banner). Computed once at init.
var setupPageHTML = strings.ReplaceAll(setupHTML, "{{WEDGE_PITCH}}", WedgePitch)

// SetSecretStore wires the credential store the /setup POST handlers use to
// persist a pasted BYOK key (the key never lands in the config file). nil leaves
// setup unable to store a key (it still persists the provider choice).
func (s *Server) SetSecretStore(store secret.Store) { s.setupSecret = store }

// SetSetupDetect injects the local-LLM probe the /setup page uses to pre-select
// "use the model on this computer". It returns the detected kind (e.g.
// "ollama"/"lmstudio") and whether one is present. nil ⇒ no local LLM reported.
func (s *Server) SetSetupDetect(fn func() (kind string, present bool)) { s.setupDetect = fn }

// registerSetupRoutes adds the first-run setup surface to the mux. Called by New.
// The three STATE-CHANGING endpoints (model/vault/wire) additionally carry the
// same-origin guard: loopbackOnly (RemoteAddr) alone cannot stop a browser on a
// malicious page from POSTing to this localhost daemon (a simple-content-type
// body skips the CORS preflight, and DNS-rebinding forges a loopback RemoteAddr).
func (s *Server) registerSetupRoutes() {
	s.mux.HandleFunc("/setup", s.loopbackOnly(s.setupPage))
	s.mux.HandleFunc("/setup/state", s.loopbackOnly(s.setupState))
	s.mux.HandleFunc("/setup/model", s.loopbackOnly(s.sameOriginOnly(s.setupModel)))
	s.mux.HandleFunc("/setup/vault", s.loopbackOnly(s.sameOriginOnly(s.setupVault)))
	s.mux.HandleFunc("/setup/wire", s.loopbackOnly(s.sameOriginOnly(s.setupWire)))
}

// sameOriginOnly guards a state-changing setup endpoint against browser-driven
// CSRF and DNS-rebinding. loopbackOnly is not enough on its own: a localhost
// daemon is reachable from ANY page in the user's browser, and decodeJSON parses
// the body regardless of Content-Type, so a cross-site "simple" POST (text/plain
// body carrying JSON) bypasses the CORS preflight, while DNS-rebinding lets an
// attacker domain resolve to 127.0.0.1 so RemoteAddr looks loopback. We require
// the request to be genuinely same-host + same-origin:
//   - Host must be a loopback name/IP (a rebind page carries Host = the attacker
//     domain, even though it resolves to 127.0.0.1);
//   - Sec-Fetch-Site (sent by modern browsers) must be same-origin or none —
//     never cross-site/same-site;
//   - Origin, when present, must be a loopback origin.
//
// A non-browser client (the oikos CLI, curl) sends no Origin/Sec-Fetch and a
// loopback Host, so it passes untouched — the guard targets browsers only.
func (s *Server) sameOriginOnly(next http.HandlerFunc) http.HandlerFunc {
	// NOTE on absent headers: a real browser ALWAYS sends Origin on a POST (same-
	// or cross-origin), so a browser CSRF attempt is caught by the Origin/Sec-Fetch
	// checks below; a request with NEITHER header is a non-browser local client
	// (the oikos CLI, curl) on the loopback interface, which is trusted. We
	// therefore fail OPEN only for the header-less local case — the browser and
	// DNS-rebind vectors (non-loopback Host) are still closed.
	return func(w http.ResponseWriter, r *http.Request) {
		if !hostIsLoopback(r.Host) {
			http.Error(w, "oikos setup: cross-host request refused", http.StatusForbidden)
			return
		}
		if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" && sfs != "none" {
			http.Error(w, "oikos setup: cross-site request refused", http.StatusForbidden)
			return
		}
		// Origin, when present, must be the SAME origin as the request's own host
		// (host:port, not merely "some loopback"): a different loopback origin —
		// e.g. a sketchy tool on http://127.0.0.1:9999 or an XSS in another local
		// app — is still cross-origin and must not drive setup (codex review).
		if o := r.Header.Get("Origin"); o != "" && !originMatchesHost(o, r.Host) {
			http.Error(w, "oikos setup: cross-origin request refused", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// hostIsLoopback reports whether a Host header value (host or host:port, with
// optional IPv6 brackets) names the loopback interface. An empty Host is refused
// — a browser always sends one, so absence on a guarded mutation is abnormal.
func hostIsLoopback(hostport string) bool {
	if hostport == "" {
		return false
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	host = strings.TrimSuffix(host, ".") // tolerate the FQDN form "localhost."
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// originMatchesHost reports whether an Origin header value names the SAME origin
// as the request's Host — the host:port of the Origin URL must equal the request
// Host (both already known loopback via the Host guard). This is true
// same-origin: a different loopback PORT is a different origin and is refused.
func originMatchesHost(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, host)
}

// loopbackOnly wraps a handler so it only serves requests originating from the
// loopback interface. The proxy already binds 127.0.0.1, but this is
// defence-in-depth (e.g. a WSL-mirrored loopback or a misconfigured bind) so the
// setup surface — which writes config and reads a key — is never reachable from
// off-host. A request with no parseable remote (httptest default) is treated as
// loopback.
func (s *Server) loopbackOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackRequest(r) {
			http.Error(w, "oikos setup is loopback-only", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func isLoopbackRequest(r *http.Request) bool {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	if host == "" {
		return true // httptest / unix socket: trust the local bind
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// A non-IP RemoteAddr (e.g. "pipe") on a loopback-bound server.
		return true
	}
	return ip.IsLoopback()
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(setupPageHTML))
}

// setupState reports what the page needs to pre-answer the one real choice: is a
// local LLM already running, and what has the user already configured.
func (s *Server) setupState(w http.ResponseWriter, r *http.Request) {
	kind, present := "", false
	if s.setupDetect != nil {
		kind, present = s.setupDetect()
	}
	c, _ := config.Load()
	writeJSON(w, http.StatusOK, map[string]any{
		"local_llm":         kind,
		"local_llm_present": present,
		"provider":          c.Provider,
		"vault_dir":         c.VaultDir,
		"wired_tools":       c.WiredTools,
		"configured":        !c.IsEmpty(),
	})
}

type modelReq struct {
	Provider string `json:"provider"`
	Key      string `json:"key"`
}

// setupModel persists the model choice. "local" records the provider; "openrouter"
// LIVE-VALIDATES the pasted key (GET {upstream}/v1/models) BEFORE doing anything,
// then stores it in the OS keychain (NEVER the config file) and records the
// provider. A key the provider rejects is reported loudly and NOTHING is persisted
// (P0-1 — no silent green). On success the running proxy re-resolves its
// provider/key live (P0-3 — no restart).
func (s *Server) setupModel(w http.ResponseWriter, r *http.Request) {
	var req modelReq
	if !decodeJSON(w, r, &req) {
		return
	}
	switch strings.ToLower(strings.TrimSpace(req.Provider)) {
	case "local":
		// Local model: no key, so no upstream validation call (the local path
		// never touches the cloud upstream).
		if err := s.persistProvider("local"); err != nil {
			writeJSON(w, http.StatusInternalServerError, errBody(err))
			return
		}
		s.notifyProviderUpdate()
	case "openrouter":
		key := strings.TrimSpace(req.Key)
		if key == "" {
			writeJSON(w, http.StatusBadRequest, errBody(errEmptyKey))
			return
		}
		if s.setupSecret == nil {
			writeJSON(w, http.StatusInternalServerError, errBody(errNoSecretStore))
			return
		}
		// P0-1: validate the key against the chosen upstream BEFORE persisting it.
		// A bad key MUST NOT be saved (it would 401 later and the user would blame
		// oikos). The validation call goes ONLY to the user's chosen upstream — it
		// is not a new phone-home.
		if err := s.validateOpenRouterKey(r.Context(), key); err != nil {
			writeJSON(w, http.StatusBadGateway, errBody(err))
			return
		}
		// Snapshot any PRIOR key so a later failure can restore it byte-exact, not
		// just delete the new one (a delete would lose a user's previously-working
		// credential). "" means there was no prior key.
		priorKey, _ := s.setupSecret.Get("openrouter-key")
		if err := s.setupSecret.Set("openrouter-key", key); err != nil {
			// Headless box / no OS Secret Service (the go-keyring "failed to unlock
			// correct collection" case). Surface a ONE-SENTENCE human message + the
			// exact fix (the env-var fallback, P1-6b) — never the raw go-keyring
			// jargon, never a bare 500. The provider is NOT persisted: if the key
			// can't be stored, recording provider=openrouter would 401 the next
			// serve.
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": "oikos can't reach your OS keychain to store the key on this machine. " +
					"Set the OIKOS_OPENROUTER_KEY environment variable to your key and restart oikos instead.",
			})
			return
		}
		if err := s.persistProvider("openrouter"); err != nil {
			// "Persist nothing on failure": the new key was already written to the
			// store above; restore the store to its PRIOR state (the previous key, or
			// delete if there was none) so a failed setup neither orphans the new key
			// nor loses the user's previous working credential.
			restoreStoredKey(s.setupSecret, "openrouter-key", priorKey)
			writeJSON(w, http.StatusInternalServerError, errBody(err))
			return
		}
		// P0-3: the key was validated AND persisted — make the running proxy pick it
		// up live (no restart). Wiring is set by cmd/oikos; nil in tests/no-op.
		s.notifyProviderUpdate()
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "provider must be \"local\" or \"openrouter\"",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// notifyProviderUpdate fires the P0-3 hot-reload hook (re-resolve the running
// proxy's provider/key from the store) when one is wired. Safe with a nil hook.
func (s *Server) notifyProviderUpdate() {
	if s.onProviderUpdate != nil {
		s.onProviderUpdate()
	}
}

// keyDeleter is the optional capability a secret store may expose to remove a key
// (used only on a post-store failure to honour "persist nothing on failure"). A
// store that does not implement it leaves the validated key in place — benign
// (next serve picks it up).
type keyDeleter interface{ Delete(key string) error }

// restoreStoredKey reverts the store entry for `key` to its prior value after a
// failed setup: it re-Sets the previous value when there was one, else deletes the
// just-written key (best-effort — a store that can't delete keeps a benign
// validated key). This both avoids orphaning the NEW key and avoids losing the
// user's PREVIOUS working credential. Errors are ignored (best-effort rollback).
func restoreStoredKey(store secret.Store, key, prior string) {
	if prior != "" {
		_ = store.Set(key, prior) // put the previous working key back
		return
	}
	if d, ok := store.(keyDeleter); ok {
		_ = d.Delete(key)
	}
}

// validateOpenRouterKey runs ONE live validation of key against the user's chosen
// upstream (P0-1): GET {base}/v1/models with the key, short timeout. A non-2xx
// (typically 401) means the provider rejected the key → a clear, actionable error
// the /setup UI shows. A transport failure (upstream unreachable) is surfaced
// honestly too (we cannot confirm the key, so we do not persist). The base is the
// upstream-base override when set, else the OpenRouter API base — NEVER a new
// host, so this is not a phone-home.
func (s *Server) validateOpenRouterKey(ctx context.Context, key string) error {
	if s.setupValidateKey != nil {
		return s.setupValidateKey(ctx, key) // test seam
	}
	return s.liveValidateKey(ctx, key)
}

// openRouterAPIBase is the upstream the BYOK OpenRouter path resolves to (mirrors
// upstream.SingleUpstream.Select). Kept here so the validator targets the SAME
// host the proxy will use — never a different/extra endpoint.
const openRouterAPIBase = "https://openrouter.ai/api"

// liveValidateKey performs the real GET {base}/v1/models with the key. It uses the
// server's own httpClient (DisableCompression, no auto-decompress) under a short
// 8s context timeout so a hung upstream never hangs the /setup POST. The key is
// sent ONLY in the Authorization header to the chosen upstream and is never logged.
func (s *Server) liveValidateKey(ctx context.Context, key string) error {
	base := openRouterAPIBase
	if s.upstreamBaseOverride != "" {
		base = s.upstreamBaseOverride
	}
	vctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	outReq, err := http.NewRequestWithContext(vctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return errors.New("oikos: could not build the validation request — " + err.Error())
	}
	outReq.Header.Set("Authorization", "Bearer "+key)
	resp, err := s.httpClient.Do(outReq)
	if err != nil {
		// Could not reach the provider to validate. Do NOT persist on an unconfirmed
		// key; tell the user plainly (no raw transport jargon as the whole message).
		return errors.New("oikos couldn't reach the model provider to check that key — verify your connection and try again")
	}
	defer resp.Body.Close()
	// Drain a little of the body so the connection can be reused, but never echo it.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<14))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return errors.New("that key was rejected by the provider — check it and paste it again")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Some other non-2xx (e.g. a 5xx): we can't confirm the key is good, so we
		// don't persist it. Keep the message human and actionable.
		return errors.New("the model provider did not accept that key (it returned an error) — check the key and try again")
	}
	return nil
}

type vaultReq struct {
	VaultDir string `json:"vault_dir"`
}

func (s *Server) setupVault(w http.ResponseWriter, r *http.Request) {
	var req vaultReq
	if !decodeJSON(w, r, &req) {
		return
	}
	dir := strings.TrimSpace(req.VaultDir)
	if dir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "vault_dir is required"})
		return
	}
	c, err := config.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody(err))
		return
	}
	c.VaultDir = dir
	if err := config.Save(c); err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type wireReq struct {
	Tool string `json:"tool"`
	Dir  string `json:"dir"`
}

// setupWire runs `oikos wire <tool>` through the same wire package the CLI uses,
// so the UI and the CLI produce identical wiring (channel auto-chosen). It is
// idempotent.
func (s *Server) setupWire(w http.ResponseWriter, r *http.Request) {
	var req wireReq
	if !decodeJSON(w, r, &req) {
		return
	}
	plan, err := wire.Resolve(req.Tool, strings.TrimSpace(req.Dir))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err))
		return
	}
	if _, err := wire.Apply(plan); err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"tool":    plan.Tool,
		"channel": plan.Channel,
		"note":    plan.Note,
	})
}

// persistProvider loads, sets the provider, and saves the config.
func (s *Server) persistProvider(provider string) error {
	c, err := config.Load()
	if err != nil {
		return err
	}
	c.Provider = provider
	return config.Save(c)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body: " + err.Error()})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func errBody(err error) map[string]any { return map[string]any{"error": err.Error()} }
