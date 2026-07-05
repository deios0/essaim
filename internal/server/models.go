package server

import (
	"io"
	"net/http"
	"time"
)

// modelsTTL is how long a fetched /v1/models response is served from cache.
const modelsTTL = 60 * time.Second

// modelsHandler handles GET /v1/models. It returns the upstream model list
// verbatim, cached in-memory for 60s. Zero-key resolves to the same 401 as the
// chat path.
func (s *Server) modelsHandler(w http.ResponseWriter, r *http.Request) {
	up, err := s.currentProvider().Select(r.Context())
	if err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, err.Error()+" — open http://127.0.0.1:4141/setup")
		return
	}

	s.models.mu.Lock()
	fresh := s.models.body != nil && s.now().Sub(s.models.at) < modelsTTL
	if fresh {
		body, code := s.models.body, s.models.code
		s.models.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write(body)
		return
	}
	s.models.mu.Unlock()

	base := up.BaseURL
	if s.upstreamBaseOverride != "" {
		base = s.upstreamBaseOverride
	}
	outReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "oikos: could not build upstream request: "+err.Error())
		return
	}
	if up.APIKey != "" {
		outReq.Header.Set("Authorization", "Bearer "+up.APIKey)
	}
	resp, err := s.httpClient.Do(outReq)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}

	// Only cache successful (2xx) responses. An upstream error (e.g. a 401 from a
	// bad key, or a transient 5xx) must NOT be pinned in the cache for the TTL —
	// otherwise a fixed key or a recovered upstream stays masked for 60s. Non-2xx
	// is relayed through but never cached.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		s.models.mu.Lock()
		s.models.body = body
		s.models.code = resp.StatusCode
		s.models.at = s.now()
		s.models.mu.Unlock()
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}
