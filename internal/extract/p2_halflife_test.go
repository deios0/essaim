package extract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"essaim/internal/rules"
)

// P2-2: an essaim-AUTHORED rule must carry a half_life_days so the decay clock can
// retire it if it is never reinforced. Before the fix writeActive/writeDraft never
// wrote half_life_days, so a loaded rule had HalfLife==0 and DecayedEffWeight
// returned the undecayed weight forever — a wrongly-promoted rule lived forever.

// The rendered ACTIVE rule (sigil path) carries half_life_days, and the rules
// loader parses it back into a positive HalfLife that actually decays.
func TestWriteActiveWritesHalfLife(t *testing.T) {
	vault := t.TempDir()
	e := New(vault, Config{})
	path, err := e.writeActive("Always use PostgreSQL, never MySQL")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "half_life_days:") {
		t.Fatalf("active rule must write half_life_days so decay can act; got:\n%s", raw)
	}

	rs, err := rules.LoadVault(filepath.Dir(filepath.Dir(path)))
	if err != nil || len(rs) != 1 {
		t.Fatalf("load: err=%v n=%d", err, len(rs))
	}
	r := rs[0]
	if r.HalfLife <= 0 {
		t.Fatalf("loaded active rule must have a positive HalfLife, got %v", r.HalfLife)
	}
	// The decay must actually bite: far past several half-lives, effective weight
	// drops well below the raw weight (proving the rule is no longer immortal).
	last := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := last.Add(time.Duration(r.HalfLife*4) * 24 * time.Hour)
	got := rules.DecayedEffWeight(r, last, now)
	if got >= r.Weight {
		t.Fatalf("after 4 half-lives the decayed weight (%.4f) must be below the raw weight (%.4f)", got, r.Weight)
	}
}

// The rendered DRAFT rule (T1 path) also carries half_life_days.
func TestWriteDraftWritesHalfLife(t *testing.T) {
	vault := t.TempDir()
	e := New(vault, Config{})
	path, err := e.writeDraft("always prefer composition over inheritance because it is the rule", Quality{Score: 0.7, Hint: "new"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "half_life_days:") {
		t.Fatalf("draft rule must write half_life_days so a wrongly-promoted rule can decay; got:\n%s", raw)
	}
}

// The default is the spec's canonical preference half-life (30 days).
func TestEssaimAuthoredHalfLifeDefault(t *testing.T) {
	if DefaultHalfLifeDays != 30 {
		t.Fatalf("essaim-authored default half-life = %v, want the spec's 30-day preference default", DefaultHalfLifeDays)
	}
}
