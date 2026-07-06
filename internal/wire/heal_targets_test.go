package wire

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"essaim/internal/config"
)

// HealTargets must produce a self-heal target for a wired base_url tool whose
// IDE config file we know how to repair (Continue), pointed at the essaim proxy.
func TestHealTargetsForWiredContinue(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "continue-config.json")
	// A Continue config that currently points at essaim.
	writeFile(t, cfgPath, `{"models":[{"apiBase":"`+ProxyBaseURL+`/v1"}]}`)

	c := config.Config{
		WiredTools: []config.WiredTool{
			{Name: "continue", Channel: ChannelBaseURL},
		},
	}
	targets := HealTargets(c, map[string]string{"continue": cfgPath})
	if len(targets) != 1 {
		t.Fatalf("want 1 heal target for wired continue, got %d", len(targets))
	}
	tg := targets[0]
	if tg.Path != cfgPath {
		t.Fatalf("target path = %q, want %q", tg.Path, cfgPath)
	}
	if !strings.Contains(tg.ExpectedURL, "127.0.0.1:4141") {
		t.Fatalf("target ExpectedURL must be the essaim proxy, got %q", tg.ExpectedURL)
	}

	// Repair: an IDE-clobbered config (apiBase moved off essaim) must be healed
	// back to the proxy while PRESERVING the rest of the JSON.
	clobbered := []byte(`{"models":[{"apiBase":"https://api.openai.com/v1"}],"theme":"dark"}`)
	healed, changed, err := tg.Repair(clobbered)
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if !changed {
		t.Fatal("Repair must report a change when the proxy url was clobbered")
	}
	if !strings.Contains(string(healed), "127.0.0.1:4141") {
		t.Fatalf("Repair must re-point apiBase at the essaim proxy:\n%s", healed)
	}
	if !strings.Contains(string(healed), `"theme"`) || !strings.Contains(string(healed), `"dark"`) {
		t.Fatalf("Repair must preserve unrelated config keys:\n%s", healed)
	}

	// A healthy config (already on the proxy) ⇒ no change.
	_, changed, err = tg.Repair([]byte(`{"models":[{"apiBase":"` + ProxyBaseURL + `/v1"}]}`))
	if err != nil {
		t.Fatalf("Repair healthy: %v", err)
	}
	if changed {
		t.Fatal("Repair must NOT change a config that already points at the proxy")
	}
}

// P1-BUG-3: the heal watcher must not SILENTLY fight a user who reverts their IDE
// to the vendor default — every successful heal must call the onHeal sink (mirror
// of onError) so `serve` can tell the user "essaim re-pointed <tool> at the proxy;
// to stop, run `essaim unwire <tool>`". The sink must fire with the tool name and
// the config path, and ONLY when a heal actually happened.
func TestHealTargetsCallsOnHealSinkOnSuccessfulHeal(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	writeFile(t, cfgPath, `{"apiBase":"`+ProxyBaseURL+`/v1"}`)

	type healEvent struct{ tool, path string }
	var got []healEvent
	sink := func(tool, path string) { got = append(got, healEvent{tool, path}) }

	targets := HealTargetsWithSink(
		config.Config{WiredTools: []config.WiredTool{{Name: "continue", Channel: ChannelBaseURL}}},
		map[string]string{"continue": cfgPath},
		sink,
	)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}

	// A vendor-default clobber → heal happens → sink fires exactly once.
	_, changed, err := targets[0].Repair([]byte(`{"apiBase":"https://api.openai.com/v1"}`))
	if err != nil || !changed {
		t.Fatalf("precondition: a vendor clobber must heal; changed=%v err=%v", changed, err)
	}
	if len(got) != 1 {
		t.Fatalf("onHeal sink must fire exactly once on a successful heal, fired %d times", len(got))
	}
	if got[0].tool != "continue" || got[0].path != cfgPath {
		t.Fatalf("onHeal must carry the tool + path; got %+v", got[0])
	}

	// A healthy config (no change) → sink must NOT fire.
	got = nil
	_, changed, err = targets[0].Repair([]byte(`{"apiBase":"` + ProxyBaseURL + `/v1"}`))
	if err != nil || changed {
		t.Fatalf("a healthy config must not heal; changed=%v err=%v", changed, err)
	}
	if len(got) != 0 {
		t.Fatalf("onHeal must NOT fire when nothing was healed, fired %d times", len(got))
	}

	// A deliberate user override (no change) → sink must NOT fire.
	got = nil
	_, changed, err = targets[0].Repair([]byte(`{"apiBase":"https://my-gateway.internal/v1"}`))
	if err != nil || changed {
		t.Fatalf("a user override must not heal; changed=%v err=%v", changed, err)
	}
	if len(got) != 0 {
		t.Fatalf("onHeal must NOT fire on a user override, fired %d times", len(got))
	}
}

// A nil onHeal sink is safe — HealTargets (the existing constructor) passes nil and
// heals must still work without a panic.
func TestHealTargetsNilSinkIsSafe(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	writeFile(t, cfgPath, `{"apiBase":"`+ProxyBaseURL+`/v1"}`)
	targets := HealTargets(
		config.Config{WiredTools: []config.WiredTool{{Name: "continue", Channel: ChannelBaseURL}}},
		map[string]string{"continue": cfgPath},
	)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	// Healing with a nil sink must not panic.
	if _, changed, err := targets[0].Repair([]byte(`{"apiBase":"https://api.openai.com/v1"}`)); err != nil || !changed {
		t.Fatalf("nil-sink heal must still work; changed=%v err=%v", changed, err)
	}
}

// The onHeal message content is essaim's honesty contract with the user. Build the
// exact stderr line `serve` should print and assert it names the tool, the path,
// and the escape hatch (`essaim unwire <tool>`).
func TestHealNoticeNamesToolPathAndUnwire(t *testing.T) {
	msg := HealNotice("continue", "/home/u/.continue/config.json")
	for _, must := range []string{"continue", "/home/u/.continue/config.json", "essaim unwire continue"} {
		if !strings.Contains(msg, must) {
			t.Fatalf("heal notice must mention %q; got:\n%s", must, msg)
		}
	}
}

// Native-file tools (Claude Code) are NEVER base_url-wired, so they must produce
// no heal target — there is no base_url to keep alive (healing one would brick
// Claude Code, which essaim v1 must never point via ANTHROPIC_BASE_URL).
func TestHealTargetsExcludesNativeFileTools(t *testing.T) {
	c := config.Config{
		WiredTools: []config.WiredTool{
			{Name: "claude-code", Channel: ChannelNativeFile, NativeFile: "/x/CLAUDE.md"},
		},
	}
	if got := HealTargets(c, nil); len(got) != 0 {
		t.Fatalf("native-file tools must yield no heal targets, got %d", len(got))
	}
}

// A wired base_url tool with no known/resolvable config file location is skipped
// (we only heal files we actually know how to repair).
func TestHealTargetsSkipsUnknownConfigLocation(t *testing.T) {
	c := config.Config{
		WiredTools: []config.WiredTool{
			{Name: "continue", Channel: ChannelBaseURL},
		},
	}
	// No path override and the default location doesn't exist in this sandbox.
	got := HealTargets(c, map[string]string{"continue": filepath.Join(t.TempDir(), "does-not-exist.json")})
	if len(got) != 0 {
		t.Fatalf("a missing config file must be skipped, got %d targets", len(got))
	}
}

func TestHealTargetsEmptyConfig(t *testing.T) {
	if got := HealTargets(config.Config{}, nil); len(got) != 0 {
		t.Fatalf("empty config must yield no heal targets, got %d", len(got))
	}
	// Sanity: the default-location resolver returns a non-empty path on this host
	// (so the wiring isn't silently dead) — but we don't require the file to exist.
	if p := os.Getenv("HOME"); p == "" {
		t.Skip("no HOME; default-location resolution not exercised")
	}
}

// Every base_url heal target must record LastWritten = the exact proxy URL essaim
// writes, so the healer only ever re-applies over essaim's OWN value.
func TestHealTargetsRecordLastWritten(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	writeFile(t, cfgPath, `{"apiBase":"`+ProxyBaseURL+`/v1"}`)
	targets := HealTargets(
		config.Config{WiredTools: []config.WiredTool{{Name: "continue", Channel: ChannelBaseURL}}},
		map[string]string{"continue": cfgPath},
	)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	if targets[0].LastWritten != ProxyBaseURL+"/v1" {
		t.Fatalf("LastWritten = %q, want the essaim proxy url", targets[0].LastWritten)
	}
}

// THE SHIP-BLOCKER TEST: a base_url the USER deliberately set to a non-essaim,
// non-vendor-default URL (their own gateway) must be LEFT ALONE. Repair reports
// no change and does not touch the value — essaim must never fight the user.
func TestRepairLeavesDeliberateUserOverrideAlone(t *testing.T) {
	cases := []string{
		`{"apiBase":"https://my-llm-gateway.internal/v1"}`,
		`{"models":[{"apiBase":"http://10.0.0.5:8080/v1"}],"theme":"dark"}`,
		`{"apiBase":"https://litellm.mycorp.example.com/v1"}`,
	}
	for _, in := range cases {
		out, changed, err := repairBaseURL([]byte(in))
		if err != nil {
			t.Fatalf("repairBaseURL(%s): unexpected err %v", in, err)
		}
		if changed {
			t.Fatalf("a deliberate user override must NOT be healed; input %s was changed to %s", in, out)
		}
		if string(out) != in {
			t.Fatalf("user override must be left byte-exact; got %s want %s", out, in)
		}
	}
}

// An IDE factory-reset (vendor-default host) IS essaim's clobbered value and MUST
// be healed back to the proxy.
func TestRepairHealsVendorDefaultClobber(t *testing.T) {
	for _, vendor := range []string{
		`{"apiBase":"https://api.openai.com/v1"}`,
		`{"apiBase":"https://openrouter.ai/api/v1"}`,
		`{"models":[{"apiBase":"https://api.anthropic.com"}]}`,
	} {
		out, changed, err := repairBaseURL([]byte(vendor))
		if err != nil {
			t.Fatalf("repairBaseURL(%s): %v", vendor, err)
		}
		if !changed {
			t.Fatalf("a vendor-default clobber must be healed; %s left unchanged", vendor)
		}
		if !strings.Contains(string(out), ProxyBaseURL) {
			t.Fatalf("heal must re-point at the essaim proxy:\n%s", out)
		}
	}
}

// An EMPTY (dropped/blanked) managed value is essaim's value removed and must be
// re-applied.
func TestRepairHealsEmptyValue(t *testing.T) {
	out, changed, err := repairBaseURL([]byte(`{"apiBase":""}`))
	if err != nil {
		t.Fatalf("repairBaseURL: %v", err)
	}
	if !changed {
		t.Fatal("an empty base_url must be healed (essaim's value was dropped)")
	}
	if !strings.Contains(string(out), ProxyBaseURL) {
		t.Fatalf("heal must re-point at the essaim proxy:\n%s", out)
	}
}

// A config already on the essaim proxy is healthy — no change, byte-exact.
func TestRepairLeavesHealthyConfigAlone(t *testing.T) {
	in := `{"apiBase":"` + ProxyBaseURL + `/v1","theme":"dark"}`
	out, changed, err := repairBaseURL([]byte(in))
	if err != nil {
		t.Fatalf("repairBaseURL: %v", err)
	}
	if changed {
		t.Fatalf("a healthy config must not be rewritten; got %s", out)
	}
	if string(out) != in {
		t.Fatalf("healthy config must be byte-exact; got %s", out)
	}
}

// P1: the heal must be SURGICAL — only the managed value's bytes change. Key
// order, exact whitespace/indentation, unrelated keys, and // and /* */ comments
// (JSONC) must all survive. The old unmarshal→MarshalIndent path destroyed these.
func TestRepairIsSurgicalAndPreservesFormattingAndComments(t *testing.T) {
	in := `{
  // essaim routes Continue through the local proxy
  "models": [
    {
      "title": "essaim",
      "apiBase": "https://api.openai.com/v1",   /* clobbered by an IDE update */
      "provider": "openai"
    }
  ],
  "tabAutocompleteModel": { "title": "keep-me" }
}`
	out, changed, err := repairBaseURL([]byte(in))
	if err != nil {
		t.Fatalf("repairBaseURL: %v", err)
	}
	if !changed {
		t.Fatal("a vendor-default clobber inside JSONC must be healed")
	}
	s := string(out)
	// The value was re-pointed…
	if !strings.Contains(s, ProxyBaseURL+"/v1") {
		t.Fatalf("apiBase not re-pointed at essaim:\n%s", s)
	}
	if strings.Contains(s, "api.openai.com") {
		t.Fatalf("clobbered vendor url should be gone:\n%s", s)
	}
	// …and EVERYTHING else is byte-preserved: comments, key order, indentation,
	// unrelated keys. The only delta is the apiBase value.
	for _, must := range []string{
		"// essaim routes Continue through the local proxy",
		"/* clobbered by an IDE update */",
		`"title": "essaim"`,
		`"provider": "openai"`,
		`"tabAutocompleteModel": { "title": "keep-me" }`,
	} {
		if !strings.Contains(s, must) {
			t.Fatalf("surgical heal dropped %q:\n%s", must, s)
		}
	}
	// Prove minimality: replacing the healed value back yields the ORIGINAL bytes.
	restored := strings.Replace(s, ProxyBaseURL+"/v1", "https://api.openai.com/v1", 1)
	if restored != in {
		t.Fatalf("heal changed more than the value's bytes.\n--- got back ---\n%s\n--- want ---\n%s", restored, in)
	}
}

// JSONC with comments was "silently un-healable" before (encoding/json refused to
// parse it, so the old path no-op'd). It must now heal.
func TestRepairHealsJSONCWithComments(t *testing.T) {
	in := "{\n  // my Continue config\n  \"apiBase\": \"https://api.openai.com/v1\"\n}"
	_, changed, err := repairBaseURL([]byte(in))
	if err != nil {
		t.Fatalf("JSONC must be parseable/healable, got err: %v", err)
	}
	if !changed {
		t.Fatal("JSONC vendor-default clobber must heal, not silently no-op")
	}
}

// A "//"-containing URL inside a string must NOT be mistaken for a comment by the
// JSONC validator (string-aware stripping).
func TestRepairURLWithSlashesIsNotTreatedAsComment(t *testing.T) {
	in := `{"apiBase":"https://my-gateway.example.com/v1","note":"see https://docs.example.com//x"}`
	out, changed, err := repairBaseURL([]byte(in))
	if err != nil {
		t.Fatalf("a URL with // must not break JSONC validation: %v", err)
	}
	if changed {
		t.Fatalf("a user-gateway URL is an override, must be left alone; got %s", out)
	}
}

// P1: a genuinely BROKEN config must SURFACE an error (so the watcher reports it
// at startup), not be silently skipped.
func TestRepairSurfacesParseErrorOnBrokenConfig(t *testing.T) {
	_, _, err := repairBaseURL([]byte(`{"apiBase": "https://api.openai.com/v1"`)) // missing closing brace
	if err == nil {
		t.Fatal("a malformed config must surface an error, not silently no-op")
	}
	if !strings.Contains(err.Error(), "JSON") {
		t.Fatalf("error should name the parse problem; got %v", err)
	}
}

// An empty file is valid-but-empty: no managed key, nothing to heal, no error.
func TestRepairEmptyFileIsNoChangeNoError(t *testing.T) {
	out, changed, err := repairBaseURL([]byte(""))
	if err != nil {
		t.Fatalf("empty file must not error: %v", err)
	}
	if changed {
		t.Fatalf("empty file has nothing to heal; got %s", out)
	}
}

// Vendor matching is HOST-EXACT, not substring: a deliberate look-alike URL the
// user set (a vendor host embedded in a path, or a confusable parent domain) is
// NOT a vendor default and must be LEFT ALONE. Guards the codex-found substring
// bug (https://api.openai.com.evil, https://proxy/api.openai.com/v1).
func TestRepairDoesNotTreatLookalikeHostAsVendorDefault(t *testing.T) {
	for _, in := range []string{
		`{"apiBase":"https://api.openai.com.evil.example/v1"}`,   // parent-domain trick
		`{"apiBase":"https://proxy.internal/api.openai.com/v1"}`, // vendor host in the PATH
		`{"apiBase":"https://not-api.openai.com.attacker.test/v1"}`,
	} {
		out, changed, err := repairBaseURL([]byte(in))
		if err != nil {
			t.Fatalf("repairBaseURL(%s): %v", in, err)
		}
		if changed {
			t.Fatalf("a look-alike host is a user value, must be left alone; %s was changed to %s", in, out)
		}
	}
	// But a real subdomain of a vendor host IS a vendor endpoint → heal.
	out, changed, err := repairBaseURL([]byte(`{"apiBase":"https://eu.api.mistral.ai/v1"}`))
	if err != nil {
		t.Fatalf("repairBaseURL: %v", err)
	}
	if !changed || !strings.Contains(string(out), ProxyBaseURL) {
		t.Fatalf("a real vendor subdomain must heal; got changed=%v out=%s", changed, out)
	}
}

// A vendor host whose URL escapes its slashes (valid JSON: `https:\/\/host\/v1`)
// must still be classified by its DECODED content and healed (codex round-3).
func TestRepairHealsEscapedSlashVendorURL(t *testing.T) {
	in := `{"apiBase":"https:\/\/api.openai.com\/v1"}`
	out, changed, err := repairBaseURL([]byte(in))
	if err != nil {
		t.Fatalf("repairBaseURL: %v", err)
	}
	if !changed {
		t.Fatalf("an escaped-slash vendor url must be recognized and healed; got %s", out)
	}
	if !strings.Contains(string(out), ProxyBaseURL) {
		t.Fatalf("must heal to the proxy:\n%s", out)
	}
	// Converse: an escaped-slash USER gateway must still be left alone.
	user := `{"apiBase":"https:\/\/my-gateway.internal\/v1"}`
	out, changed, err = repairBaseURL([]byte(user))
	if err != nil {
		t.Fatalf("repairBaseURL: %v", err)
	}
	if changed {
		t.Fatalf("an escaped-slash user gateway must be left alone; got %s", out)
	}
}

// A SCHEME-LESS vendor host an IDE may write (no http://) must still be recognized
// as a vendor default and healed (codex round-2: url.Parse gives an empty host on
// a scheme-less value).
func TestRepairHealsSchemelessVendorHost(t *testing.T) {
	for _, in := range []string{
		`{"apiBase":"api.openai.com/v1"}`,
		`{"apiBase":"api.openai.com:443/v1"}`,
	} {
		out, changed, err := repairBaseURL([]byte(in))
		if err != nil {
			t.Fatalf("repairBaseURL(%s): %v", in, err)
		}
		if !changed || !strings.Contains(string(out), ProxyBaseURL) {
			t.Fatalf("a scheme-less vendor host must heal; %s → changed=%v out=%s", in, changed, out)
		}
	}
	// A scheme-less NON-vendor host is still a user value → left alone.
	in := `{"apiBase":"my-gateway.internal/v1"}`
	out, changed, err := repairBaseURL([]byte(in))
	if err != nil {
		t.Fatalf("repairBaseURL: %v", err)
	}
	if changed {
		t.Fatalf("a scheme-less user host must be left alone; got %s", out)
	}
}

// The healer heals back to the target's RECORDED value (LastWritten), proving the
// wire-record is load-bearing — not a global constant.
func TestRepairHealsBackToRecordedLastWritten(t *testing.T) {
	const recorded = "http://127.0.0.1:4141/v1"
	heal := repairBaseURLFor(recorded)
	out, changed, err := heal([]byte(`{"apiBase":"https://api.openai.com/v1"}`))
	if err != nil || !changed {
		t.Fatalf("must heal a vendor clobber; changed=%v err=%v", changed, err)
	}
	if !strings.Contains(string(out), recorded) {
		t.Fatalf("must heal back to the recorded value %q; got %s", recorded, out)
	}
	// And a config already holding exactly the recorded value is healthy.
	_, changed, err = heal([]byte(`{"apiBase":"` + recorded + `"}`))
	if err != nil || changed {
		t.Fatalf("a config already on the recorded value is healthy; changed=%v err=%v", changed, err)
	}
}

// A managed key written INSIDE a comment is not a real config key and must NEVER
// be rewritten (codex-found: the regex ran over raw bytes including comments).
func TestRepairIgnoresManagedKeyInsideComments(t *testing.T) {
	cases := []string{
		"{\n  // \"apiBase\": \"https://api.openai.com/v1\"  (an example in a comment)\n  \"apiBase\": \"" + ProxyBaseURL + "/v1\"\n}",
		"{\n  /* old: \"apiBase\":\"https://api.openai.com/v1\" */\n  \"apiBase\": \"" + ProxyBaseURL + "/v1\"\n}",
	}
	for _, in := range cases {
		out, changed, err := repairBaseURL([]byte(in))
		if err != nil {
			t.Fatalf("repairBaseURL(%s): %v", in, err)
		}
		if changed {
			t.Fatalf("a commented-out apiBase must not be healed (real key already healthy); got %s", out)
		}
		if string(out) != in {
			t.Fatalf("must be byte-exact when only a comment matched; got %s", out)
		}
	}
}

// An UNTERMINATED block comment is malformed JSONC and must SURFACE an error, not
// be silently stripped-to-EOF and accepted (codex-found).
func TestRepairSurfacesUnterminatedBlockComment(t *testing.T) {
	_, _, err := repairBaseURL([]byte(`{"apiBase":"x" /* never closed`))
	if err == nil {
		t.Fatal("an unterminated block comment must surface an error")
	}
}

// A block comment between tokens must act as a SEPARATOR, so `[1/*x*/2]` is
// rejected as invalid (it is NOT the number 12) — codex-found token-merge bug.
func TestRepairBlockCommentIsTokenSeparatorNotMerger(t *testing.T) {
	_, _, err := repairBaseURL([]byte(`{"a":[1/*x*/2]}`))
	if err == nil {
		t.Fatal("`[1/*x*/2]` is invalid JSON; the comment must separate tokens, not merge them")
	}
}
