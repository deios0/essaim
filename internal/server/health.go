package server

import (
	"encoding/json"
	"net/http"
)

// CaptureStats is the optional interface a capture sink implements to surface
// M3 learning meters on /health (BR-A2-16). All values are additive; absence
// (a nil sink or one not implementing this) simply omits them.
type CaptureStats interface {
	DraftsPending() int
	ExtractCostToday() float64
	ExtractCostCap() float64
}

// health reports liveness. It is always open (no auth), creates no state, is
// never proxied, and never phones home (spec §5.5). It reports the B1 injection
// layer's sticky degraded flag and the current rules-indexed count so a
// fail-open is observable WITHOUT being visible to the IDE (silent no-injection).
// M3 adds the capture meters additively (never breaking the existing keys).
func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	out := map[string]any{
		"status":           "ok",
		"backend":          "none",
		"degraded":         s.inj.degraded(),
		"rules_indexed":    s.inj.rulesIndexed(),
		"watcher_degraded": s.inj.watcherDegraded(),
	}
	if s.captureSink != nil {
		out["capture_dropped_bytes"] = s.capture.droppedBytes.Load()
		out["capture_dropped"] = s.capture.dropped.Load()
		out["captures_enqueued"] = s.capture.enqueued.Load()
		if cs, ok := s.captureSink.(CaptureStats); ok {
			out["drafts_pending"] = cs.DraftsPending()
			out["extract_cost_today"] = cs.ExtractCostToday()
			out["extract_cost_cap"] = cs.ExtractCostCap()
		}
		// Surface the learner's queue-drop count so silent moat degradation (a full
		// learning queue dropping captures) is observable (P2). Optional interface.
		if qd, ok := s.captureSink.(interface{ QueueDropped() int64 }); ok {
			out["queue_dropped"] = qd.QueueDropped()
		}
	}
	_ = json.NewEncoder(w).Encode(out)
}
