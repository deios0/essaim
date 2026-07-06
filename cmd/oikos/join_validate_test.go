package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"oikos/internal/config"
)

// `oikos join` live-validates the key against the endpoint BEFORE persisting:
// a rejected key (401/403) must NOT be saved (never persist an unconfirmed
// credential — mirrors the model-key validation).
func TestRunJoinRejectsBadKeyNoPersist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("OIKOS_CONFIG", filepath.Join(dir, "config.json"))
	kf := filepath.Join(dir, "z.key")
	_ = os.WriteFile(kf, []byte("bad-key\n"), 0o600)

	var out bytes.Buffer
	err := runJoin([]string{"--endpoint", srv.URL, "--zone", "team", "--key-file", kf}, &out)
	if err == nil {
		t.Fatal("runJoin persisted despite a 403; a rejected key must error and not join")
	}
	c, _ := config.Load()
	if c.Bus != nil {
		t.Fatalf("a rejected join was persisted: %+v", c.Bus)
	}
}

// A valid key (2xx) validates and persists.
func TestRunJoinValidKeyPersists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Aibus-Key") != "good-key" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"events":[]}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("OIKOS_CONFIG", filepath.Join(dir, "config.json"))
	kf := filepath.Join(dir, "z.key")
	_ = os.WriteFile(kf, []byte("good-key\n"), 0o600)

	var out bytes.Buffer
	if err := runJoin([]string{"--endpoint", srv.URL, "--zone", "team", "--key-file", kf}, &out); err != nil {
		t.Fatalf("runJoin with a valid key: %v", err)
	}
	c, _ := config.Load()
	if c.Bus == nil || c.Bus.KeyFile != kf {
		t.Fatalf("valid join not persisted: %+v", c.Bus)
	}
}

// --no-verify skips the live check (offline / air-gapped join); persists as-is.
func TestRunJoinNoVerifySkipsValidation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OIKOS_CONFIG", filepath.Join(dir, "config.json"))
	var out bytes.Buffer
	err := runJoin([]string{"--endpoint", "http://unreachable.invalid/aibus/events", "--no-verify"}, &out)
	if err != nil {
		t.Fatalf("runJoin --no-verify should not reach the network: %v", err)
	}
	c, _ := config.Load()
	if c.Bus == nil {
		t.Fatal("--no-verify join not persisted")
	}
}
