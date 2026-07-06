package rules

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

// defaultDebounce is the fsnotify coalescing window: rapid bursts of writes (an
// editor saving, a git checkout touching many files) trigger ONE rebuild.
const defaultDebounce = 200 * time.Millisecond

// defaultPollInterval is the mtime-gated poll fallback period (P0-4). fsnotify
// delivers NO events on WSL2 DrvFs (/mnt/c) and many network filesystems, so an
// event-only watcher freezes the index there forever. Each tick cheaply scans
// the vault's max modtime and rebuilds ONLY when it advanced past the last
// rebuild, so the fallback is near-free on a healthy inotify filesystem (the
// fsnotify path keeps lastMod current → the scan finds nothing) and caps
// staleness on a filesystem where fsnotify is dead.
const defaultPollInterval = 15 * time.Second

// Store owns the vault directory, the atomic index Pointer, and the fsnotify
// watcher. The request path reads only Pointer.Load() — never the Store mutex or
// the filesystem (spec §5.2). Rebuilds run off the request path and atomically
// swap the published index (build-then-swap).
type Store struct {
	dir      string
	ptr      Pointer
	debounce time.Duration
	poll     time.Duration // mtime-gated poll fallback period (P0-4); 0 disables

	// degraded is set when a rebuild kept the last-good index instead of
	// publishing an empty one (the vault dir went missing/unreadable, or a load
	// returned 0 rules while the prior index was non-empty — P1-4). It is sticky
	// until a healthy non-empty load clears it. The server ORs it into /health.
	degraded atomic.Bool

	// watcherDegraded is set (sticky) the first time the poll fallback has to
	// re-index a vault change fsnotify never reported — a reliable signal that
	// inotify is not delivering on this filesystem (WSL2 /mnt/c, network FS).
	// The server surfaces it on /health so a silently-dead watcher is visible.
	watcherDegraded atomic.Bool

	mu      sync.Mutex
	lastMod time.Time // max vault modtime as of the last rebuild (guarded by mu)
	watcher *fsnotify.Watcher
	onSwap  func(*Index) // optional hook for the NativeFileEmitter (M3)

	// allRules is the FULL loaded slice (every status, including drafts) kept
	// for the lifecycle sweep, which must SEE drafts to promote them (M3-R1).
	// The PUBLISHED index (ptr) is the status-filtered subset {active,live}; the
	// hot path never sees a draft. Guarded by mu.
	allRules []Rule
}

// NewStore loads the vault at dir and publishes the initial index. An empty dir
// (ESSAIM_VAULT unset) yields a Store with a nil index = no rules = no injection,
// cleanly. It does NOT start watching; call Watch to enable live reload.
func NewStore(dir string) (*Store, error) {
	s := &Store{dir: dir, debounce: defaultDebounce, poll: defaultPollInterval}
	if err := s.rebuild(); err != nil {
		return nil, err
	}
	return s, nil
}

// SetPollInterval overrides the poll-fallback period (P0-4). 0 disables the
// fallback. Must be called before Watch. Primarily a test/config seam.
func (s *Store) SetPollInterval(d time.Duration) { s.poll = d }

// WatcherDegraded reports whether the poll fallback has had to re-index a change
// fsnotify never reported — i.e. inotify is not delivering on this filesystem.
// Sticky once set. The server ORs it into /health.
func (s *Store) WatcherDegraded() bool {
	if s == nil {
		return false
	}
	return s.watcherDegraded.Load()
}

// Index returns the current immutable index via the atomic pointer (the hot-path
// read). May be nil before the first successful rebuild / when no vault is set.
func (s *Store) Index() *Index { return s.ptr.Load() }

// SetOnSwap registers a callback invoked (off the request path) after each index
// swap — the seam the NativeFileEmitter consumes. Safe to leave nil.
func (s *Store) SetOnSwap(fn func(*Index)) {
	s.mu.Lock()
	s.onSwap = fn
	s.mu.Unlock()
}

// rebuild reloads the vault, builds a fresh immutable index, and atomically
// swaps it in (build-then-swap). It degrades to STALE, never to EMPTY, on a
// transient FS error (P1-4): if the load yields 0 rules AND either the dir is
// now missing/unreadable OR the previously-published index was non-empty, the
// last-good index is KEPT and the store is marked degraded — a single fsnotify
// event firing while the dir is briefly gone (editor atomic save-via-rename,
// OneDrive/AV lock) must not silently wipe every live rule with degraded=false.
//
// LoadVault loads EVERY rule (every status, incl. drafts in _inbox/) — the
// lifecycle sweep needs to see them. The Store keeps that full slice; the
// PUBLISHED index is the status-filtered {active,live} subset (M3-R1), so a
// draft never enters the matchable index nor any tool's context.
func (s *Store) rebuild() error {
	// Capture the poll baseline BEFORE reading files (gemini review — TOCTOU): if
	// an external write lands DURING LoadVault, LoadVault may read the old content
	// while a post-load mtime snapshot would swallow the new mtime, so the poll
	// would never re-fire for that change. Snapshotting the max mtime first means
	// any concurrent-or-later write registers as strictly newer than lastMod, so
	// the next pollOnce catches it. On a hard load error we do NOT advance the
	// baseline (the poll keeps retrying); we only commit it once the load succeeds.
	beforeMod, hasMod := maxModTime(s.dir)

	rs, err := LoadVault(s.dir)
	if err != nil {
		return err
	}
	ix := BuildIndex(InjectableRules(rs)) // publish only {active,live}

	// Commit the pre-load baseline on BOTH the deliberate P1-4 stale-preserve path
	// AND the healthy publish path — in both we have SEEN and handled this on-disk
	// state. Advance monotonically so a stale re-read never rewinds the baseline.
	if hasMod {
		s.mu.Lock()
		if beforeMod.After(s.lastMod) {
			s.lastMod = beforeMod
		}
		s.mu.Unlock()
	}

	// P1-4 guard: a 0-rule load is only published when it is the HONEST empty
	// state (the dir exists and readable AND we did not previously have rules).
	// Otherwise keep the last-good index and degrade.
	if len(rs) == 0 {
		prev := s.ptr.Load()
		prevNonEmpty := prev != nil && prev.Len() > 0
		if s.dir != "" && (dirMissingOrUnreadable(s.dir) || prevNonEmpty) {
			s.degraded.Store(true)
			return nil // KEEP the last-good index; do NOT publish empty
		}
	}

	s.mu.Lock()
	s.allRules = rs // keep the full slice for the lifecycle sweep
	cb := s.onSwap
	s.mu.Unlock()
	s.ptr.Store(ix)         // atomic publish; readers see old-or-new, never a torn index
	s.degraded.Store(false) // a healthy publish clears the degraded flag
	if cb != nil {
		cb(ix)
	}
	return nil
}

// Degraded reports whether the last rebuild kept a stale index instead of
// publishing an empty one (P1-4). The server ORs this into /health's degraded.
func (s *Store) Degraded() bool {
	if s == nil {
		return false
	}
	return s.degraded.Load()
}

// dirMissingOrUnreadable reports whether dir cannot be stat'd as a directory
// (gone, not-a-dir, or permission-denied) — the transient-FS-error signal P1-4
// uses to decide "keep the last-good index, don't wipe to empty."
func dirMissingOrUnreadable(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil {
		return true
	}
	return !info.IsDir()
}

// AllRules returns a copy of the FULL loaded rule slice (every status), for the
// lifecycle sweep. The published hot-path index is status-filtered; this is the
// unfiltered view the sweep needs to promote drafts (M3-R1).
func (s *Store) AllRules() []Rule {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Rule(nil), s.allRules...)
}

// Dir returns the vault directory (the lifecycle sweep + the extractor write
// drafts/active rules under it).
func (s *Store) Dir() string { return s.dir }

// Rebuild forces a synchronous reload+republish (used by the lifecycle sweep
// and the extractor after they write a rule, so the new state is visible without
// waiting for the fsnotify debounce).
func (s *Store) Rebuild() error { return s.rebuild() }

// maxModTime returns the latest modification time across every entry (files and
// dirs) under dir and whether any was found. It is the cheap change-detector for
// the poll fallback: a stat-walk with no file reads. Unreadable entries are
// skipped so one bad file never blinds detection.
func maxModTime(dir string) (time.Time, bool) {
	if dir == "" {
		return time.Time{}, false
	}
	var newest time.Time
	var any bool
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if m := info.ModTime(); m.After(newest) {
			newest = m
		}
		any = true
		return nil
	})
	return newest, any
}

// pollOnce is one tick of the mtime-gated poll fallback (P0-4). It rebuilds iff
// the vault's max modtime advanced past the last rebuild — a change fsnotify did
// not deliver — and returns whether it rebuilt. The first such rebuild flags the
// watcher degraded (inotify is not delivering on this filesystem). On a healthy
// inotify FS the fsnotify path keeps lastMod current, so this is a no-op scan.
func (s *Store) pollOnce() bool {
	m, ok := maxModTime(s.dir)
	if !ok {
		return false
	}
	s.mu.Lock()
	stale := m.After(s.lastMod)
	s.mu.Unlock()
	if !stale {
		return false
	}
	s.watcherDegraded.Store(true) // the poll had to cover for fsnotify
	_ = s.rebuild()               // refreshes lastMod via recordVaultMod
	return true
}

// Watch starts the fsnotify watcher on the vault dir (recursively) and rebuilds
// the index (debounced) on any change. It returns once the watcher is armed; the
// watch loop runs until ctx is cancelled or Close is called. A nil/empty dir is a
// no-op (no vault, nothing to watch).
func (s *Store) Watch(ctx context.Context) error {
	if s.dir == "" {
		return nil
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.watcher = w
	s.mu.Unlock()

	// Watch the root and every existing subdirectory (fsnotify is non-recursive).
	_ = addDirsRecursive(w, s.dir)

	go s.loop(ctx, w)
	return nil
}

// Close stops the watcher.
func (s *Store) Close() error {
	s.mu.Lock()
	w := s.watcher
	s.watcher = nil
	s.mu.Unlock()
	if w != nil {
		return w.Close()
	}
	return nil
}

func (s *Store) loop(ctx context.Context, w *fsnotify.Watcher) {
	var (
		timer   *time.Timer
		timerCh <-chan time.Time
	)
	arm := func() {
		if timer == nil {
			timer = time.NewTimer(s.debounce)
		} else {
			timer.Reset(s.debounce)
		}
		timerCh = timer.C
	}

	// Mtime-gated poll fallback (P0-4): fires a scan on a filesystem where
	// fsnotify is dead (WSL2 /mnt/c, network FS). Near-free when inotify works.
	var pollCh <-chan time.Time
	if s.poll > 0 {
		pt := time.NewTicker(s.poll)
		defer pt.Stop()
		pollCh = pt.C
	}

	for {
		select {
		case <-ctx.Done():
			_ = w.Close()
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			// A newly created directory must be added to the watch set so rules
			// dropped into a new folder are picked up.
			if ev.Op&fsnotify.Create != 0 {
				_ = addDirsRecursive(w, ev.Name)
			}
			arm()
		case _, ok := <-w.Errors:
			if !ok {
				return
			}
			// Ignore individual watcher errors; the next event re-arms a rebuild.
		case <-timerCh:
			_ = s.rebuild()
			timerCh = nil
		case <-pollCh:
			s.pollOnce()
		}
	}
}

// addDirsRecursive adds dir and all its subdirectories to the watcher. Errors on
// individual paths are ignored (best-effort); a file path passed here is a no-op.
func addDirsRecursive(w *fsnotify.Watcher, root string) error {
	info, err := statDir(root)
	if err != nil || !info {
		return nil
	}
	return walkDirs(root, func(d string) { _ = w.Add(d) })
}
