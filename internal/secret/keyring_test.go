package secret

import (
	"os"
	"testing"
)

// Round-trips through the real OS credential store. Guarded so headless CI
// (no keyring) skips it. Run locally with: ESSAIM_KEYRING_E2E=1 go test ./internal/secret/
func TestKeyringRoundTrip(t *testing.T) {
	if os.Getenv("ESSAIM_KEYRING_E2E") == "" {
		t.Skip("needs an OS keyring; set ESSAIM_KEYRING_E2E=1 to run")
	}
	k := Keyring{Service: "essaim-test"}
	const key, val = "rt-key", "rt-val"
	if err := k.Set(key, val); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := k.Get(key)
	if err != nil || got != val {
		t.Fatalf("Get round-trip: got %q err=%v, want %q", got, err, val)
	}
	// Absent key → "" with nil error (ErrNotFound is swallowed).
	if got, err := k.Get("definitely-absent"); err != nil || got != "" {
		t.Fatalf("absent key: got %q err=%v, want empty/nil", got, err)
	}
}

// EnvOrKeyring prefers the mapped env var over the keyring and never touches the
// OS keyring when the env var is set — so this runs headlessly.
func TestEnvOrKeyringPrefersEnv(t *testing.T) {
	e := EnvOrKeyring{
		Keyring: Keyring{Service: "essaim-test"},
		EnvVars: map[string]string{"openrouter-key": "ESSAIM_OPENROUTER_KEY"},
		getenv:  func(name string) string { return map[string]string{"ESSAIM_OPENROUTER_KEY": "env-secret"}[name] },
	}
	got, err := e.Get("openrouter-key")
	if err != nil || got != "env-secret" {
		t.Fatalf("env must win: got %q err=%v, want env-secret", got, err)
	}
}

// When the env var is empty, EnvOrKeyring.Get falls through to the keyring.
// This inherently touches the OS credential store, so it is guarded like the
// round-trip e2e (headless CI / locked Secret Service would otherwise error).
func TestEnvOrKeyringFallsThroughWhenEnvEmpty(t *testing.T) {
	if os.Getenv("ESSAIM_KEYRING_E2E") == "" {
		t.Skip("falls through to the OS keyring; set ESSAIM_KEYRING_E2E=1 to run")
	}
	e := EnvOrKeyring{
		Keyring: Keyring{Service: "essaim-test-nonexistent"},
		EnvVars: map[string]string{"openrouter-key": "ESSAIM_OPENROUTER_KEY"},
		getenv:  func(string) string { return "" },
	}
	got, err := e.Get("openrouter-key")
	if err != nil {
		t.Fatalf("fallthrough must not error on a missing keyring entry, got %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty fallthrough value, got %q", got)
	}
}
