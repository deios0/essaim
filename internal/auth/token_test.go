package auth

import (
	"errors"
	"testing"
)

type memStore map[string]string

func (m memStore) Get(k string) (string, error) { return m[k], nil }
func (m memStore) Set(k, v string) error        { m[k] = v; return nil }

func TestLoadOrCreateTokenIsIdempotent(t *testing.T) {
	s := memStore{}
	a, err := LoadOrCreateToken(s)
	if err != nil || len(a) != 64 {
		t.Fatalf("want 64-hex token, got %q err=%v", a, err)
	}
	b, _ := LoadOrCreateToken(s)
	if a != b {
		t.Fatalf("token must be stable: %q != %q", a, b)
	}
}

// brokenKeyring simulates the headless-WSL go-keyring failure: every Get and Set
// errors (no Secret Service / "failed to unlock correct collection"). Used to
// prove the token path degrades gracefully instead of crashing (P1-6b).
type brokenKeyring struct{}

func (brokenKeyring) Get(string) (string, error) { return "", errKeyringBroken }
func (brokenKeyring) Set(string, string) error   { return errKeyringBroken }

var errKeyringBroken = errors.New("failed to unlock correct collection '/org/freedesktop/secrets'")

// P1-6b: when the OS keyring is unavailable, LoadOrCreateToken must NOT panic. It
// returns a clear sentinel error (ErrKeyringUnavailable) the caller can present
// as a one-liner + exit cleanly — never a go-keyring stack/panic.
func TestLoadOrCreateTokenKeyringUnavailableGraceful(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("P1-6b: a broken keyring must not panic; got %v", r)
		}
	}()
	tok, err := LoadOrCreateToken(brokenKeyring{})
	if err == nil {
		t.Fatalf("P1-6b: a broken keyring with no env fallback must return an error")
	}
	if tok != "" {
		t.Fatalf("P1-6b: no token must be returned on a keyring failure, got %q", tok)
	}
	if err != ErrKeyringUnavailable {
		t.Fatalf("P1-6b: want ErrKeyringUnavailable (clear one-liner), got %v", err)
	}
}

// P1-6b: with an env-var fallback present, the token is read from the env even
// when the underlying keyring is broken — the secure default works headless.
func TestLoadOrCreateTokenEnvFallbackOverridesBrokenKeyring(t *testing.T) {
	store := envFirst{
		env:      map[string]string{tokenKey: "deadbeefcafe0000deadbeefcafe0000deadbeefcafe0000deadbeefcafe0000a"},
		fallback: brokenKeyring{},
	}
	tok, err := LoadOrCreateToken(store)
	if err != nil {
		t.Fatalf("P1-6b: env fallback must succeed despite a broken keyring; got %v", err)
	}
	if tok != "deadbeefcafe0000deadbeefcafe0000deadbeefcafe0000deadbeefcafe0000a" {
		t.Fatalf("P1-6b: token must come from the env, got %q", tok)
	}
}

// envFirst is a SecretStore that returns an env value when present, else
// delegates to a (broken) fallback — modeling secret.EnvOrKeyring at the auth
// boundary without importing it.
type envFirst struct {
	env      map[string]string
	fallback SecretStore
}

func (e envFirst) Get(k string) (string, error) {
	if v, ok := e.env[k]; ok && v != "" {
		return v, nil
	}
	return e.fallback.Get(k)
}
func (e envFirst) Set(k, v string) error { return e.fallback.Set(k, v) }
