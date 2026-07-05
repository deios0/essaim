package extract

import (
	"strings"
	"sync"
	"time"
)

// Config holds the T2 cheap-async-LLM-gate knobs (M3-R6). T2 is the ONLY path
// that may contact a network, and it is DEFAULT OFF: AllowCloud must be
// explicitly enabled AND it is read ONCE at startup (flipping the completions
// key never flips it — BR-A12). It is local-preferred with a visible per-day
// COST cap (not a call-count cap).
type Config struct {
	// AllowCloud is the extract.allow_cloud opt-in (default false). With it
	// false, T2 NEVER runs and NEVER contacts any network.
	AllowCloud bool
	// CostCapPerDay is the visible per-day spend cap (currency units). When the
	// day's spend reaches the cap, T2 stops.
	CostCapPerDay float64
	// Gate is the LLM gate function: given a re-templated WHAT/HOW/BETTER prompt
	// it returns the raw model output and the call's cost. Pluggable so tests run
	// with NO network. nil ⇒ T2 is a no-op even when AllowCloud is true.
	Gate GateFunc

	// meter is the per-day cost meter (reset when the UTC day rolls). It is a
	// POINTER so a Config can be copied by value without copying the mutex (go
	// vet copylocks). Initialized lazily by ensureMeter.
	meter *costMeter
}

// ensureMeter lazily allocates the cost meter (so a zero Config is usable and
// copyable). Not concurrency-safe to call from multiple goroutines before the
// first use; the Extractor calls it once at construction.
func (c *Config) ensureMeter() {
	if c.meter == nil {
		c.meter = &costMeter{}
	}
}

// GateFunc runs the LLM gate and returns the raw output + the call cost. A
// transport error returns ("", 0, err); the caller treats any error as SKIP.
type GateFunc func(prompt string) (out string, cost float64, err error)

// GATE_PROMPT is the WHAT/HOW/BETTER rigid schema (ported from the reference lesson-extractor
// GATE_PROMPT), RE-TEMPLATED from a git-diff to a chat
// exchange: the input is the user correction + the assistant answer (NOT a
// diff). The three exact lines or exactly SKIP contract is preserved.
const GATE_PROMPT = `You distill ONE reusable engineering lesson from a chat exchange, for a personal knowledge base.

Output RULES:
- If the exchange teaches something reusable (a stated preference, a correction, a non-obvious gotcha, a "do it this way next time" insight) output EXACTLY three lines:
WHAT: <one line — what the user wants / what was corrected>
HOW: <one line — the key rule/approach to apply>
BETTER: <one line — the reusable rule for next time>
- If the exchange is ROUTINE with no reusable lesson (small talk, a one-off question, an answer with no preference stated) output EXACTLY: SKIP
- No preamble, no markdown, no extra lines. Be specific to THIS exchange, not generic.

User: {user}

Assistant: {assistant}
`

// buildGatePrompt fills the chat-exchange template.
func buildGatePrompt(ex Exchange) string {
	p := strings.ReplaceAll(GATE_PROMPT, "{user}", strings.TrimSpace(ex.UserText))
	p = strings.ReplaceAll(p, "{assistant}", truncate(strings.TrimSpace(ex.AssistantText), 2000))
	return p
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// runGate ports the run_gate parse EXACTLY: SKIP / empty
// / no "WHAT:" → no lesson; otherwise keep ONLY the line-anchored WHAT/HOW/BETTER
// lines; empty keep → no lesson (reference engine returns None; closes
// F-9 — a prose body merely Contains("WHAT:") yields keep==[] → no write).
func runGate(out string) (lesson string, ok bool) {
	out = strings.TrimSpace(out)
	if out == "" || strings.HasPrefix(strings.ToUpper(out), "SKIP") || !strings.Contains(out, "WHAT:") {
		return "", false
	}
	var keep []string
	for _, ln := range strings.Split(out, "\n") {
		head := strings.ToUpper(strings.TrimSpace(strings.SplitN(ln, ":", 2)[0]))
		if head == "WHAT" || head == "HOW" || head == "BETTER" {
			keep = append(keep, ln)
		}
	}
	if len(keep) == 0 {
		return "", false
	}
	return strings.Join(keep, "\n"), true
}

// runT2 runs the cheap LLM gate on an exchange when T1 cleared its gate. It is
// only reached when AllowCloud is true AND the capture is not lossy. On a
// non-SKIP result it REFINES the existing T1 draft body (BR-A11) — it does NOT
// create a second draft. Cost is metered against the per-day cap; once the cap
// is reached, T2 stops.
func (e *Extractor) runT2(ex Exchange, t1Path string) (refinedPath string, ok bool) {
	if e.cfg.Gate == nil {
		return "", false
	}
	if e.cfg.CostCapPerDay > 0 && e.cfg.meter.spent(e.now().UTC()) >= e.cfg.CostCapPerDay {
		return "", false // cap reached — stop
	}
	out, cost, err := e.cfg.Gate(buildGatePrompt(ex))
	if err != nil {
		return "", false
	}
	e.cfg.meter.add(e.now().UTC(), cost)
	lesson, gok := runGate(out)
	if !gok {
		return "", false
	}
	// Refine the existing T1 draft body in place (same file).
	if err := e.refineDraft(t1Path, lesson); err != nil {
		return "", false
	}
	return t1Path, true
}

// CostToday returns the metered T2 spend for the current UTC day (for /health).
func (c *Config) CostToday(now time.Time) float64 {
	if c.meter == nil {
		return 0
	}
	return c.meter.spent(now.UTC())
}

// costMeter tracks per-UTC-day T2 spend, resetting when the day rolls.
type costMeter struct {
	mu    sync.Mutex
	day   string
	total float64
}

func (m *costMeter) add(now time.Time, cost float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := now.Format("2006-01-02")
	if d != m.day {
		m.day = d
		m.total = 0
	}
	m.total += cost
}

func (m *costMeter) spent(now time.Time) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := now.Format("2006-01-02")
	if d != m.day {
		return 0
	}
	return m.total
}
