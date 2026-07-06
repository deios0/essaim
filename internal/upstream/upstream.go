// Package upstream resolves the BYOK upstream target for the proxy: an
// OpenRouter endpoint (when a key is set) or an auto-detected local LLM
// (Ollama / LM Studio). This is the UpstreamProvider seam — v2 swaps in a
// Bridge/BDRR router provider additively.
package upstream

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

// ErrNoBackend is returned when there is neither a key nor a reachable local LLM.
var ErrNoBackend = errors.New("essaim: no upstream — add an OpenRouter key or start Ollama/LM Studio")

// defaultDetectTTL is how long a successful local-LLM detection is reused
// before re-dialing. Keeps the TCP probe off the per-request hot path (the
// <20ms autocomplete/bypass budget) while still noticing a backend that goes
// away within a few seconds.
const defaultDetectTTL = 3 * time.Second

// Upstream is a resolved forwarding target.
type Upstream struct {
	BaseURL string
	APIKey  string
	Kind    string
}

// Provider resolves an Upstream per request.
type Provider interface {
	Select(ctx context.Context) (Upstream, error)
}

// SingleUpstream is the v1 provider: key wins, else first reachable local LLM,
// else ErrNoBackend. Successful local detection is memoized for DetectTTL so a
// TCP dial is not paid on every request (R1: the keyless path otherwise dials
// 150ms — up to 300ms — per call, including the <20ms /v1/completions bypass).
type SingleUpstream struct {
	Key string
	// Detect reports whether a local LLM is reachable at addr. When nil, a real
	// TCP dial is used; in tests it is injected.
	Detect func(addr string) bool
	// DetectTTL overrides the detection-cache TTL; 0 → defaultDetectTTL.
	DetectTTL time.Duration

	mu     sync.Mutex
	cached *Upstream        // last successful local detection (nil until first hit)
	at     time.Time        // when cached was resolved
	now    func() time.Time // injectable clock for tests; nil → time.Now
}

func dial(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 150*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// Select returns the OpenRouter upstream when Key is set (no dial), otherwise a
// reachable local LLM (Ollama on :11434, then LM Studio on :1234) — memoized for
// DetectTTL — otherwise ErrNoBackend. A negative result is never cached, so a
// local LLM started after a cold start is picked up on the next request.
func (s *SingleUpstream) Select(_ context.Context) (Upstream, error) {
	// Keyed path needs no detection — return immediately, never dial.
	if s.Key != "" {
		return Upstream{BaseURL: "https://openrouter.ai/api", APIKey: s.Key, Kind: "openrouter"}, nil
	}

	now := s.now
	if now == nil {
		now = time.Now
	}
	ttl := s.DetectTTL
	if ttl == 0 {
		ttl = defaultDetectTTL
	}

	// Warm cache → no dial.
	s.mu.Lock()
	if s.cached != nil && now().Sub(s.at) < ttl {
		u := *s.cached
		s.mu.Unlock()
		return u, nil
	}
	s.mu.Unlock()

	// Detect OFF the lock: a dial can take up to ~300ms and we must not serialize
	// all requests behind it. A rare duplicate probe on a cold/expired cache is
	// acceptable; holding the mutex across the dial would not be.
	d := s.Detect
	if d == nil {
		d = dial
	}
	var resolved Upstream
	switch {
	case d("127.0.0.1:11434"):
		resolved = Upstream{BaseURL: "http://127.0.0.1:11434", Kind: "ollama"}
	case d("127.0.0.1:1234"):
		resolved = Upstream{BaseURL: "http://127.0.0.1:1234", Kind: "lmstudio"}
	default:
		return Upstream{}, ErrNoBackend // negative result NOT cached
	}

	s.mu.Lock()
	s.cached = &resolved
	s.at = now()
	s.mu.Unlock()
	return resolved, nil
}
