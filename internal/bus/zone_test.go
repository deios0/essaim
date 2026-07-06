package bus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Zone returns the SERVER-enforced zone by reading it off an event the key is
// allowed to see (the server filters to the caller's zone), not any client label.
func TestZoneReadsServerEnforcedZone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Aibus-Key") != "bkey" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"events":[{"id":7782,"zone":"business","kind":"x"}]}`))
	}))
	defer srv.Close()

	z, err := New(Endpoint{URL: srv.URL, Key: "bkey"}).Zone(context.Background())
	if err != nil {
		t.Fatalf("Zone: %v", err)
	}
	if z != "business" {
		t.Fatalf("Zone = %q, want the server-enforced 'business' (not any client label)", z)
	}
}

// No events yet in the zone → Zone returns "" (unknown), not an error.
func TestZoneEmptyWhenNoEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"events":[]}`))
	}))
	defer srv.Close()
	z, err := New(Endpoint{URL: srv.URL, Key: "k"}).Zone(context.Background())
	if err != nil || z != "" {
		t.Fatalf("Zone on empty = (%q,%v), want (\"\",nil)", z, err)
	}
}
