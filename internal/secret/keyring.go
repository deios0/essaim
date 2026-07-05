// Package secret implements SecretStore backends: the OS credential store via
// go-keyring, and an env-var-first fallback for headless WSL/servers.
package secret

import (
	"os"

	"github.com/zalando/go-keyring"
)

// Store is the minimal credential-store interface oikos uses to read/write
// secrets (the BYOK key, the loopback token). It is satisfied by Keyring and
// EnvOrKeyring (and by in-test fakes).
type Store interface {
	Get(key string) (string, error)
	Set(key, val string) error
}

// Keyring is a SecretStore backed by the OS credential store (Keychain on
// macOS, Secret Service on Linux, Credential Manager on Windows).
type Keyring struct{ Service string }

// Get returns the stored value, or "" (nil error) when the key is absent.
func (k Keyring) Get(key string) (string, error) {
	v, err := keyring.Get(k.Service, key)
	if err == keyring.ErrNotFound {
		return "", nil
	}
	return v, err
}

// Set stores val under key in the OS credential store.
func (k Keyring) Set(key, val string) error {
	return keyring.Set(k.Service, key, val)
}

// Delete removes key from the OS credential store. A missing key is not an error
// (idempotent), so a rollback of an un-stored key is a clean no-op.
func (k Keyring) Delete(key string) error {
	err := keyring.Delete(k.Service, key)
	if err == keyring.ErrNotFound {
		return nil
	}
	return err
}

// EnvOrKeyring reads from environment variables first (for headless WSL/servers
// where no OS keyring is available), then falls back to the OS Keyring. It does
// NOT write env vars; Set always targets the Keyring.
type EnvOrKeyring struct {
	Keyring Keyring
	// EnvVars maps a logical key (e.g. "openrouter-key") to the env var name
	// (e.g. "OIKOS_OPENROUTER_KEY") that overrides it.
	EnvVars map[string]string
	// getenv is injectable for tests; defaults to os.Getenv.
	getenv func(string) string
}

// Get returns the env-var value if the mapped env var is set and non-empty,
// otherwise the keyring value.
func (e EnvOrKeyring) Get(key string) (string, error) {
	g := e.getenv
	if g == nil {
		g = os.Getenv
	}
	if env, ok := e.EnvVars[key]; ok {
		if v := g(env); v != "" {
			return v, nil
		}
	}
	return e.Keyring.Get(key)
}

// Set writes through to the OS keyring.
func (e EnvOrKeyring) Set(key, val string) error {
	return e.Keyring.Set(key, val)
}

// Delete removes key from the OS keyring (it never unsets an env var — env vars
// are read-only here). Used to roll back a just-written key on a failed setup.
func (e EnvOrKeyring) Delete(key string) error {
	return e.Keyring.Delete(key)
}
