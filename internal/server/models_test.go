package server

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"oikos/internal/upstream"
)

func TestModelsProxyCaches60s(t *testing.T) {
	const body = `{"data":[{"id":"m"}]}`
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer up.Close()

	s := New("127.0.0.1:4141")
	s.SetProvider(&upstream.SingleUpstream{Key: "k", Detect: func(string) bool { return false }})
	s.upstreamBaseOverride = up.URL

	// First call: hits upstream.
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Body.String() != body {
		t.Fatalf("want upstream body verbatim %q, got %q", body, rec.Body.String())
	}

	// Second call within 60s: served from cache, does NOT hit upstream.
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Body.String() != body {
		t.Fatalf("cached body wrong, got %q", rec.Body.String())
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("cache must avoid a 2nd upstream hit within 60s; upstream hits=%d", got)
	}

	// Advance the injected clock past the TTL → next call refetches.
	s.now = func() time.Time { return time.Now().Add(2 * modelsTTL) }
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("expired cache must refetch; upstream hits=%d", got)
	}
}

// Fix #5 [MED]: a non-2xx upstream response must NOT be cached for the TTL.
// Upstream returns 401 then 200; the 2nd /v1/models call must RE-HIT the upstream
// (not be served a stale cached 401), so a fixed key / recovered upstream is not
// masked for 60s.
func TestModelsDoesNotCacheNon2xx(t *testing.T) {
	const okBody = `{"data":[{"id":"m"}]}`
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"bad key"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody))
	}))
	defer up.Close()

	s := New("127.0.0.1:4141")
	s.SetProvider(&upstream.SingleUpstream{Key: "k", Detect: func(string) bool { return false }})
	s.upstreamBaseOverride = up.URL

	// First call: upstream 401, relayed through but NOT cached.
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("first call should relay the upstream 401, got %d", rec.Code)
	}

	// Second call WITHIN the TTL: must re-hit the upstream (cache must be empty),
	// now getting the 200 — proving the 401 was not pinned for 60s.
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("second call must re-hit upstream and get 200 (401 must not be cached), got %d", rec.Code)
	}
	if rec.Body.String() != okBody {
		t.Fatalf("second call body wrong, got %q", rec.Body.String())
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("non-2xx must not be cached; expected 2 upstream hits, got %d", got)
	}
}

// Zero-key resolves to a 401 (same as the chat path), not a panic.
func TestModelsZeroKeyIs401(t *testing.T) {
	s := New("127.0.0.1:4141")
	s.SetProvider(&upstream.SingleUpstream{Detect: func(string) bool { return false }})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("zero-key /v1/models must be 401, got %d", rec.Code)
	}
}
