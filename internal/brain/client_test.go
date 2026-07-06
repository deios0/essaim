package brain

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Pull fetches the zone's Brain rules via GET /api/rules?project=<p> with the
// X-Brain-Key header, returning the rule bodies. The server derives+enforces the
// zone from the key (business key -> business rules); essaim never asserts a zone.
func TestPullFetchesZoneRules(t *testing.T) {
	var gotKey, gotProject, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Brain-Key")
		gotProject = r.URL.Query().Get("project")
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"project":"eodhd","zone":"business","count":2,"rules":[
			{"id":"a","title":"Git hygiene","body":"sync via git push/pull only"},
			{"id":"b","title":"Use Postgres","body":"always postgres"}]}`)
	}))
	defer srv.Close()

	c := New(Endpoint{URL: srv.URL, Key: "brain-biz-key"})
	rules, err := c.Pull(context.Background(), "eodhd")
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if gotKey != "brain-biz-key" {
		t.Errorf("X-Brain-Key = %q, want the brain key in the header", gotKey)
	}
	if gotProject != "eodhd" {
		t.Errorf("project = %q, want eodhd", gotProject)
	}
	if gotPath != "/api/rules" {
		t.Errorf("path = %q, want /api/rules", gotPath)
	}
	if len(rules) != 2 || rules[0].Body != "sync via git push/pull only" || rules[0].Title != "Git hygiene" {
		t.Fatalf("rules = %+v, want the 2 zone rules with title+body", rules)
	}
}

// A 403 (zone-guard denial) surfaces as an error — a foreign brain key denied on
// read must not look like an empty (successful) pull.
func TestPullSurfaces403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	if _, err := New(Endpoint{URL: srv.URL, Key: "foreign"}).Pull(context.Background(), "p"); err == nil {
		t.Fatal("Pull returned nil error on 403; a zone denial must surface")
	}
}
