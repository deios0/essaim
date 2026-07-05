package lifecycle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"oikos/internal/rules"
)

// countsSidecar is the per-vault file the reinforce counters are persisted to.
// It lives under _inbox/ alongside the drafts it gates (the drafts dir is the
// ephemeral-hot working area). It is a LOCAL, per-machine file — it is NOT
// frontmatter (the 3-class discipline / the frontmatter-immutability rule forbids hot
// counters in frontmatter), and it should be gitignored like the drafts inbox.
//
// P0-2 FIX: the reinforce count is the ONE hot counter that gates a class-cross
// (draft→live on reinforce-twice). The promotion threshold ("≥2 INDEPENDENT
// reinforces", spec §3.3 BR-A1.5-2) is meaningless if the count is lost on every
// daemon restart — the canonical "I keep correcting the same thing across days"
// case (a restart in between) would forever see count=1 and never promote. So
// the count earns a DURABLE-but-LOCAL home: a sidecar JSON loaded on startup and
// rewritten (atomically) on every bump. Frontmatter stays untouched.
const (
	inboxDir   = rules.InboxDir // single source of truth (shared with the extractor)
	countsFile = ".counts.json"
)

// hotStore is the LOCAL ephemeral-hot store of reinforce counters keyed by
// title-hash (the 3-class discipline: hot counters NEVER touch frontmatter — the
// frontmatter-immutability rule). A reinforce increments the counter and records the time;
// the sweep reads the count to decide a promote. There is NO weight cap (Brain
// has none, lessons.py:110 is a bare `weight += 1`); if a cap were added it would
// be an explicit oikos policy.
//
// The counts are mirrored to a per-vault sidecar (_inbox/.counts.json) so a
// reinforce survives a daemon restart (P0-2). When path is "" (no vault / a
// hermetic test) persistence is a no-op and the store is purely in-mem.
type hotStore struct {
	mu     sync.Mutex
	counts map[string]*hotEntry
	path   string // sidecar file path ("" ⇒ in-mem only)
}

type hotEntry struct {
	count         int       // reinforce count (starts at 1 on first record)
	lastReinforce time.Time // for last_reinforced_at refresh
	validated     bool      // sticky validated-quality hint (upgrade, never downgrade)
	// latestAtLeastNew records whether the MOST RECENT reinforce carried a quality
	// hint >= new (RISK-4 / BR-A1.5-2: promote requires the LATEST hint be >= new,
	// not just the count). It is overwritten on each reinforce (latest-wins), unlike
	// `validated` which is sticky-up. The draft-creating write is hint >= new by
	// construction (it cleared the T1 gate), so a fresh entry starts true.
	latestAtLeastNew bool
}

// persistedEntry is the on-disk shape of a hotEntry in the counts sidecar.
type persistedEntry struct {
	Count            int       `json:"count"`
	LastReinforce    time.Time `json:"last_reinforce"`
	Validated        bool      `json:"validated"`
	LatestAtLeastNew bool      `json:"latest_at_least_new"`
}

// newHotStoreAt constructs a hot store backed by a sidecar under vault/_inbox/.
// It loads any previously-persisted counts so reinforce counts survive a daemon
// restart (P0-2). A missing/unreadable/corrupt sidecar yields an empty store
// (best-effort; a bad sidecar must never crash the lifecycle).
func newHotStoreAt(vault string) *hotStore {
	h := &hotStore{counts: make(map[string]*hotEntry)}
	if vault == "" {
		return h
	}
	h.path = filepath.Join(vault, inboxDir, countsFile)
	h.load()
	return h
}

// load reads the sidecar into the in-mem map (best-effort).
func (h *hotStore) load() {
	if h.path == "" {
		return
	}
	raw, err := os.ReadFile(h.path)
	if err != nil {
		return // missing/unreadable ⇒ start empty
	}
	var m map[string]persistedEntry
	if err := json.Unmarshal(raw, &m); err != nil {
		return // corrupt ⇒ start empty (never crash on a bad sidecar)
	}
	for hash, pe := range m {
		h.counts[hash] = &hotEntry{
			count:            pe.Count,
			lastReinforce:    pe.LastReinforce,
			validated:        pe.Validated,
			latestAtLeastNew: pe.LatestAtLeastNew,
		}
	}
}

// persist atomically rewrites the sidecar from the current in-mem map. Caller
// holds h.mu. Best-effort: a persist failure is non-fatal (the in-mem count is
// still correct for this session; only the cross-restart durability is lost).
func (h *hotStore) persist() {
	if h.path == "" {
		return
	}
	m := make(map[string]persistedEntry, len(h.counts))
	for hash, e := range h.counts {
		m[hash] = persistedEntry{Count: e.count, LastReinforce: e.lastReinforce, Validated: e.validated, LatestAtLeastNew: e.latestAtLeastNew}
	}
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	// Ensure the _inbox/ dir exists (drafts may not have been written yet) AND that
	// it carries a .gitignore so a git-tracked vault never commits this sidecar
	// (review fix). h.path == vault/_inbox/.counts.json, so the vault root is the
	// grandparent. Best-effort: a dir-create failure falls through to atomicWrite,
	// which will surface its own error (swallowed here, persist is best-effort).
	_, _ = rules.EnsureInboxDir(filepath.Dir(filepath.Dir(h.path)))
	_ = atomicWrite(h.path, data)
}

// reinforce bumps the local reinforce counter for a title-hash and returns the
// new count. validatedHint upgrades the sticky validated flag (never
// downgrades); atLeastNew records whether THIS reinforce's quality hint was
// >= new (latest-wins — the gate the promote reads, RISK-4). NO cap (Brain
// parity). The new state is persisted to the sidecar so it survives a restart
// (P0-2).
func (h *hotStore) reinforce(hash string, validatedHint, atLeastNew bool, now time.Time) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.counts[hash]
	if e == nil {
		// A brand-new entry is the draft-creating write: it cleared the T1 gate,
		// so its hint is >= new by construction.
		e = &hotEntry{count: 1, lastReinforce: now, latestAtLeastNew: true}
		h.counts[hash] = e
	} else {
		e.count++
		e.lastReinforce = now
		// A real reinforce: the latest hint replaces the cached flag (latest-wins).
		// A validated hint is also >= new; a rejected/unknown hint clears it.
		e.latestAtLeastNew = atLeastNew || validatedHint
	}
	if validatedHint {
		e.validated = true // sticky-up
	}
	h.persist()
	return e.count
}

func (h *hotStore) count(hash string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.counts[hash]; e != nil {
		return e.count
	}
	return 0
}

// lastReinforce returns the time of the most recent reinforce for a title-hash
// (P2-reinforce-ts: the decay clock re-anchors at this instant on a sweep). A
// zero time means no in-session reinforce was recorded for this rule (the sweep
// then falls back to the frontmatter last_reinforced_at).
func (h *hotStore) lastReinforce(hash string) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.counts[hash]; e != nil {
		return e.lastReinforce
	}
	return time.Time{}
}

// latestHintAtLeastNew reports whether the most recent reinforce for a title-hash
// carried a quality hint >= new (RISK-4 promote gate). An absent entry is false.
func (h *hotStore) latestHintAtLeastNew(hash string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.counts[hash]; e != nil {
		return e.latestAtLeastNew
	}
	return false
}

// syncFile flushes a file's data to stable storage. It is a package var so a
// durability test can observe that the temp is fsynced BEFORE the rename
// (the crash-durability contract ede42a4 established for the counts sidecar —
// without this an interrupted write could leave a renamed zero-length
// .counts.json, silently resetting every reinforce count). Production always
// uses (*os.File).Sync.
var syncFile = func(f *os.File) error { return f.Sync() }

// atomicWrite writes data to path via write-temp-then-rename, crash-durably:
// the temp is fsynced (syncFile) BEFORE the rename so a crash can't leave a
// renamed zero-length file, and the parent dir is fsynced AFTER so the rename
// itself survives a crash. This is the SAME durability discipline as the vault
// atomicWrite copies (ede42a4) — it must stay consistent for the reinforce-count
// sidecar (.counts.json), which gates the draft→live promotion (P0-2).
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".oikos-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	// Flush data before the rename so a crash can't leave a renamed zero-length file.
	if err := syncFile(tmp); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	// Best-effort directory fsync so the rename itself survives a crash.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
