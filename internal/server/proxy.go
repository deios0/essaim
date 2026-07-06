package server

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"essaim/internal/inject"
	"essaim/internal/upstream"
)

// contextOverflowSignatures are the upstream error-body markers that mean the
// request exceeded the model's context window (spec §5.4 CONTEXT_OVERFLOW_PATTERN).
// Matched case-insensitively against an ERROR-status response body only (never a
// success/stream body, which is relayed verbatim and never buffered).
var contextOverflowSignatures = []string{
	"context_length_exceeded",
	"maximum context length",
	"context length exceeded",
}

// bodyIsContextOverflow reports whether an upstream error-response body matches a
// context-overflow signature (spec §5.4).
func bodyIsContextOverflow(body []byte) bool {
	low := strings.ToLower(string(body))
	for _, sig := range contextOverflowSignatures {
		if strings.Contains(low, sig) {
			return true
		}
	}
	return false
}

// hopByHopHeaders are the RFC 7230 §6.1 connection-specific headers that a proxy
// MUST NOT forward end-to-end. removeHopByHop strips these (plus any header named
// in the Connection header value) from h, in place.
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// removeHopByHop deletes the standard hop-by-hop headers from h, as well as any
// header explicitly named in the Connection header value (RFC 7230 §6.1). It is
// applied to the OUTBOUND upstream request headers and to the COPIED upstream
// response headers before they are written to the client.
func removeHopByHop(h http.Header) {
	// Headers named in the Connection value are also hop-by-hop.
	for _, conn := range h.Values("Connection") {
		for _, f := range strings.Split(conn, ",") {
			if name := strings.TrimSpace(f); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
}

// chatCompletions handles POST /v1/chat/completions. It resolves the upstream
// (zero-key → a 401 with a setup deep-link), runs the B1 request-side injection
// (strip-then-inject one leading essaim rule block, fail-open to the verbatim
// original bytes on any overrun/panic), then relays the upstream response
// VERBATIM (byte-for-byte, per-chunk flush). The response stream is NEVER
// mutated — injection is request-side only (spec §B1).
func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	up, err := s.currentProvider().Select(r.Context())
	if err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, err.Error()+" — open http://127.0.0.1:4141/setup")
		return
	}

	// Read the ORIGINAL bytes VERBATIM first — the fail-open anchor (spec §5.1).
	origBody, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "essaim: could not read request body: "+err.Error())
		return
	}
	_ = r.Body.Close()

	// Default: forward the original bytes verbatim. Injection only ever REPLACES
	// this with a spliced body on the success path; every error path leaves it
	// byte-identical to the client's request (cache-stable fail-open).
	body := origBody
	injected := false
	var snap inject.Snapshot
	// One-channel-per-tool arbiter (§5.3): a tool wired to the NativeFileEmitter
	// gets its rules via its always-on CLAUDE.md block, so the proxy must NOT also
	// inject for it (no double-injection). Such a request is forwarded VERBATIM,
	// with NO capture (the native-file channel owns that tool entirely).
	if s.inj != nil && s.shouldProxyInject(r) {
		built, sn, berr := s.inj.safeBuild(r.Context(), origBody)
		switch {
		case berr == nil:
			body = built                             // success: spliced body (one leading element added)
			injected = !bytes.Equal(built, origBody) // a block was actually spliced
			snap = sn                                // M3: thread the snapshot to the capture tap
		case berr == errDeadline || berr == errPanic:
			s.inj.markDegraded() // fail-open: body stays == origBody (verbatim); NO capture
		case berr == inject.ErrInjectUnsupported:
			// SKIP-on-unsupported (A-2.3): the target would 400 on an extra
			// instruction message. Forward the VERBATIM origBody (incl. any stale
			// block — no strip), NOT degraded, NO retry, NO capture. body stays == origBody.
		default:
			// Honest miss (ErrNoMatch / ErrIndexEmpty / ErrSkip): NOT degraded.
			// On ErrNoMatch the stripped-only body is safe to forward; on
			// ErrSkip/ErrIndexEmpty `built` == origBody. The snapshot still carries the
			// clean (essaim-free) messages so a no-match turn is still captured.
			body = built
			injected = !bytes.Equal(built, origBody) // a stale block may have been stripped
			snap = sn
		}
	}

	// M3: the capture context for the PRIMARY relays (NOT the 413-retry relay,
	// BR-A2-15). nil when capture is disabled or there is no clean snapshot.
	cc := s.newCaptureCtx(snap)

	// F-B: when essaim mutated the body (injected a block), a 413 / context-overflow
	// upstream response may be essaim's own doing — it could have pushed a request
	// that fit the model's window over it. Retry EXACTLY ONCE with the verbatim
	// ORIGINAL body so essaim never makes a request that would have worked fail.
	// When the body is unchanged (no injection), there is nothing to retry with.
	s.forwardChat(w, r, up, "/v1/chat/completions", body, origBody, injected, cc)
}

// completions handles POST /v1/completions. It is a plain forward/bypass: the
// same verbatim relay with NO mutation (no injection exists yet, and this is
// the <20ms autocomplete path that bypasses injection by design).
func (s *Server) completions(w http.ResponseWriter, r *http.Request) {
	up, err := s.currentProvider().Select(r.Context())
	if err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, err.Error()+" — open http://127.0.0.1:4141/setup")
		return
	}
	s.relay(w, r, up, "/v1/completions")
}

// relay forwards r.Body (streamed) to the upstream and copies the upstream
// response back VERBATIM. Used by the /v1/completions bypass, which never
// injects, so streaming the body straight through is correct and cheapest.
func (s *Server) relay(w http.ResponseWriter, r *http.Request, up upstream.Upstream, path string) {
	s.forward(w, r, up, path, r.Body)
}

// forwardChat forwards the (possibly injected) chat request body and relays the
// response verbatim, with the F-B 413/context-overflow retry-once on top: if the
// body was injected AND the upstream returns 413 (or an error-status body that
// matches a context-overflow signature), essaim re-sends the EXACT unmutated
// origBody exactly once, marks the server degraded, and relays THAT response. The
// retry budget is one (A-2.4): a second 413 (even on the clean body) is relayed
// verbatim, never a third attempt.
//
// Only ERROR-status responses are ever buffered for the overflow signature check;
// success/stream bodies are streamed verbatim and never decoded (the verbatim
// relay invariant).
func (s *Server) forwardChat(w http.ResponseWriter, r *http.Request, up upstream.Upstream, path string, body, origBody []byte, injected bool, cc *captureCtx) {
	resp, err := s.doUpstream(r, up, path, bytes.NewReader(body))
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}

	// Decide whether this is an essaim-caused overflow we must retry. Only when we
	// actually injected AND have an alternative (the smaller origBody) to try.
	if injected {
		overflow := resp.StatusCode == http.StatusRequestEntityTooLarge
		var errBody []byte
		if !overflow && resp.StatusCode >= 400 {
			// Buffer the error body (bounded, error path only) to sniff the
			// context-overflow signature. This never touches a success/stream body.
			errBody, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			overflow = bodyIsContextOverflow(errBody)
		}
		if overflow {
			_ = resp.Body.Close()
			if s.inj != nil {
				s.inj.markDegraded() // F-B / spec §5.5: an essaim-caused overflow is degraded
			}
			// Retry-once with the verbatim ORIGINAL (un-injected) body.
			retryResp, rerr := s.doUpstream(r, up, path, bytes.NewReader(origBody))
			if rerr != nil {
				writeOpenAIError(w, http.StatusBadGateway, "upstream error: "+rerr.Error())
				return
			}
			// Relay the retry response verbatim — whatever it is (incl. a second
			// 413, which the IDE then owns: it is the user's own context now). The
			// overflow-retry turn is NOT captured (BR-A2-15 / F-6): its MatchedRuleIDs
			// describe the INJECTED turn, but the retry answered UN-injected → a false
			// reinforcement signal. Pass nil (no-capture sentinel).
			s.relayResponse(w, retryResp, nil, nil)
			return
		}
		// Not an overflow: relay verbatim. If we buffered an error body above, hand
		// it to the relay so those already-read bytes are not lost. A non-2xx
		// upstream (4xx/5xx incl 429/500) is NOT a training signal — pass a nil
		// capture context so a model error never trains the learner (P2-2; mirrors
		// the overflow-retry path, which already passes nil).
		s.relayResponse(w, resp, errBody, captureIf2xx(resp.StatusCode, cc))
		return
	}

	s.relayResponse(w, resp, nil, captureIf2xx(resp.StatusCode, cc))
}

// captureIf2xx returns cc for a 2xx upstream status and nil otherwise. A non-2xx
// response (4xx/5xx incl 429/500) must never be fed to the capture/learn pipeline
// (P2-2): the error body is not an assistant answer, so learning from it would
// reinforce rules against a failed turn. Success is the ONLY status essaim learns
// from.
func captureIf2xx(status int, cc *captureCtx) *captureCtx {
	if status >= 200 && status < 300 {
		return cc
	}
	return nil
}

// forward builds and executes the upstream request from the given body reader
// and relays the response verbatim. The response bytes are never JSON-decoded on
// the client path; each read is flushed immediately so SSE streams pass through
// chunk-for-chunk. The upstream request inherits r.Context(), so a client
// disconnect cancels the upstream call.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, up upstream.Upstream, path string, body io.Reader) {
	resp, err := s.doUpstream(r, up, path, body)
	if err != nil {
		// Fail-open: relay the upstream error through rather than 500-ing essaim.
		writeOpenAIError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	// The /v1/completions bypass never injects/captures → nil capture context.
	s.relayResponse(w, resp, nil, nil)
}

// doUpstream builds and executes the upstream request from the given body reader.
// It clones the client headers, strips hop-by-hop + the loopback token, lets the
// transport recompute Content-Length (the body may have been rewritten), and
// applies the BYOK key. The returned response's Body is the caller's to close.
func (s *Server) doUpstream(r *http.Request, up upstream.Upstream, path string, body io.Reader) (*http.Response, error) {
	base := up.BaseURL
	if s.upstreamBaseOverride != "" {
		base = s.upstreamBaseOverride
	}
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, base+path, body)
	if err != nil {
		return nil, err
	}
	outReq.Header = r.Header.Clone()
	removeHopByHop(outReq.Header)        // RFC 7230 §6.1: never forward hop-by-hop headers
	outReq.Header.Del("Authorization")   // strip the loopback token before forwarding
	outReq.Header.Del("Content-Length")  // body may have been rewritten; let the transport recompute
	outReq.Header.Del("Accept-Encoding") // M3 BR-A2-3: force plaintext upstream so the capture tee reads
	//                                      `data:` lines, not gzip — without this a compressed stream feeds
	//                                      the off-path consumer gzip → empty assistant_text in the field.
	//                                      The client still gets a correct (now-plaintext) stream verbatim.
	if up.APIKey != "" {
		outReq.Header.Set("Authorization", "Bearer "+up.APIKey)
	}
	return s.httpClient.Do(outReq)
}

// relayResponse copies the upstream response to the client VERBATIM: headers
// (minus hop-by-hop) then the status then the body, each read flushed immediately
// so SSE streams pass through chunk-for-chunk. The response bytes are never
// JSON-decoded on the client path. prefix, when non-nil, is a slice of the body
// already read off resp.Body (e.g. an error body sniffed for the overflow
// signature) and is written before the remaining stream so no bytes are lost.
//
// cc, when non-nil, is the M3 capture context: AFTER each client write the same
// bytes are copied (off-path) into a reassembler/buffer to build the learning
// Capture. The capture tap is OBSERVATION-ONLY (locked invariant 2): the bytes
// are already written+flushed to the client before the tee runs, so the tee can
// never delay, backpressure, or alter the verbatim client stream. A nil cc is
// the pure M2 relay.
func (s *Server) relayResponse(w http.ResponseWriter, resp *http.Response, prefix []byte, cc *captureCtx) {
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	removeHopByHop(w.Header())
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)

	isStream := isEventStream(resp.Header)

	fl, _ := w.(http.Flusher)
	if len(prefix) > 0 {
		if _, werr := w.Write(prefix); werr != nil {
			s.finishRelay(cc, isStream, true) // client gone → partial
			return
		}
		if fl != nil {
			fl.Flush()
		}
		s.teeAfterWrite(cc, prefix, isStream)
	}
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				// Client disconnected mid-stream: the bytes that DID reach the client
				// are already teed; mark the capture partial (BR-A2-10).
				s.finishRelay(cc, isStream, true)
				return
			}
			if fl != nil {
				fl.Flush()
			}
			// Tee the JUST-WRITTEN bytes off-path (observation only).
			s.teeAfterWrite(cc, buf[:n], isStream)
		}
		if rerr != nil {
			// Upstream EOF (or read error): finish the capture. Partial iff the
			// stream never saw [DONE] (Ollama-no-[DONE], BR-A2-6) — finish() derives
			// that from the reassembler.
			s.finishRelay(cc, isStream, false)
			return
		}
	}
}

// teeAfterWrite copies the just-written client bytes into the off-path capture
// context (stream reassembler or non-stream buffer). It runs AFTER the client
// write+flush, so it is purely observational.
func (s *Server) teeAfterWrite(cc *captureCtx, b []byte, isStream bool) {
	if cc == nil {
		return
	}
	if isStream {
		cc.teeStreamBytes(b)
	} else {
		cc.teeNonStreamBytes(b)
	}
}

// finishRelay assembles + enqueues the capture (off-path). Safe with a nil cc.
func (s *Server) finishRelay(cc *captureCtx, isStream, partial bool) {
	s.finish(cc, isStream, partial)
}

// isEventStream reports whether the response is an SSE stream (Content-Type
// text/event-stream). A non-stream chat response is a single JSON body.
func isEventStream(h http.Header) bool {
	ct := h.Get("Content-Type")
	return strings.Contains(strings.ToLower(ct), "text/event-stream")
}
