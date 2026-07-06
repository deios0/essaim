package bus

import "testing"

// env AIBUS_URL/AIBUS_KEY must WIN over the stored join config — this is the
// ADR gotcha: an off-tailnet override (or a wrong-zone fix) is applied by env,
// never defeated by a hardcoded/stored endpoint. (bridge.zone_config force-source
// translated to standalone Go: env is the force-source.)
func TestResolveEnvWinsOverStored(t *testing.T) {
	env := map[string]string{
		"AIBUS_URL": "https://bus.example.com/aibus/events",
		"AIBUS_KEY": "env-key",
	}
	getenv := func(k string) string { return env[k] }
	stored := Endpoint{URL: "http://stored:8719", Key: "stored-key", Zone: "personal"}

	got, ok := Resolve(getenv, stored)
	if !ok {
		t.Fatal("Resolve returned not-joined, want joined (env supplies an endpoint)")
	}
	if got.URL != "https://bus.example.com/aibus/events" {
		t.Errorf("URL = %q, want the env AIBUS_URL to win", got.URL)
	}
	if got.Key != "env-key" {
		t.Errorf("Key = %q, want the env AIBUS_KEY to win", got.Key)
	}
}

// With no env override, the stored join config is used (the normal joined case).
func TestResolveFallsBackToStored(t *testing.T) {
	getenv := func(string) string { return "" }
	stored := Endpoint{URL: "http://stored:8719", Key: "stored-key", Zone: "team"}

	got, ok := Resolve(getenv, stored)
	if !ok || got.URL != "http://stored:8719" || got.Key != "stored-key" {
		t.Fatalf("Resolve = %+v, ok=%v; want the stored endpoint", got, ok)
	}
}

// Default-off: neither env nor stored → not joined, no bus.
func TestResolveNotJoinedWhenNeither(t *testing.T) {
	getenv := func(string) string { return "" }
	if _, ok := Resolve(getenv, Endpoint{}); ok {
		t.Fatal("Resolve reported joined with no env and empty stored config; want not-joined (default-off)")
	}
}
