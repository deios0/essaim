package inject

import (
	"bytes"
	"encoding/json"
	"strconv"

	"oikos/internal/rules"
)

// Parsed holds the result of ONE structural parse of the request body so the
// hot path never scans the (multi-MB) body twice (P0-1). The server computes the
// match query via LastUser() and then injects via Build() over the SAME parse —
// eliminating the prior double scan (LastUserMessage + Build each re-parsed).
type Parsed struct {
	body     []byte
	msgs     []message
	arrStart int
	arrEnd   int
	ok       bool
}

// Parse structurally scans body ONCE and returns a reusable Parsed. ok is false
// (caller fails open verbatim) on a missing/degenerate messages array — exactly
// the cases parseMessages rejects.
func Parse(body []byte) Parsed {
	msgs, arrStart, arrEnd, ok := parseMessages(body)
	return Parsed{body: body, msgs: msgs, arrStart: arrStart, arrEnd: arrEnd, ok: ok && len(msgs) > 0}
}

// OK reports whether the body parsed into a non-empty messages array.
func (p Parsed) OK() bool { return p.ok }

// LastUser returns the flattened text of the LAST role:"user" message (the
// T0/T1 match query) without re-parsing the body.
func (p Parsed) LastUser() string {
	if !p.ok {
		return ""
	}
	for i := len(p.msgs) - 1; i >= 0; i-- {
		if p.msgs[i].role == "user" && p.msgs[i].contentOK {
			return p.msgs[i].content
		}
	}
	return ""
}

// Build performs the full B1 request-side transform on the raw request body:
//
//	STEP 0  idempotent strip of every prior oikos block (whole-element delete);
//	        compute the oikos-free clean messages for the capture snapshot.
//	STEP 2  if matched is empty → return the stripped body (nothing injected).
//	STEP 3  render+wrap one block in the inherited instruction role and splice it
//	        immediately before the client's first instruction message (or index 0
//	        when none), leaving every client message byte-identical.
//
// It does a full encoding/json Marshal of NOTHING on the big payload: all client
// bytes pass through the original buffer; only the ONE spliced element is
// serialized (Bridge finding #5). On any structural surprise it returns ErrSkip
// so the caller forwards the original bytes verbatim (fail-open).
//
// matchedJSONRole is computed from `model` for role sniffing (Bridge finding #2).
func Build(body []byte, matched []rules.Rule, matchedIDs []string) (Result, error) {
	return Parse(body).Build(matched, matchedIDs)
}

// Build runs the transform over an already-parsed body (P0-1: single parse). It
// is the canonical implementation; the package-level Build is a thin wrapper.
func (p Parsed) Build(matched []rules.Rule, matchedIDs []string) (Result, error) {
	if !p.ok {
		// No messages array (or empty) → nothing to inject into. Fail-open SKIP:
		// forward verbatim; no capture (no trusted clean_messages).
		return Result{}, ErrSkip
	}
	body, msgs, arrStart, arrEnd := p.body, p.msgs, p.arrStart, p.arrEnd

	model, _ := getModel(body)

	// STEP 0 — strip prior oikos blocks. We build a parallel list of surviving
	// message views (for role/index decisions). `stripped` is true iff at least
	// one prior block was removed (the rare re-injection path).
	clean := make([]message, 0, len(msgs))
	stripped := false
	for _, m := range msgs {
		if isOikosBlock(m) {
			stripped = true
			continue // drop our prior-turn self-block (closes B2 stacking)
		}
		clean = append(clean, m)
	}

	// The oikos-free messages array bytes for the capture snapshot. When nothing
	// was stripped this is the ORIGINAL array sub-slice (ZERO-copy — P0-1: avoid a
	// full-body copy on the hot path); only when a block was actually stripped do
	// we rebuild from the surviving spans.
	var cleanArrJSON []byte
	if !stripped {
		cleanArrJSON = body[arrStart : arrEnd+1] // sub-slice, no allocation
	} else {
		cleanArrJSON = joinElements(body, clean)
	}

	snap := Snapshot{
		CleanMessagesJSON: cleanArrJSON,
		MatchedRuleIDs:    matchedIDs,
		Model:             model,
	}

	// P1-3: the structural scanner only validates brace/bracket BALANCE, not JSON
	// grammar — a malformed-but-balanced ENVELOPE (e.g. `],"temperature"0.5}` —
	// missing colon OUTSIDE the messages array) passes the gate, and any rewrite
	// is then ALSO invalid JSON. Spec §5.1 / A-5.5 bind "any ambiguity ⇒ forward
	// the EXACT original bytes verbatim", so we MUST NOT emit different
	// still-invalid bytes. We validate the ENVELOPE GRAMMAR (everything outside the
	// messages array, with an empty-array placeholder) — O(envelope), NOT O(body):
	// re-scanning the multi-MB verbatim client content with json.Valid would blow
	// the <15ms P0-1 budget (~7ms/5MB) for no benefit, since that content is passed
	// through unchanged and was already structurally balanced.
	if !envelopeValid(body, arrStart, arrEnd) {
		return Result{}, ErrSkip
	}

	// STEP 2 — empty match. TRUE NO-OP fast path (P2 cache-stability): when NOTHING
	// will be injected (len(matched)==0) AND nothing was stripped, the result is
	// the input unchanged. Return the ORIGINAL `body` slice VERBATIM — do NOT
	// rebuild via joinElements/splice, which would re-serialize a pretty-printed
	// body and strip intra-array whitespace, busting upstream prompt-caching (the
	// splice-not-Marshal design). Only when a block WAS stripped do we emit the
	// rebuilt body.
	if len(matched) == 0 {
		if !stripped {
			return Result{Body: body, Snapshot: snap}, nil // nothing to do: verbatim
		}
		out := splice(body, arrStart, arrEnd, joinElements(body, clean))
		return Result{Body: out, Snapshot: snap}, nil
	}

	// STEP 3 — render + wrap + choose role + splice one element at ARRAY INDEX 0
	// of the STRIPPED array (B1 v1.1 A-5.3.1 / A-3.1: index-0 placement is BINDING
	// and SUPERSEDES the v1.0 "before the first instruction message" rule). Role
	// is still CHOSEN per A-2 (inherit the leading instruction role / model-sniff);
	// only the POSITION is pinned to 0. Index-0 is the only placement provably safe
	// for tool-call alternation without parsing element roles — the array head can
	// never land between an assistant{tool_calls} element and its tool replies
	// (A-3). chooseInjectRole reads the STRIPPED array, never the raw input, so a
	// prior block can never influence role.
	role := chooseInjectRole(clean, model)
	wrapped := rules.WrapBlock(rules.RenderBody(matched))
	blockJSON := encodeMessage(role, wrapped)

	var out []byte
	if !stripped {
		// FAST PATH (P0-1): no prior block was stripped, so every original element
		// is contiguous in `body`. Inject ONE head element with a SINGLE pre-sized
		// copy: body[:arrStart] + "[" + blockJSON + "," + body[arrStart+1:]
		// (everything after the original '[' — incl. inter-element whitespace —
		// passes through VERBATIM). This replaces the prior two-copy +
		// doubling-buffer rebuild (≈8.5× body in allocs) with one exact-size copy.
		out = headInject(body, arrStart, blockJSON)
	} else {
		// STRIP PATH (a prior oikos block was removed mid-conversation — common in
		// normal use: the NativeFileEmitter writes a block into CLAUDE.md and the
		// tool sends that file back as context). The surviving elements are NOT
		// contiguous (a hole was punched where the block stood), so we cannot
		// pass-through one tail; we must rebuild the array from the surviving spans.
		// We do it in ONE pre-sized pass (P0-1 discipline applied to the strip path):
		// exactly one allocation of the final length, no doubling buffer, no second
		// whole-array copy — preserving byte-exactness of every surviving element.
		out = headInjectStripped(body, arrStart, arrEnd, clean, blockJSON)
	}

	snap.Injected = true
	return Result{Body: out, Snapshot: snap, RuleIDs: matchedIDs}, nil
}

// envelopeValid reports whether the request envelope GRAMMAR is valid JSON,
// validating everything OUTSIDE the messages array with an empty-array
// placeholder in the array's place (P1-3). It catches a malformed-but-balanced
// envelope (the structural scanner only checks brace BALANCE) WITHOUT re-scanning
// the multi-MB verbatim client message content (which would blow the <15ms
// budget). The array elements themselves are the client's verbatim bytes — if a
// client element is itself malformed, oikos forwards it unchanged (it never made
// a valid request invalid), so it is out of scope for this gate.
func envelopeValid(body []byte, arrStart, arrEnd int) bool {
	skeleton := make([]byte, 0, arrStart+2+(len(body)-arrEnd-1))
	skeleton = append(skeleton, body[:arrStart]...) // up to (not incl) the '['
	skeleton = append(skeleton, '[', ']')           // empty-array placeholder
	skeleton = append(skeleton, body[arrEnd+1:]...) // after the ']'
	return json.Valid(skeleton)
}

// headInject builds the rewritten body for the common case "inject ONE element
// at array head, no element stripped" using a SINGLE pre-sized allocation and a
// SINGLE copy of the (huge) verbatim tail (P0-1). Layout:
//
//	body[:arrStart] + '[' + blockJSON + ',' + body[arrStart+1:]
//
// where body[arrStart+1:] is EVERYTHING after the original '[' — any post-'['
// whitespace, the first original element through the closing ']', and the rest
// of the envelope (e.g. `]}` plus trailing top-level fields like "temperature").
// Slicing from arrStart+1 (not the first element's start) keeps inter-element
// whitespace byte-VERBATIM too; every client byte from the '[' onward — and
// everything before it — is preserved; only the leading '[' + injected element +
// ',' is new. (Fast path only runs with ≥1 surviving element, so the ',' never
// leaves a trailing-comma empty array.)
func headInject(body []byte, arrStart int, blockJSON []byte) []byte {
	tail := body[arrStart+1:] // post-'[' ws … first element … ']' … envelope close … trailing fields
	head := body[:arrStart]   // everything before the '['
	exactLen := len(head) + 1 /*'['*/ + len(blockJSON) + 1 /*','*/ + len(tail)
	out := make([]byte, 0, exactLen)
	out = append(out, head...)
	out = append(out, '[')
	out = append(out, blockJSON...)
	out = append(out, ',')
	out = append(out, tail...)
	return out
}

// headInjectStripped builds the rewritten body for the STRIP path — "a prior
// oikos block was removed, then inject ONE element at array head" — using a
// SINGLE pre-sized allocation and ONE copy of every surviving span (P0-1 applied
// to the strip path). Unlike headInject, the surviving elements are NOT contiguous
// (the stripped block left a hole), so we cannot pass-through one verbatim tail;
// we splice the surviving element spans (each byte-identical to its original) with
// commas, leading with blockJSON, all in one allocation. Layout:
//
//	body[:arrStart] + '[' + blockJSON + ',' + clean[0] + ',' + clean[1] + … + ']' + body[arrEnd+1:]
//
// `clean` are the surviving (non-oikos) message views into `body`; `arrStart` /
// `arrEnd` bound the original messages array '['…']' inclusive, so body[:arrStart]
// (envelope head, e.g. `{"model":…,"messages":`) and body[arrEnd+1:] (envelope
// tail, e.g. `]}` minus the array … plus trailing top-level fields) pass through
// byte-VERBATIM. This replaces the prior two-pass rebuild (spliceElement's
// doubling bytes.Buffer ≈2× the body, then splice's whole-array re-copy ≈another
// body) with one exact-size make+copy, so the strip path meets the same <15ms
// budget as the fast path and never fail-opens the moat on the emitter→input→strip
// round-trip. (This superseded the prior spliceElement-then-splice two-pass
// rebuild that allocated ≈2× the body and could overrun the 15ms deadline.)
//
// PRECONDITION: len(clean) >= 1 (caller only reaches the strip path with
// len(matched) > 0; if clean is empty the array would be `[block]` — handled
// below by emitting no leading comma). Each clean[i] is byte-contiguous in body
// at [rawStart:rawEnd].
func headInjectStripped(body []byte, arrStart, arrEnd int, clean []message, blockJSON []byte) []byte {
	head := body[:arrStart] // everything before the '[' (envelope head)
	tail := body[arrEnd+1:] // everything after the ']' (envelope close + trailing fields)
	// Exact final length: head + '[' + blockJSON + (per-survivor: ',' + span) + ']' + tail.
	exactLen := len(head) + 1 /*'['*/ + len(blockJSON)
	for _, m := range clean {
		exactLen += 1 /*','*/ + (m.rawEnd - m.rawStart)
	}
	exactLen += 1 /*']'*/ + len(tail)

	out := make([]byte, 0, exactLen)
	out = append(out, head...)
	out = append(out, '[')
	out = append(out, blockJSON...)
	for _, m := range clean {
		out = append(out, ',')
		out = append(out, body[m.rawStart:m.rawEnd]...)
	}
	out = append(out, ']')
	out = append(out, tail...)
	return out
}

// Model reads the top-level `model` string from the raw request body using the
// object-level key discipline (depth-1 key, not an in-content substring — B1
// v1.1 A-2.1 / F-V7), so a `"model":"gpt-5"` pasted inside a user message never
// mis-routes the skip/role decision. Returns "" when absent/unreadable (treated
// as unknown by callers). Used by the server's skip-on-unsupported gate (A-2.3).
func Model(body []byte) string {
	m, _ := getModel(body)
	return m
}

// getModel reads the top-level `model` string from the raw body.
func getModel(body []byte) (string, bool) {
	s, e, vt, ok := locateValue(body, "model")
	if !ok || vt != valOther {
		return "", false
	}
	// s..e is inclusive; if it's a quoted string, unquote it.
	v := body[s : e+1]
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		u, err := strconv.Unquote(string(v))
		if err == nil {
			return u, true
		}
		return string(v[1 : len(v)-1]), true
	}
	return string(v), true
}

// joinElements rebuilds a JSON array (brackets included) from the VERBATIM byte
// spans of the given messages, joined by ",". Every surviving client message is
// byte-identical to its original bytes.
//
// P1-a-gap: this is a SINGLE pre-sized copy (sum the spans → one make → one pass),
// not a doubling bytes.Buffer + whole-array re-copy. joinElements is called for
// the capture snapshot cleanArrJSON (inject+stripped path) and the empty-match-
// stripped emit — both triggered by a large multi-turn body carrying a prior
// oikos block, the SAME trigger as the original P0-1 slow-path bug. A non-presized
// buffer that grows by doubling (and is then re-copied) can blow the <15ms budget
// on a multi-MB body and fail-open the moat; one exact-size allocation cannot.
func joinElements(body []byte, msgs []message) []byte {
	if len(msgs) == 0 {
		return []byte("[]")
	}
	// Exact final length: '[' + spans + (len-1) commas + ']'.
	exactLen := 2 /*'['+']'*/ + (len(msgs) - 1) /*commas*/
	for _, m := range msgs {
		exactLen += m.rawEnd - m.rawStart
	}
	out := make([]byte, 0, exactLen)
	out = append(out, '[')
	for i, m := range msgs {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, body[m.rawStart:m.rawEnd]...)
	}
	out = append(out, ']')
	return out
}

// splice replaces body[arrStart:arrEnd+1] (the original messages array, brackets
// inclusive) with newArr, leaving everything before arrStart and after arrEnd
// byte-identical to the original request (Bridge finding #5: only the messages
// array region is rewritten; the rest of the big payload is untouched).
func splice(body []byte, arrStart, arrEnd int, newArr []byte) []byte {
	out := make([]byte, 0, arrStart+len(newArr)+(len(body)-arrEnd-1))
	out = append(out, body[:arrStart]...)
	out = append(out, newArr...)
	out = append(out, body[arrEnd+1:]...)
	return out
}

// encodeMessage serializes a single {"role":..,"content":..} message object. The
// content is the wrapped oikos block; we JSON-escape it correctly (it contains
// no characters needing escaping beyond newlines, but we escape defensively).
func encodeMessage(role, content string) []byte {
	var b bytes.Buffer
	b.WriteString(`{"role":`)
	b.Write(encodeJSONString(role))
	b.WriteString(`,"content":`)
	b.Write(encodeJSONString(content))
	b.WriteByte('}')
	return b.Bytes()
}

// encodeJSONString writes a JSON string literal for s, escaping the characters
// JSON requires (", \, and control chars including \n). Pure, allocation-light.
func encodeJSONString(s string) []byte {
	var b bytes.Buffer
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 {
				b.WriteString(`\u00`)
				const hex = "0123456789abcdef"
				b.WriteByte(hex[c>>4])
				b.WriteByte(hex[c&0xf])
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.Bytes()
}
