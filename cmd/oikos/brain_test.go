package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"oikos/internal/config"
	"oikos/internal/rules"
)

// `oikos brain pull` fetches the zone rules and mirrors them into the vault's
// _brain/ dir so a subsequent `oikos emit` writes them into the native files.
func TestRunBrainPullMirrorsZoneRules(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Brain-Key") != "bkey" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"rules":[{"id":"g1","title":"Git","body":"sync via git only"},{"id":"p1","title":"PG","body":"use postgres"}]}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("OIKOS_CONFIG", filepath.Join(dir, "config.json"))
	vault := filepath.Join(dir, "vault")
	_ = os.MkdirAll(vault, 0o755)
	kf := filepath.Join(dir, "brain.key")
	_ = os.WriteFile(kf, []byte("bkey\n"), 0o600)
	_ = config.Save(config.Config{
		VaultDir: vault,
		Brain:    &config.BrainJoin{URL: srv.URL, Zone: "business", KeyFile: kf},
	})

	var out bytes.Buffer
	if err := runBrain([]string{"pull", "--project", "eodhd"}, &out); err != nil {
		t.Fatalf("runBrain pull: %v", err)
	}
	rs, _ := rules.LoadVault(vault)
	if n := len(rules.InjectableRules(rs)); n != 2 {
		t.Fatalf("want 2 mirrored injectable rules, got %d", n)
	}
	if !strings.Contains(out.String(), "2") {
		t.Errorf("output should report the pulled count: %q", out.String())
	}
}

// Not joined to a Brain and no flags → clear error, no network.
func TestRunBrainPullNotJoined(t *testing.T) {
	t.Setenv("OIKOS_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	var out bytes.Buffer
	if err := runBrain([]string{"pull"}, &out); err == nil {
		t.Fatal("runBrain pull with no Brain join returned nil; want an error")
	}
}
