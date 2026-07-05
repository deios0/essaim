package inject

// This file hand-rolls the minimal JSON scanning the hot path needs: locate the
// `messages` array value within the raw body, and split that array into its
// top-level element byte-spans. We do NOT json.Unmarshal the body (Bridge
// finding #5: marshalling a 5MB payload blows the <15ms budget). jsonparser is
// used only to read small scalar fields (role, content) out of already-bounded
// element bytes.

// locateArray finds the JSON array value for the given top-level key in body and
// returns the byte offsets of its opening '[' and closing ']' (inclusive), plus
// ok. It is a structural scan (string/escape aware) — it does not allocate. ok is
// false when the key is absent, its value is not an array, OR the array is
// unbalanced/truncated (no matching ']'), so the caller fails open verbatim
// rather than rebuilding+injecting onto degenerate input (P3).
func locateArray(body []byte, key string) (start, end int, ok bool) {
	vStart, vEnd, vt, vok := locateValue(body, key)
	if !vok || vt != valArray {
		return 0, 0, false
	}
	return vStart, vEnd, true
}

type valType int

const (
	valNone valType = iota
	valArray
	valObject
	valOther
)

// locateValue finds the value bytes for a top-level object key. Returns the
// start (first byte of the value) and end (last byte of the value, inclusive)
// offsets, the value's coarse type, and ok. ok is false when the key is absent
// OR its value is unbalanced/truncated (no matching close), so the caller can
// fail open verbatim (P3). Top-level only (depth-1 keys) — adequate for the
// OpenAI chat body whose `messages`/`model` are top-level.
func locateValue(body []byte, key string) (start, end int, vt valType, ok bool) {
	i := 0
	n := len(body)
	// Find the opening object brace.
	for i < n && body[i] != '{' {
		i++
	}
	if i >= n {
		return 0, 0, valNone, false
	}
	i++ // past '{'
	depth := 1
	for i < n {
		c := body[i]
		switch c {
		case '"':
			// Read a string (could be a key at depth 1).
			ks, ke, after := scanString(body, i)
			if depth == 1 && matchKey(body, ks, ke, key) {
				// Expect ':' then the value.
				j := skipWS(body, after)
				if j < n && body[j] == ':' {
					j = skipWS(body, j+1)
					return scanValue(body, j)
				}
			}
			i = after
			continue
		case '{', '[':
			depth++
			i++
		case '}', ']':
			depth--
			i++
		default:
			i++
		}
	}
	return 0, 0, valNone, false
}

// matchKey reports whether the raw string bytes body[ks:ke] (the content between
// the quotes, exclusive) equal key. Assumes no escapes in the key name (true for
// "messages"/"model").
func matchKey(body []byte, ks, ke int, key string) bool {
	if ke-ks != len(key) {
		return false
	}
	for i := 0; i < len(key); i++ {
		if body[ks+i] != key[i] {
			return false
		}
	}
	return true
}

// scanString scans a JSON string starting at the opening quote at i. Returns the
// content start (ks, just past the opening quote), content end (ke, the index of
// the closing quote), and after (the index just past the closing quote). Handles
// backslash escapes.
func scanString(body []byte, i int) (ks, ke, after int) {
	// body[i] == '"'
	ks = i + 1
	j := ks
	n := len(body)
	for j < n {
		c := body[j]
		if c == '\\' {
			j += 2
			continue
		}
		if c == '"' {
			return ks, j, j + 1
		}
		j++
	}
	return ks, n, n
}

// scanValue scans a JSON value starting at i (first non-ws byte) and returns its
// inclusive byte span, coarse type, and ok. ok is false on unbalanced/truncated
// container input (a '['/'{' with no matching close) so the caller fails open
// verbatim rather than acting on a degenerate span (P3). Scalars are always ok.
func scanValue(body []byte, i int) (start, end int, vt valType, ok bool) {
	n := len(body)
	if i >= n {
		return 0, 0, valNone, false
	}
	switch body[i] {
	case '[':
		e, bok := scanBalanced(body, i, '[', ']')
		return i, e, valArray, bok
	case '{':
		e, bok := scanBalanced(body, i, '{', '}')
		return i, e, valObject, bok
	case '"':
		_, _, after := scanString(body, i)
		return i, after - 1, valOther, true
	default:
		// number / true / false / null: read until a structural/ws terminator.
		j := i
		for j < n {
			c := body[j]
			if c == ',' || c == '}' || c == ']' || c == ' ' || c == '\t' || c == '\n' || c == '\r' {
				break
			}
			j++
		}
		return i, j - 1, valOther, true
	}
}

// scanBalanced returns the inclusive index of the matching close for the open
// bracket at i, string/escape aware, plus ok. ok is false when the input is
// unbalanced/truncated (the scanner reached EOF without the depth returning to
// zero) — signalling the caller to fail open verbatim (P3) instead of treating
// the last byte as a (false) close.
func scanBalanced(body []byte, i int, open, close byte) (end int, ok bool) {
	n := len(body)
	depth := 0
	j := i
	for j < n {
		c := body[j]
		switch c {
		case '"':
			_, _, after := scanString(body, j)
			j = after
			continue
		case open:
			depth++
			j++
		case close:
			depth--
			j++
			if depth == 0 {
				return j - 1, true
			}
		default:
			j++
		}
	}
	return n - 1, false
}

// splitArrayElements returns the inclusive byte-span [start,end] of each
// top-level element inside the array whose '[' is at arrStart and ']' at arrEnd.
// Whitespace between elements is excluded from spans. Empty array ⇒ nil spans.
// ok is false if any element value is itself unbalanced/truncated (so the caller
// fails open verbatim, P3); a well-formed array always returns ok=true.
func splitArrayElements(body []byte, arrStart, arrEnd int) (spans [][2]int, ok bool) {
	i := skipWS(body, arrStart+1) // past '['
	for i <= arrEnd-1 {
		if body[i] == ']' {
			break
		}
		s, e, vt, vok := scanValue(body, i)
		if !vok {
			return nil, false
		}
		// F-D forward-progress guard (belt-and-suspenders, defense in depth): a
		// degenerate value — empty/inverted span (e < s) or no value type at all
		// (valNone) — makes NO forward progress. Without this, `i = skipWS(body,
		// e+1)` can re-land on the same byte and the loop spins (CPU-DoS). Fail
		// open here so a hang can't return even if the bodyWellFormed gate that
		// catches `{"messages":[}]}` / `{"messages":[{...},}]}` upstream is ever
		// weakened. scanValue must always advance past at least one byte.
		if e < s || vt == valNone {
			return nil, false
		}
		spans = append(spans, [2]int{s, e})
		i = skipWS(body, e+1)
		if i <= arrEnd-1 && body[i] == ',' {
			i = skipWS(body, i+1)
		}
	}
	return spans, true
}

// bodyWellFormed reports whether body's leading value is a single balanced JSON
// object with only whitespace after it: i.e. the request envelope itself is not
// truncated and has no trailing garbage. locateValue/scanBalanced only validate
// the located *value* (e.g. the messages array); they cannot see that the
// ENCLOSING object is unterminated or that junk trails it (P3 — the
// `{"messages":[...]} GARBAGE` and truncated-envelope cases). The caller uses
// this to fail open verbatim on a malformed envelope. String/escape aware; no
// alloc.
func bodyWellFormed(body []byte) bool {
	i := skipWS(body, 0)
	if i >= len(body) || body[i] != '{' {
		return false
	}
	end, ok := scanBalanced(body, i, '{', '}')
	if !ok {
		return false // unterminated top-level object (truncated envelope)
	}
	// Only whitespace may follow the balanced object (no trailing garbage).
	return skipWS(body, end+1) >= len(body)
}

func skipWS(body []byte, i int) int {
	n := len(body)
	for i < n {
		switch body[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}
