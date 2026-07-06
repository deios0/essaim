package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The wedge pitch is the ONE differentiating sentence: essaim auto-writes &
// maintains your AGENTS.md from your AI corrections (a static file doesn't learn).
// It must appear verbatim, prominently, on the first-run /setup page (the front
// door a new user sees). This guards against a future copy edit quietly dropping
// the differentiator.
func TestSetupPageCarriesWedgePitch(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/setup", nil)
	req.RemoteAddr = "127.0.0.1:54321" // /setup is loopback-only
	New("127.0.0.1:4141").Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /setup: want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, WedgePitch) {
		t.Fatalf("/setup page is missing the wedge pitch %q\n--- body ---\n%s", WedgePitch, body)
	}
	// The native rules file the pitch rides — the AGENTS.md standard — must be named
	// on the page so the differentiator is concrete, not abstract.
	if !strings.Contains(body, "AGENTS.md") {
		t.Fatalf("/setup page must name AGENTS.md (the standard essaim rides)")
	}
	// The tools essaim can ACTUALLY reach must each appear in the wire surface so the
	// "every tool" claim is concrete (Cursor + Continue via base_url, Claude Code
	// via its native file). Copilot is NOT one of them — it must never be claimed.
	for _, tool := range []string{"Cursor", "Claude Code", "Continue"} {
		if !strings.Contains(body, tool) {
			t.Fatalf("/setup page must name the tool %q in the wire surface", tool)
		}
	}
	// Guard against re-introducing the false "Copilot" claim essaim cannot deliver.
	if strings.Contains(body, "Copilot") {
		t.Fatalf("/setup page must NOT claim Copilot — essaim cannot wire it")
	}
}

// The wedge pitch constant is the single source of truth both the HTML and the
// CLI banner reference, so they can never drift apart. The repositioned pitch
// rides the AGENTS.md standard (auto-write & keep current from corrections).
func TestWedgePitchWording(t *testing.T) {
	for _, want := range []string{"AGENTS.md", "correction"} {
		if !strings.Contains(WedgePitch, want) {
			t.Fatalf("WedgePitch %q must contain %q", WedgePitch, want)
		}
	}
	if strings.Contains(WedgePitch, "Copilot") {
		t.Fatalf("WedgePitch %q must NOT name Copilot — essaim cannot wire it", WedgePitch)
	}
}
