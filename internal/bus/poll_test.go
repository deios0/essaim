package bus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Poll GETs events after a cursor and returns them + the new max id, so essaim can
// receive rules/coordination from its zone. The key goes in the header (server
// filters to the zone); the caller advances the cursor.
func TestPollReturnsEventsAndAdvancesCursor(t *testing.T) {
	var gotSince, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSince = r.URL.Query().Get("since")
		gotKey = r.Header.Get("X-Aibus-Key")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"events":[{"id":10,"kind":"essaim.rule.shared"},{"id":12,"kind":"x"}]}`))
	}))
	defer srv.Close()

	c := New(Endpoint{URL: srv.URL, Key: "zkey"})
	evs, maxID, err := c.Poll(context.Background(), 7)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if gotSince != "7" {
		t.Errorf("since = %q, want the passed cursor 7", gotSince)
	}
	if gotKey != "zkey" {
		t.Errorf("X-Aibus-Key = %q, want the zone key", gotKey)
	}
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2", len(evs))
	}
	if maxID != 12 {
		t.Errorf("maxID = %d, want 12 (the highest event id)", maxID)
	}
}

// A 403 while polling surfaces as an error — a foreign/guest key denied on read
// must not look like an empty (successful) poll.
func TestPollSurfaces403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c := New(Endpoint{URL: srv.URL, Key: "foreign"})
	if _, _, err := c.Poll(context.Background(), 0); err == nil {
		t.Fatal("Poll returned nil error on 403; a zone denial must surface, not look like empty")
	}
}
