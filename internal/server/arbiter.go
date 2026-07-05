package server

import "net/http"

// oikosToolHeader is the wire-time per-tool identity header (M3-R9 / §5.3). It
// is injected into a tool's config by `oikosd wire` (reusing the design-closure
// §6 per-tool-token plumbing), so oikos knows WHICH tool sent a request. The
// arbiter keys on THIS, never on up.BaseURL — SingleUpstream.Select returns the
// IDENTICAL OpenRouter base_url for every keyed request, so two tools sharing
// one key are indistinguishable by base_url; the channel split must be by tool
// identity (closes BL-4).
const oikosToolHeader = "X-Oikos-Tool"

// toolIdentity returns the wire-time tool identity for a request: the
// X-Oikos-Tool header (listener-derived identity would be the alternative on a
// per-tool loopback socket). Empty when unwired. User-Agent is deliberately NOT
// used — it is spoofable/absent; the identity is wire-time-assigned (B-4.1).
func (s *Server) toolIdentity(r *http.Request) string {
	return r.Header.Get(oikosToolHeader)
}

// fileEmitActiveFor reports whether a tool is wired as a NATIVE-FILE channel —
// in which case the proxy stays OUT of its context (one channel per tool).
func (s *Server) fileEmitActiveFor(tool string) bool {
	if tool == "" || s.fileEmitTools == nil {
		return false
	}
	return s.fileEmitTools[tool]
}

// shouldProxyInject decides whether the proxy injects for THIS request — the
// one-channel-per-tool arbiter (§5.3). A native-file-wired tool is NOT
// proxy-injected (its always-on CLAUDE.md block is the channel); every other
// tool IS. /v1/messages is deferred → the native file owns it (proxy never
// injects there). The decision keys on the wire-time TOOL IDENTITY, NOT
// up.BaseURL (BL-4).
func (s *Server) shouldProxyInject(r *http.Request) bool {
	if r.URL.Path == "/v1/messages" {
		return false // deferred → native file owns it
	}
	tool := s.toolIdentity(r)
	if s.fileEmitActiveFor(tool) {
		return false // wired as a native-file channel → proxy stays out
	}
	return true // exactly one channel per tool
}
