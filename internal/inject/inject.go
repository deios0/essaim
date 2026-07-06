// Package inject implements the B1 request-side injection mechanic: it splices
// exactly one leading essaim rule block into an OpenAI `POST /v1/chat/completions`
// body, idempotently (strip-then-inject, never stacks), in the client's own
// instruction role, while leaving every client message byte-identical.
//
// The hot-path transform operates on the RAW request bytes via jsonparser
// (zero-alloc over the big payload — Bridge finding #5: a full encoding/json
// Marshal of a 5MB body costs 30-80ms and blows the <15ms budget). Only the ONE
// spliced leading element is serialized; all other client bytes pass through the
// original buffer untouched.
package inject

import (
	"errors"
	"strings"

	"essaim/internal/rules"
	"github.com/buger/jsonparser"
)

// Re-export the sentinels so callers in this package read them locally.
const (
	begin = rules.ESSAIM_BEGIN
	end   = rules.ESSAIM_END
)

// instructionRoles is the SET of roles that count as a leading instruction
// message (spec R-3): OpenAI replaced `system` with `developer` for o-series /
// reasoning models.
var instructionRoles = map[string]bool{"system": true, "developer": true}

func isInstruction(role string) bool { return instructionRoles[role] }

// ErrSkip signals that injection must be skipped entirely and the original bytes
// forwarded verbatim (fail-open) — used when there are no messages to inject
// into (no messages array / empty / degenerate input).
var ErrSkip = errors.New("essaim: injection skipped (fail-open)")

// ErrInjectUnsupported signals that the resolved target (model or wired upstream)
// would REJECT an extra leading instruction message — i.e. it would 400 on an
// injected system/developer element (B1 v1.1 A-2.3). essaim SKIPS injection
// entirely: forward the VERBATIM original bytes (including any pre-existing stale
// essaim block — strip is NOT run on the wire), enqueue NO capture. This is an
// honest, policy-driven no-injection — degraded=FALSE (distinct from
// ErrDeadline/ErrPanic). Never turn a working request into a 400.
var ErrInjectUnsupported = errors.New("essaim: injection unsupported by target (skip, fail-open)")

// message is a lightweight view of one chat message extracted from the raw body.
// rawStart/rawEnd bound the message's original bytes in the request buffer so it
// can be spliced back VERBATIM (byte-identical) without re-serialization.
type message struct {
	role     string
	rawStart int // offset of the message object's '{' in the original body
	rawEnd   int // offset just past the message object's '}'
	// content is the flattened text content (string content, or concatenation of
	// text parts for multimodal). ok=false ⇒ not flattenable (e.g. pure image
	// parts) ⇒ never recognized as an essaim block (fail-safe).
	content   string
	contentOK bool
}

// Snapshot is the pre-injection capture struct (spec §4.1) plumbed to the
// async learning loop in a LATER milestone (M3). M2 only POPULATES it; it does
// NOT extract corrections. CleanMessages is the post-strip, essaim-free messages
// array as raw JSON bytes (the array value, including brackets).
type Snapshot struct {
	// CleanMessagesJSON is the raw JSON of the messages array AFTER stripping any
	// prior essaim block (essaim-free). Never the injected array, never raw bytes
	// that may carry a prior block (spec R-5). nil when there were no messages.
	CleanMessagesJSON []byte
	// MatchedRuleIDs travels out-of-band (never in the messages array) so it can
	// never leak into the prompt or be captured back (spec §4.1).
	MatchedRuleIDs []string
	// Model is the request `model` field (for role sniffing / later routing).
	Model string
	// Injected reports whether a block was actually spliced this turn.
	Injected bool
}

// Result is the output of Build: the (possibly) rewritten body plus the capture
// snapshot and the matched rule IDs for async usage recording.
type Result struct {
	Body     []byte
	Snapshot Snapshot
	RuleIDs  []string
}

// modelWantsDeveloper reports whether the request `model` is an o-series /
// GPT-5.x reasoning model that uses the `developer` instruction role (Bridge
// finding #2). Matching is on the model id prefix, case-insensitive.
func modelWantsDeveloper(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	// Strip a provider prefix like "openai/" so "openai/o1" matches.
	if i := strings.LastIndexByte(m, '/'); i >= 0 {
		m = m[i+1:]
	}
	switch {
	case strings.HasPrefix(m, "o1"),
		strings.HasPrefix(m, "o3"),
		strings.HasPrefix(m, "o4"),
		strings.HasPrefix(m, "gpt-5"):
		return true
	}
	return false
}

// chooseInjectRole picks the essaim block's role (spec §2.2 + Bridge finding #2):
//  1. the role of the FIRST instruction-role message, if any (inherit the
//     client's leading instruction role);
//  2. else, if the model is an o-series/GPT-5.x reasoning model, "developer";
//  3. else, if ANY message uses role "developer", "developer";
//  4. else "system".
func chooseInjectRole(msgs []message, model string) string {
	for _, m := range msgs {
		if isInstruction(m.role) {
			return m.role
		}
	}
	if modelWantsDeveloper(model) {
		return "developer"
	}
	for _, m := range msgs {
		if m.role == "developer" {
			return "developer"
		}
	}
	return "system"
}

// isEssaimBlock implements the 3-factor, byte-0-anchored, payload-AGNOSTIC
// recognizer (spec §2.6 + Bridge finding #1): a message is an essaim block IFF
//  1. role ∈ instructionRoles (never strip a user/assistant/tool message),
//  2. flattened content STARTS WITH begin+"\n" (anchored, not "contains"),
//  3. flattened content ENDS WITH "\n"+end (the message is ENTIRELY the block).
//
// P2-3: factors 2/3 are checked against the SURROUNDING-WHITESPACE-NORMALIZED
// content (leading/trailing whitespace + newlines trimmed). WrapBlock emits the
// block with NO surrounding whitespace, but a tool/IDE round-trip that re-serves
// our prior block can append a trailing '\n' (or prepend one) — a cosmetic
// difference that left the old EXACT-equality gate (HasSuffix "\n"+end) unmatched,
// so the stale block was NOT stripped and the fresh injection STACKED on it
// turn-over-turn. Trimming only OUTER whitespace still requires the sentinels to
// bound the ENTIRE (trimmed) message — inner prose after END, a mid-text marker,
// or a partial sentinel are all still rejected — so recognition is not widened to
// any non-essaim content (role-first factor 1 still protects user/assistant echoes).
//
// Recognition keys on the WRAPPER sentinels ONLY, never the enclosed rule bodies
// or any hash — so a prior block whose payload changed (rules changed mid-convo)
// is still stripped → exactly one block survives, never A+B (Bridge finding #1,
// Tests 8/11b).
func isEssaimBlock(m message) bool {
	if !isInstruction(m.role) {
		return false
	}
	if !m.contentOK {
		return false
	}
	c := strings.TrimSpace(m.content)
	return strings.HasPrefix(c, begin+"\n") && strings.HasSuffix(c, "\n"+end)
}

// parseMessages extracts the messages array from the raw body using the
// hand-rolled structural scanner (scan.go) — NOT a full unmarshal (Bridge
// finding #5). It returns the messages (with raw byte bounds, so each can be
// spliced back byte-identically), the byte offsets of the array's opening '['
// and closing ']' within body, and ok. jsonparser is used only to read scalar
// fields out of the already-bounded element bytes. Multimodal content is
// flattened by concatenating `type=="text"` parts; non-flattenable content sets
// contentOK=false.
//
// ok is false (caller fails open verbatim) when: there is no messages array; the
// array (or any element value) is unbalanced/truncated (P3); OR any element is
// NOT a JSON object (e.g. null/number/string/nested-array). A non-object element
// is degenerate input — rebuilding the array would silently DROP it and change
// the bytes — so we forward the original request VERBATIM instead (P2 fail-open).
func parseMessages(body []byte) (msgs []message, arrStart, arrEnd int, ok bool) {
	// Envelope must be a single balanced object with no trailing garbage; a
	// truncated envelope or trailing junk (e.g. `{"messages":[...]} GARBAGE`) →
	// fail open verbatim (P3). locateArray alone validates only the array value.
	if !bodyWellFormed(body) {
		return nil, 0, 0, false
	}
	arrStart, arrEnd, ok = locateArray(body, "messages")
	if !ok {
		return nil, 0, 0, false
	}
	spans, sok := splitArrayElements(body, arrStart, arrEnd)
	if !sok {
		return nil, 0, 0, false // unbalanced/truncated element → fail open (P3)
	}
	for _, span := range spans {
		s, e := span[0], span[1]
		if body[s] != '{' {
			// Non-object element (null/number/string/array). Dropping it would
			// byte-change the array; forward the request VERBATIM instead (P2).
			return nil, 0, 0, false
		}
		obj := body[s : e+1]
		role, _ := jsonparser.GetString(obj, "role")
		content, cok := flattenContent(obj)
		msgs = append(msgs, message{
			role:      role,
			rawStart:  s,
			rawEnd:    e + 1, // exclusive
			content:   content,
			contentOK: cok,
		})
	}
	return msgs, arrStart, arrEnd, true
}

// LastUserMessage returns the flattened text content of the LAST role:"user"
// message in the raw body — the T0/T1 match query (design closure §5: match over
// the last user message). Returns "" when there is no user message or it has no
// flattenable text. Uses the structural scanner (no full unmarshal).
//
// It is a thin wrapper over Parse().LastUser(); the hot path uses Parse once and
// shares the parse with Build (P0-1), so this stand-alone form is for callers
// that only need the query.
func LastUserMessage(body []byte) string {
	return Parse(body).LastUser()
}

// flattenContent extracts a message's text content. A string content flattens to
// itself; a multimodal parts array flattens to the concatenation of its
// `type=="text"` parts' `text` fields. Anything else (or a non-text-only parts
// array that can't produce a string starting with the sentinel) is reported as
// not-flattenable (contentOK=false) so it is NEVER treated as an essaim block
// (fail-safe: never strip a client message, spec §2.6).
func flattenContent(msg []byte) (string, bool) {
	v, typ, _, err := jsonparser.Get(msg, "content")
	if err != nil {
		return "", false
	}
	switch typ {
	case jsonparser.String:
		s, err := jsonparser.ParseString(v)
		if err != nil {
			return "", false
		}
		return s, true
	case jsonparser.Array:
		var sb strings.Builder
		any := false
		_, _ = jsonparser.ArrayEach(v, func(part []byte, dt jsonparser.ValueType, _ int, _ error) {
			if dt != jsonparser.Object {
				return
			}
			pt, _ := jsonparser.GetString(part, "type")
			if pt == "text" {
				if txt, err := jsonparser.GetString(part, "text"); err == nil {
					sb.WriteString(txt)
					any = true
				}
			}
		})
		if !any {
			return "", false
		}
		return sb.String(), true
	default:
		return "", false
	}
}
