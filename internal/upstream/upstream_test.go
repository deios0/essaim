package upstream

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSelectPrefersKeyThenLocalThenError(t *testing.T) {
	none := func(string) bool { return false }
	if _, err := (&SingleUpstream{Detect: none}).Select(context.Background()); !errors.Is(err, ErrNoBackend) {
		t.Fatal("no key + no local must be ErrNoBackend")
	}
	u, _ := (&SingleUpstream{Key: "k", Detect: none}).Select(context.Background())
	if u.Kind != "openrouter" {
		t.Fatalf("key must win, got %s", u.Kind)
	}
	loc := func(a string) bool { return a == "127.0.0.1:11434" }
	u, _ = (&SingleUpstream{Detect: loc}).Select(context.Background())
	if u.Kind != "ollama" {
		t.Fatalf("local fallback wrong, got %s", u.Kind)
	}

	// LM Studio fallback when only :1234 is up.
	lm := func(a string) bool { return a == "127.0.0.1:1234" }
	u, _ = (&SingleUpstream{Detect: lm}).Select(context.Background())
	if u.Kind != "lmstudio" {
		t.Fatalf("lmstudio fallback wrong, got %s", u.Kind)
	}
}

// R1: a successful local detection is memoized for DetectTTL so the keyless
// hot path does not dial on every request; an expired cache re-dials.
func TestSelectCachesLocalDetection(t *testing.T) {
	calls := 0
	det := func(a string) bool { calls++; return a == "127.0.0.1:11434" }
	clock := time.Unix(1000, 0)
	s := &SingleUpstream{Detect: det, now: func() time.Time { return clock }}

	u, err := s.Select(context.Background())
	if err != nil || u.Kind != "ollama" {
		t.Fatalf("first select: want ollama, got %q err=%v", u.Kind, err)
	}
	afterFirst := calls
	if afterFirst == 0 {
		t.Fatal("first select must probe")
	}

	// Within TTL → served from cache, no new probe.
	if _, err := s.Select(context.Background()); err != nil {
		t.Fatalf("warm select err=%v", err)
	}
	if calls != afterFirst {
		t.Fatalf("warm cache must not re-dial: probes %d→%d", afterFirst, calls)
	}

	// Past TTL → re-probe.
	clock = clock.Add(defaultDetectTTL + time.Second)
	if _, err := s.Select(context.Background()); err != nil {
		t.Fatalf("post-TTL select err=%v", err)
	}
	if calls == afterFirst {
		t.Fatal("expired cache must re-dial")
	}
}

// R1 minor (a): a negative detection is NEVER cached. Each keyless miss
// re-probes, and a backend that comes up is picked up on the very next request
// (cold-start UX) — even inside one DetectTTL window. The clock is held fixed so
// the re-probe is provably from the absent negative-cache, not TTL expiry.
func TestSelectNegativeNotCached(t *testing.T) {
	calls := 0
	up := false // flips to true when the local LLM "starts"
	det := func(a string) bool { calls++; return up && a == "127.0.0.1:11434" }
	clock := time.Unix(2000, 0)
	s := &SingleUpstream{Detect: det, now: func() time.Time { return clock }}

	// First miss → ErrNoBackend, and it must have probed.
	if _, err := s.Select(context.Background()); !errors.Is(err, ErrNoBackend) {
		t.Fatalf("first keyless miss: want ErrNoBackend, got %v", err)
	}
	afterFirst := calls
	if afterFirst == 0 {
		t.Fatal("first miss must probe")
	}

	// Second miss, clock unchanged → must probe AGAIN: a negative is not cached.
	if _, err := s.Select(context.Background()); !errors.Is(err, ErrNoBackend) {
		t.Fatalf("second keyless miss: want ErrNoBackend, got %v", err)
	}
	if calls == afterFirst {
		t.Fatal("negative result must NOT be cached: second select must re-probe")
	}

	// Backend comes up within the same TTL window (clock fixed): the next
	// request must see it, proving no stale negative is masking it.
	up = true
	if u, err := s.Select(context.Background()); err != nil || u.Kind != "ollama" {
		t.Fatalf("just-started backend must be picked up next request, got %q err=%v", u.Kind, err)
	}
}

// A keyed provider must never dial (no detection on the OpenRouter path).
func TestSelectKeyedNeverDials(t *testing.T) {
	probed := false
	s := &SingleUpstream{Key: "k", Detect: func(string) bool { probed = true; return true }}
	u, err := s.Select(context.Background())
	if err != nil || u.Kind != "openrouter" {
		t.Fatalf("want openrouter, got %q err=%v", u.Kind, err)
	}
	if probed {
		t.Fatal("keyed path must not dial")
	}
}
