package bus

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Publish POSTs the event to the endpoint with the X-Aibus-Key header and the
// {project_id, kind, payload} body the aibus server expects, and returns the
// server-assigned event id. The KEY goes in the header (server derives the zone
// from it) — never in the body, never a client-asserted zone.
func TestPublishSendsKeyHeaderAndReturnsID(t *testing.T) {
	var gotKey, gotKind, gotProject string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Aibus-Key")
		var body struct {
			ProjectID string         `json:"project_id"`
			Kind      string         `json:"kind"`
			Payload   map[string]any `json:"payload"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		gotKind, gotProject = body.Kind, body.ProjectID
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id": 4242, "created_at": "now"}`)
	}))
	defer srv.Close()

	c := New(Endpoint{URL: srv.URL, Key: "secret-zone-key", Zone: "team"})
	id, err := c.Publish(context.Background(), "oikos-test", "oikos.rule.shared", map[string]any{"msg": "hi"})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if id != 4242 {
		t.Errorf("id = %d, want 4242 (server-assigned)", id)
	}
	if gotKey != "secret-zone-key" {
		t.Errorf("X-Aibus-Key = %q, want the endpoint key in the header", gotKey)
	}
	if gotKind != "oikos.rule.shared" || gotProject != "oikos-test" {
		t.Errorf("body kind=%q project=%q, want the passed kind + project", gotKind, gotProject)
	}
}

// A non-2xx from the bus (e.g. 403 zone-guard denial) is surfaced as an error,
// not silently swallowed — a guest/foreign key that gets 403 must NOT look like
// a successful publish.
func TestPublishSurfaces403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"detail":"zone denied"}`)
	}))
	defer srv.Close()

	c := New(Endpoint{URL: srv.URL, Key: "foreign"})
	if _, err := c.Publish(context.Background(), "p", "k", nil); err == nil {
		t.Fatal("Publish returned nil error on a 403; a zone denial must surface as an error")
	}
}
