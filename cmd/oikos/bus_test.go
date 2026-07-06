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
)

// `oikos bus` on a not-joined binary reports not-joined (and does NOT hit the
// network) — default-off must be observable.
func TestRunBusNotJoined(t *testing.T) {
	t.Setenv("OIKOS_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	var out bytes.Buffer
	if err := runBus(nil, &out); err != nil {
		t.Fatalf("runBus not-joined: %v", err)
	}
	if !strings.Contains(strings.ToLower(out.String()), "not joined") {
		t.Errorf("runBus output %q should say not joined", out.String())
	}
}

// `oikos bus` on a joined binary polls the endpoint with the zone key and reports
// the connection is live (proves the stored join actually reaches the bus).
func TestRunBusJoinedPolls(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Aibus-Key")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"events":[{"id":5,"kind":"x"}]}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("OIKOS_CONFIG", filepath.Join(dir, "config.json"))
	kf := filepath.Join(dir, "z.key")
	_ = os.WriteFile(kf, []byte("zone-key\n"), 0o600)
	_ = config.Save(config.Config{Bus: &config.BusJoin{URL: srv.URL, Zone: "team", KeyFile: kf}})

	var out bytes.Buffer
	if err := runBus(nil, &out); err != nil {
		t.Fatalf("runBus joined: %v", err)
	}
	if gotKey != "zone-key" {
		t.Errorf("poll used key %q, want the key loaded from the key file", gotKey)
	}
	if !strings.Contains(strings.ToLower(out.String()), "team") {
		t.Errorf("runBus output %q should show the joined zone", out.String())
	}
}
