package bus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Verify does a live GET against the endpoint with the zone key. 2xx = the key
// is accepted for this zone; a 401/403 surfaces as an error so `essaim join` can
// refuse to persist an unconfirmed/rejected key (mirrors the model-key live
// validation — never persist on an unconfirmed credential).
func TestVerifyAcceptsValidKey(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Aibus-Key")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"events":[]}`))
	}))
	defer srv.Close()

	c := New(Endpoint{URL: srv.URL, Key: "good"})
	if err := c.Verify(context.Background()); err != nil {
		t.Fatalf("Verify on a 200: %v", err)
	}
	if gotKey != "good" {
		t.Errorf("X-Aibus-Key = %q, want the key sent for validation", gotKey)
	}
}

func TestVerifyRejectsBadKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := New(Endpoint{URL: srv.URL, Key: "bad"})
	if err := c.Verify(context.Background()); err == nil {
		t.Fatal("Verify returned nil on a 401; a rejected key must surface as an error")
	}
}

// LoadKey reads the raw key from a key file (trimming whitespace) — config stores
// only the file path, never the raw key, so join must load it at use time.
func TestLoadKeyReadsFileTrimmed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "z.key")
	if err := os.WriteFile(p, []byte("  secret-zone-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadKey(p)
	if err != nil {
		t.Fatalf("LoadKey: %v", err)
	}
	if got != "secret-zone-key" {
		t.Errorf("LoadKey = %q, want the trimmed key", got)
	}
}

func TestLoadKeyMissingFileErrors(t *testing.T) {
	if _, err := LoadKey(filepath.Join(t.TempDir(), "nope.key")); err == nil {
		t.Fatal("LoadKey on a missing file returned nil error")
	}
}
