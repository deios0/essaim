// Package auth manages the opt-in loopback bearer token (amendment 1).
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
)

// SecretStore is the minimal credential-store interface the token machinery
// needs. It is satisfied by internal/secret.Keyring (and the in-test memStore).
type SecretStore interface {
	Get(key string) (string, error)
	Set(key, val string) error
}

const tokenKey = "loopback-token"

// ErrKeyringUnavailable is returned when the OS credential store cannot be used
// (headless Linux/WSL with no Secret Service — the go-keyring "failed to unlock
// correct collection" case) AND no env-var fallback supplied a token. The caller
// presents it as a clear one-liner and exits cleanly — it must NEVER surface as a
// go-keyring panic/stack (P1-6b). The fix path is to set OIKOS_LOOPBACK_TOKEN.
var ErrKeyringUnavailable = errors.New(
	"oikos: OS keyring unavailable for the loopback token; set OIKOS_LOOPBACK_TOKEN to a 64-hex token (e.g. `openssl rand -hex 32`) to use --require-token headlessly")

// LoadOrCreateToken returns the existing loopback token from the store, or
// generates a fresh 32-byte (64-hex-char) token, persists it, and returns it.
// It is idempotent: repeated calls against the same store return the same token.
//
// P1-6b: it degrades GRACEFULLY on a headless box. A non-empty Get result (e.g.
// from an OIKOS_LOOPBACK_TOKEN env var via secret.EnvOrKeyring) is used as-is and
// no keyring write is attempted. Only when there is NO token AND the store cannot
// persist one does it return ErrKeyringUnavailable — a clear, actionable error,
// never the raw go-keyring failure (and never a panic).
func LoadOrCreateToken(s SecretStore) (string, error) {
	// An existing token (from the keyring OR the env fallback) wins outright.
	if t, err := s.Get(tokenKey); err == nil && t != "" {
		return t, nil
	}
	// No usable token yet → mint one and try to persist it.
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	t := hex.EncodeToString(buf)
	if err := s.Set(tokenKey, t); err != nil {
		// The store can't persist (no Secret Service on this host) and nothing
		// supplied a token. Fail with a clear, actionable error — never the cryptic
		// go-keyring message or a panic.
		return "", ErrKeyringUnavailable
	}
	return t, nil
}
