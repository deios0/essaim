package extract

import (
	"sync"
	"time"
)

// seenSet is a bounded set of sigil-line-hashes for the T0 idempotency backstop
// (BR-A1): a sigil whose line-hash is already present is skipped, so a resent
// history never re-fires the same /remember. It is an ephemeral-hot structure
// (LOCAL, in-mem, NEVER frontmatter — the 3-class discipline). When it reaches
// capacity it drops the oldest insertion (a simple ring eviction).
type seenSet struct {
	mu    sync.Mutex
	set   map[string]struct{}
	order []string
	cap   int
}

func newSeenSet(capacity int) *seenSet {
	if capacity <= 0 {
		capacity = 1024
	}
	return &seenSet{set: make(map[string]struct{}, capacity), cap: capacity}
}

func (s *seenSet) has(h string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.set[h]
	return ok
}

func (s *seenSet) add(h string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.set[h]; ok {
		return
	}
	if len(s.order) >= s.cap {
		old := s.order[0]
		s.order = s.order[1:]
		delete(s.set, old)
	}
	s.set[h] = struct{}{}
	s.order = append(s.order, h)
}

// ttlSeen is a bounded, TIME-WINDOWED seen-set for the T1 retry dedup (P2-3). A
// key seen within `window` of a prior sighting is a NON-INDEPENDENT repeat (a
// retry / duplicate send); a sighting after the window has elapsed is treated as
// independent and re-admitted. It stores the LAST-seen time per key and evicts
// the oldest INSERTION when it reaches capacity (a simple ring, like seenSet), so
// memory is bounded regardless of key cardinality. All times are supplied by the
// caller (the extractor's `now` seam) so the behavior is deterministic in tests.
type ttlSeen struct {
	mu     sync.Mutex
	last   map[string]time.Time
	order  []string
	cap    int
	window time.Duration
}

func newTTLSeen(capacity int, window time.Duration) *ttlSeen {
	if capacity <= 0 {
		capacity = 1024
	}
	return &ttlSeen{last: make(map[string]time.Time, capacity), cap: capacity, window: window}
}

// seenWithin reports whether key was already seen within `window` of `now`. It
// is a test-and-set: on return it records `now` as the key's last-seen time
// regardless of the outcome, so the window always slides from the MOST RECENT
// sighting (a burst of retries keeps extending the dedup window, exactly the
// retry-storm case). A first-ever sighting (or one past the window) returns false
// and is admitted.
func (s *ttlSeen) seenWithin(key string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.last[key]
	// Record the new sighting time (slide the window) whether or not it deduped.
	if !ok {
		if len(s.order) >= s.cap {
			old := s.order[0]
			s.order = s.order[1:]
			delete(s.last, old)
		}
		s.order = append(s.order, key)
	}
	s.last[key] = now
	if !ok {
		return false // first sighting of this key
	}
	// A window <= 0 disables the window (any prior sighting dedups); otherwise the
	// repeat must fall within `window` of the previous sighting to be a retry.
	if s.window <= 0 {
		return true
	}
	return now.Sub(prev) < s.window
}
