package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"essaim/internal/config"
)

// join must store the SERVER-enforced zone (read from an event), NOT the user's
// --zone label. A dev who mistypes --zone personal with a BUSINESS key must end
// up recorded (and shown) as business — the label can never misrepresent the
// real, key-enforced zone.
func TestRunJoinStoresServerZoneNotLabel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"events":[{"id":7782,"zone":"business","kind":"x"}]}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("ESSAIM_CONFIG", filepath.Join(dir, "config.json"))
	kf := filepath.Join(dir, "z.key")
	_ = os.WriteFile(kf, []byte("bkey\n"), 0o600)

	var out bytes.Buffer
	if err := runJoin([]string{"--endpoint", srv.URL, "--zone", "personal", "--key-file", kf}, &out); err != nil {
		t.Fatalf("runJoin: %v", err)
	}
	c, _ := config.Load()
	if c.Bus == nil || c.Bus.Zone != "business" {
		t.Fatalf("stored zone = %q, want the server-enforced 'business' (not the --zone personal label)", c.Bus.Zone)
	}
	if strings.Contains(out.String(), "personal") {
		t.Errorf("join output must not present the false 'personal' label: %q", out.String())
	}
}
