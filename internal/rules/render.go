package rules

import (
	"sort"
	"strings"
)

// Sentinel literals (spec §2.1, R-1). FIXED ASCII, no per-turn data — the same
// two literals are used by the proxy fence, the NativeFileEmitter fence, and the
// capture-tap recognizer (one delimiter, three readers). They live here so the
// rule-store (render), the injector (strip/wrap), and the future emitter all
// share one definition.
//
// CRITICAL (Bridge finding #1): the strip condition keys on these sentinels ONLY
// (role + exact fences), NEVER on a payload hash — the payload changes when rules
// change mid-conversation, so a hash-gated strip would fail to self-match a prior
// block and would stack/leak. See inject.isOikosBlock.
const (
	OIKOS_BEGIN = "<!-- oikos:rules:begin v=1 -->"
	OIKOS_END   = "<!-- oikos:rules:end -->"
)

// ManagedRegion locates oikos's OWN fenced block in s: the first line-anchored,
// solo-line OIKOS_BEGIN paired with the next line-anchored, solo-line OIKOS_END.
// It returns the half-open byte range [start,end) covering both sentinels and
// whether such a pair was found. It is the single, shared recognizer used by the
// native-file emitter AND the wire/unwire restorer so both agree on exactly what
// is oikos-managed.
//
// Recognizing a marker ONLY when it is line-anchored (offset 0 or right after a
// '\n') AND alone on its line (immediately followed by end-of-line or EOF) is the
// exact shape WrapBlock always writes ("BEGIN\n…\nEND"). A marker a user embedded
// INLINE in prose is never mistaken for the managed block, so neither emit nor
// unwire can splice from a user's inline sentinel into oikos's block and delete
// the content in between. If a solo BEGIN is followed by ANOTHER solo BEGIN
// before any solo END, the first is an orphan (user-written) and is skipped so
// the real, tightly-paired block is found.
func ManagedRegion(s string) (start, end int, ok bool) {
	for i := 0; ; {
		bi := indexSoloMarker(s, i, OIKOS_BEGIN)
		if bi < 0 {
			return 0, 0, false
		}
		after := bi + len(OIKOS_BEGIN)
		endAt := indexSoloMarker(s, after, OIKOS_END)
		if endAt < 0 {
			// No solo END anywhere after this BEGIN — no later BEGIN (which searches
			// a subset) can find one either, so no pair is possible (gemini review).
			return 0, 0, false
		}
		nextBegin := indexSoloMarker(s, after, OIKOS_BEGIN)
		if nextBegin < 0 || endAt < nextBegin {
			return bi, endAt + len(OIKOS_END), true
		}
		i = nextBegin // this BEGIN was orphan; adopt the next one
	}
}

// indexSoloMarker returns the byte offset of the first line-anchored, solo-line
// occurrence of mark at or after `from`, or -1. Inline/quoted occurrences are
// skipped.
func indexSoloMarker(s string, from int, mark string) int {
	for i := from; i+len(mark) <= len(s); {
		j := strings.Index(s[i:], mark)
		if j < 0 {
			return -1
		}
		at := i + j
		if isSoloLineMarker(s, at, mark) {
			return at
		}
		i = at + len(mark)
	}
	return -1
}

// isSoloLineMarker reports whether the sentinel mark at byte offset i in s is
// line-anchored (start of file or immediately after '\n') AND alone on its line
// (immediately followed by end-of-line — '\n', a '\r' of a CRLF, or EOF). This
// matches an oikos-written sentinel but never a marker embedded in a line of
// prose. CRLF native files (common on Windows — P0-4's platform) are tolerated.
func isSoloLineMarker(s string, i int, mark string) bool {
	if i < 0 || i+len(mark) > len(s) || s[i:i+len(mark)] != mark {
		return false
	}
	if i != 0 && s[i-1] != '\n' {
		return false
	}
	after := i + len(mark)
	return after == len(s) || s[after] == '\n' || s[after] == '\r'
}

// renderLine renders ONE rule to its single deterministic body line, using
// DURABLE fields only (no last_used/hits/timestamp/nonce) so the block is
// byte-stable turn-over-turn for an unchanged matched set (spec §2.4 invariant).
func renderLine(r Rule) string {
	return "- [" + confBucket(r) + "] " + r.Title + ": " + oneline(r.Body)
}

// RenderBody renders the matched, kept rules into the deterministic injection
// body (spec §2.4). It sorts defensively (PRIMARY: effWeight DESC; TIEBREAK: id
// ASC; STABLE) so the output is identical regardless of input order, then writes
// one line per rule. The result is cache-stable: same set + same durable weights
// ⇒ byte-identical body across calls/turns/machines.
func RenderBody(matched []Rule) string {
	cp := append([]Rule(nil), matched...)
	sort.SliceStable(cp, func(a, b int) bool {
		wa, wb := cp[a].effWeight(), cp[b].effWeight()
		if wa != wb {
			return wa > wb
		}
		return cp[a].ID < cp[b].ID
	})
	var b strings.Builder
	for i, r := range cp {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(renderLine(r))
	}
	return b.String()
}

// WrapBlock wraps a rendered body in the sentinel fence: BEGIN + "\n" + body +
// "\n" + END (spec §2.3 STEP 3). This is the exact content string of the
// injected instruction message. The body is defanged of any embedded sentinel
// so the block always carries exactly one, well-formed BEGIN…END pair (a body
// that quoted OIKOS_END would otherwise create a nested fence the emitter's
// region-replacer mis-bounds, corrupting the user's native file).
func WrapBlock(body string) string {
	return OIKOS_BEGIN + "\n" + sentinelDefang.Replace(body) + "\n" + OIKOS_END
}

// sentinelDefang neutralises a sentinel literal that slipped INTO a body (e.g. a
// learned rule that quotes "<!-- oikos:rules:end -->"). Replacing the inner ':'
// with a space breaks the exact-match recognizers (emitter region-replacer + the
// proxy strip recognizer) while keeping the text readable. Bodies are normally
// sentinel-free; this is belt-and-suspenders for the dogfood case.
var sentinelDefang = strings.NewReplacer(
	OIKOS_BEGIN, "<!-- oikos rules begin v=1 -->",
	OIKOS_END, "<!-- oikos rules end -->",
)
