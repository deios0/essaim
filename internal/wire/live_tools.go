package wire

import (
	"os"
	"sync"

	"essaim/internal/config"
)

// LiveWiredTools returns the predicate the heal watcher uses to learn the CURRENT
// set of wired-tool names (what essaim's config.json holds right now), so a tool
// removed by `essaim unwire` on a running daemon is no longer healed — no restart
// (RESPECTS-UNWIRE / P1). It is the live overlay over the boot-time heal targets.
//
// It is CHEAP on the hot pass: it stats config.json and only re-reads/re-parses
// when the file's mtime+size changes (an mtime cache). A pass where the config is
// unchanged costs one Stat. The predicate is goroutine-safe (the heal loop and an
// explicit CheckOnce may both call it).
//
// Contract (matches heal.Watcher.SetLiveTools): it returns (set, true) when it
// could read the live wired-tool set, or (_, false) when it could not — a config
// path it can't resolve, a missing file, or any read/parse error. On false the
// watcher FAILS TOWARD HEALING (keeps guarding), so a transient unreadable config
// never silently drops the guard. The returned set holds every wired tool's name
// (channel-agnostic — the heal targets are already restricted to base_url tools,
// so gating by name alone is sufficient and robust to a re-wire that changes a
// tool's channel).
func LiveWiredTools() func() (map[string]bool, bool) {
	var (
		mu       sync.Mutex
		haveStat bool
		lastMod  int64
		lastSize int64
		cached   map[string]bool
	)
	return func() (map[string]bool, bool) {
		p, err := config.Path()
		if err != nil || p == "" {
			return nil, false // can't locate the config — undeterminable
		}
		info, err := os.Stat(p)
		if err != nil {
			return nil, false // missing/unreadable — fail toward healing
		}

		mu.Lock()
		defer mu.Unlock()

		mod, size := info.ModTime().UnixNano(), info.Size()
		if haveStat && mod == lastMod && size == lastSize && cached != nil {
			return cached, true // mtime cache hit — no re-read
		}

		c, err := config.Load()
		if err != nil {
			return nil, false // unparseable — undeterminable, keep healing
		}
		// Key the live set by BOTH the tool NAME (heal's identity — base_url tools)
		// AND the NATIVE-FILE path (the emitter's identity). Native-file records are
		// keyed per (name, native_file), so two projects can wire the same tool name
		// (e.g. "claude-code"); a name-only set would report the tool live after one
		// project is unwired and keep the emitter writing the OTHER project's file.
		// Names and absolute paths never collide, so one map serves both consumers.
		set := make(map[string]bool, len(c.WiredTools)*2)
		for _, wt := range c.WiredTools {
			set[wt.Name] = true
			if wt.NativeFile != "" {
				set[wt.NativeFile] = true
			}
		}
		cached, lastMod, lastSize, haveStat = set, mod, size, true
		return set, true
	}
}
