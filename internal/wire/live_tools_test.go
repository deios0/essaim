package wire

import (
	"path/filepath"
	"testing"

	"oikos/internal/config"
)

// LiveWiredTools must reflect the CURRENT config.json: a tool present in the
// wired set is reported live; after it is removed (the effect of `oikos unwire`)
// the predicate no longer reports it — the running watcher's RESPECTS-UNWIRE
// signal (P1). This is the real config-backed path the heal stub tests model.
func TestLiveWiredToolsReflectsConfigAcrossUnwire(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OIKOS_CONFIG", filepath.Join(dir, "config.json"))

	// Wire a base_url tool (cursor) so config.json records it.
	if err := config.Save(config.Config{
		WiredTools: []config.WiredTool{{Name: "cursor", Channel: ChannelBaseURL}},
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	live := LiveWiredTools()
	set, ok := live()
	if !ok {
		t.Fatal("predicate must determine the set for a readable config")
	}
	if !set["cursor"] {
		t.Fatalf("cursor must be reported live while wired; set=%v", set)
	}

	// Simulate `oikos unwire cursor`: persist a config without it.
	if err := config.Save(config.Config{WiredTools: nil}); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}

	set, ok = live()
	if !ok {
		t.Fatal("predicate must determine the set after the config changed")
	}
	if set["cursor"] {
		t.Fatalf("cursor must NOT be live after it was unwired; set=%v", set)
	}
}

// The predicate FAILS TOWARD HEALING (ok=false) when it cannot read the config —
// a missing file is undeterminable, so the watcher keeps guarding rather than
// silently dropping it. (config.Load treats a missing file as empty; the live
// predicate must instead signal "unknown" so a transient/odd state never stops
// the guard.)
func TestLiveWiredToolsUndeterminableOnMissingConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OIKOS_CONFIG", filepath.Join(dir, "does-not-exist.json"))

	if _, ok := LiveWiredTools()(); ok {
		t.Fatal("a missing config must be undeterminable (ok=false), not an empty live set")
	}
}

// The mtime cache must not go stale: a real edit to config.json (new mtime) must
// be picked up on the next call, not masked by the cache.
func TestLiveWiredToolsMtimeCacheRefreshesOnChange(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OIKOS_CONFIG", filepath.Join(dir, "config.json"))

	if err := config.Save(config.Config{
		WiredTools: []config.WiredTool{{Name: "continue", Channel: ChannelBaseURL}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	live := LiveWiredTools()
	if set, ok := live(); !ok || !set["continue"] {
		t.Fatalf("continue must be live initially; ok=%v set=%v", ok, set)
	}

	// Add a second tool. config.Save is atomic (temp+rename), giving a fresh inode
	// and (virtually always) a changed mtime/size so the cache invalidates.
	if err := config.Save(config.Config{
		WiredTools: []config.WiredTool{
			{Name: "continue", Channel: ChannelBaseURL},
			{Name: "cursor", Channel: ChannelBaseURL},
		},
	}); err != nil {
		t.Fatalf("grow: %v", err)
	}
	set, ok := live()
	if !ok {
		t.Fatal("predicate must re-determine after the config grew")
	}
	if !set["continue"] || !set["cursor"] {
		t.Fatalf("both tools must be live after the edit; set=%v", set)
	}
}

// End-to-end target identity: HealTargets must stamp each target with its Tool
// name, so the live-tools filter can address it. (Without Tool, the RESPECTS-
// UNWIRE filter cannot gate the target and unwire would not take effect.)
func TestHealTargetsStampToolName(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "continue.json")
	writeFile(t, cfgPath, `{"apiBase":"`+ProxyBaseURL+`/v1"}`)

	targets := HealTargets(
		config.Config{WiredTools: []config.WiredTool{{Name: "continue", Channel: ChannelBaseURL}}},
		map[string]string{"continue": cfgPath},
	)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	if targets[0].Tool != "continue" {
		t.Fatalf("target Tool = %q, want \"continue\"", targets[0].Tool)
	}
}
