package server

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// requireToken gates /v1/* behind an "Authorization: Bearer <token>" check.
//
// Amendment 1 (Denis 2026-06-23): the gate is OPT-IN. It is engaged only when a
// token has been set via SetToken (i.e. the operator passed --require-token).
// With no token the default is single-user-host trust and /v1/* is open.
// /health is ALWAYS open regardless.
func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Gate disabled (single-user-host trust) or non-/v1 path → pass through.
		if s.token == "" || !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		// Require the "Bearer " scheme explicitly: TrimPrefix is a no-op when the
		// scheme is absent, which would let a raw `Authorization: <token>` through.
		// CutPrefix reports whether the prefix was actually present.
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeOpenAIError(w, http.StatusUnauthorized,
				"missing or invalid oikos loopback token — run `oikosd wire` to configure your tool")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeOpenAIError writes an OpenAI-shaped error envelope so OpenAI-compatible
// clients surface the message cleanly.
func writeOpenAIError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "oikos",
		},
	})
}
