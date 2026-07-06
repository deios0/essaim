package wire

import (
	"path/filepath"
	"strings"
	"testing"
)

// Deferred-6: `essaim unwire` of a base_url tool must restore the user's ORIGINAL
// upstream (their real provider), not a hardcoded OpenAI default. The original is
// captured at WIRE time from the tool's IDE config, stored on the wired-tool
// record, and restored verbatim on unwire.

// captureOriginalBaseURL reads the user's real upstream out of an IDE config,
// verbatim, before essaim ever heals it.
func TestCaptureOriginalBaseURLReadsUserUpstream(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	const userUpstream = "https://api.mistral.ai/v1"
	writeFile(t, cfg, `{"models":[{"apiBase":"`+userUpstream+`"}],"theme":"dark"}`)

	got := captureOriginalBaseURL(cfg)
	if got != userUpstream {
		t.Fatalf("capture must read the user's real upstream verbatim: got %q, want %q", got, userUpstream)
	}
}

// When the config already holds only essaim's OWN proxy URL (essaim healed it
// already), there is no pristine original to capture — capture returns "".
func TestCaptureOriginalBaseURLIgnoresProxyValue(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"apiBase":"`+proxyV1URL+`"}`)

	if got := captureOriginalBaseURL(cfg); got != "" {
		t.Fatalf("capture must return \"\" when only the essaim proxy is present, got %q", got)
	}
}

// A missing/absent config yields "" (best-effort; no original to capture).
func TestCaptureOriginalBaseURLMissingIsEmpty(t *testing.T) {
	if got := captureOriginalBaseURL(""); got != "" {
		t.Fatalf("empty path must yield \"\", got %q", got)
	}
	if got := captureOriginalBaseURL("/no/such/config.json"); got != "" {
		t.Fatalf("missing file must yield \"\", got %q", got)
	}
}

// The full round-trip: an IDE config pointing at the user's own gateway → essaim
// heals it to the proxy → unwire (restoreBaseURLConfigTo with the captured
// original) must restore the user's EXACT gateway, not the OpenAI default.
func TestCaptureThenRestoreRoundTripsOriginal(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	const userGateway = "https://llm.corp.internal:8443/openai/v1"
	writeFile(t, cfg, `{"apiBase":"`+userGateway+`","theme":"dark"}`)

	// 1. Capture at wire time (before healing).
	original := captureOriginalBaseURL(cfg)
	if original != userGateway {
		t.Fatalf("capture failed: got %q, want %q", original, userGateway)
	}

	// 2. Simulate the heal watcher rewriting the config to the essaim proxy URL.
	writeFile(t, cfg, `{"apiBase":"`+proxyV1URL+`","theme":"dark"}`)

	// 3. Unwire restores to the captured original.
	st, err := restoreBaseURLConfigTo(cfg, original)
	if err != nil {
		t.Fatalf("restoreBaseURLConfigTo: %v", err)
	}
	if !st.Changed {
		t.Fatal("restore must report a change (the proxy URL was present)")
	}
	// A restore to a KNOWN original needs no manual hint — essaim is confident.
	if st.NeedsManualHint {
		t.Fatal("restoring a captured original must not flag a manual hint")
	}

	got := readFile(t, cfg)
	if !strings.Contains(got, userGateway) {
		t.Fatalf("unwire must restore the user's ORIGINAL gateway, got:\n%s", got)
	}
	if strings.Contains(got, "127.0.0.1:4141") {
		t.Fatalf("the essaim proxy URL must be gone after unwire:\n%s", got)
	}
	if strings.Contains(got, "api.openai.com") {
		t.Fatalf("unwire must NOT restore to the OpenAI default when the real upstream is known:\n%s", got)
	}
	// Unrelated keys preserved (surgical).
	if !strings.Contains(got, `"theme"`) {
		t.Fatalf("unrelated keys must be preserved:\n%s", got)
	}
}

// When no original was captured (empty restoreTo — an old wire-record), restore
// falls back to the vendor default AND flags a manual hint so the CLI tells the
// user to double-check. This preserves the pre-deferred-6 behavior for old records.
func TestRestoreFallsBackToVendorDefaultWithHint(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"apiBase":"`+proxyV1URL+`"}`)

	st, err := restoreBaseURLConfigTo(cfg, "") // no captured original
	if err != nil {
		t.Fatalf("restoreBaseURLConfigTo: %v", err)
	}
	if !st.Changed {
		t.Fatal("restore must report a change")
	}
	if !st.NeedsManualHint {
		t.Fatal("a GUESSED restore (no captured original) must flag a manual hint")
	}
	got := readFile(t, cfg)
	if !strings.Contains(got, "api.openai.com") {
		t.Fatalf("with no captured original, restore must fall back to the vendor default:\n%s", got)
	}
}
