// Package capture implements the M3 response-side capture tap: an SSE/non-stream
// reassembler that runs OFF the client response path (locked invariant 2 — the
// capture tap is observation-only and never delays, backpressures, or alters the
// verbatim client stream). It consumes the M2 pre-injection Snapshot
// (CleanMessagesJSON, oikos-free) plus the reassembled assistant text to form an
// exchange handed to the async extractor.
package capture

import (
	"strings"

	"github.com/buger/jsonparser"
)

// maxAssistantText caps the reassembled assistant_text (BR-A2-7). On cap, the
// capture is marked Partial and appending stops.
const maxAssistantText = 256 * 1024

// maxRawLine caps the RAW byte accumulator (s.buf) — the partial-line buffer
// (CAP-2). A hostile/buggy upstream that streams a multi-MB `data:` line with NO
// '\n' would otherwise grow s.buf unbounded (an off-path OOM / DoS). When the
// buffer exceeds this with no newline, the in-progress line is DROPPED (the
// reassembler marks itself overflowed + lossy and resyncs on the next '\n'). It
// must be >= maxAssistantText so a legitimate single large content line is still
// captured up to the assistant-text cap before this hard guard fires.
const maxRawLine = 1 << 20 // 1 MiB

// Capture is the payload handed to the async learning queue (spec §4.1). It is
// built OFF the hot path from the (oikos-free) clean snapshot + the reassembled
// assistant text — never the injected array, never raw bytes carrying a prior
// block (M3-R3).
type Capture struct {
	OriginalMessages []ChatMessage // parsed off-path from Snapshot.CleanMessagesJSON
	AssistantText    string        // stream: concat(delta.content); non-stream: choices[0].message.content
	MatchedRuleIDs   []string      // from Snapshot.MatchedRuleIDs (out-of-band, NEVER from the prompt)
	Model            string
	Stream           bool
	Partial          bool // client disconnect / upstream EOF before [DONE], or cap hit
	Lossy            bool // tee dropped bytes for THIS capture → assistant_text incomplete (M3-R11)
	RequestHash      string

	// credentialDropped is set by Redact when ANY message body or the assistant
	// text carried a private-key BEGIN/END marker (P1-b). Redact strips the key
	// in place, but a key-bearing exchange must be WHOLE-MESSAGE-DROPPED — never
	// learned from — so ViolatesHardInvariant honors this flag. It is read AFTER
	// Redact (the server tap already runs Redact → ViolatesHardInvariant → drop).
	credentialDropped bool
}

// ChatMessage is a minimal flattened view of one captured message.
type ChatMessage struct {
	Role    string
	Content string
}

// StreamReassembler reassembles an OpenAI SSE chat-completions stream into the
// assistant text, tolerant of: chunk/TCP boundaries mid-`data:` and mid-JSON
// (BR-A2-4), multibyte UTF-8 split across reads (BR-A2-5 — we buffer RAW bytes
// and only decode at a complete data line), Ollama's missing `[DONE]` (treat
// upstream EOF as end-of-stream, BR-A2-6), `data: [DONE]` with/without a space
// and CRLF/LF (BR-A2-6), and malformed JSON lines (skipped, BR-A2-8).
type StreamReassembler struct {
	buf      strings.Builder // raw byte accumulator (holds partial lines/runes)
	text     strings.Builder // decoded assistant text
	done     bool            // saw an explicit [DONE]
	cap      bool            // hit the assistant_text cap
	overflow bool            // a single line exceeded maxRawLine → dropped + resyncing (CAP-2)
	dropping bool            // currently discarding an over-long line until the next '\n'
}

// NewStreamReassembler constructs a streaming reassembler.
func NewStreamReassembler() *StreamReassembler { return &StreamReassembler{} }

// Write feeds teed upstream bytes. It NEVER errors (best-effort, off-path) and
// NEVER blocks. It buffers raw bytes and processes only complete lines, so a
// multibyte rune or a JSON object split across Write calls is reassembled
// before decode.
func (s *StreamReassembler) Write(p []byte) {
	s.buf.WriteString(string(p))
	s.drainLines()
}

// drainLines processes every COMPLETE line (terminated by '\n') currently in the
// buffer, leaving any trailing partial line buffered for the next Write. The
// trailing partial line is BOUNDED by maxRawLine (CAP-2): an over-long line with
// no '\n' is dropped and the reassembler resyncs on the next newline, so a
// hostile no-newline stream cannot grow s.buf without bound (off-path OOM guard).
func (s *StreamReassembler) drainLines() {
	cur := s.buf.String()
	for {
		nl := strings.IndexByte(cur, '\n')
		if nl < 0 {
			break // partial line — keep it buffered (subject to the bound below)
		}
		line := cur[:nl]
		cur = cur[nl+1:]
		if s.dropping {
			// We were discarding an over-long line; the '\n' ends it → resync.
			s.dropping = false
			continue
		}
		s.handleLine(line)
	}
	// Bound the trailing partial line. If it (alone) exceeds the cap with no '\n',
	// drop it: mark overflow+lossy, enter dropping mode, and discard until the
	// next newline. This caps s.buf at ~maxRawLine regardless of upstream behavior.
	if s.dropping {
		// Still mid-drop: never retain the discarded tail.
		s.buf.Reset()
		return
	}
	if len(cur) > maxRawLine {
		s.overflow = true
		s.dropping = true
		s.buf.Reset() // drop the over-long partial line; resync on the next '\n'
		return
	}
	s.buf.Reset()
	s.buf.WriteString(cur)
}

// handleLine processes one complete SSE line.
func (s *StreamReassembler) handleLine(line string) {
	// CAP-2 defense-in-depth: a COMPLETE line that itself exceeds maxRawLine (a
	// multi-MB `data:` payload that DID end in '\n') is dropped without the
	// expensive jsonparser parse — mark overflow+lossy and skip it. The off-path
	// capture for this exchange is incomplete, but bounded; the client stream is
	// unaffected (this runs off the relay path).
	if len(line) > maxRawLine {
		s.overflow = true
		return
	}
	line = strings.TrimRight(line, "\r") // tolerate CRLF
	if line == "" {
		return // event terminator / blank line
	}
	if strings.HasPrefix(line, ":") {
		return // SSE comment line
	}
	if !strings.HasPrefix(line, "data:") {
		return // non-data field (event:, id:, …) — ignore
	}
	payload := strings.TrimSpace(line[len("data:"):])
	if payload == "[DONE]" {
		s.done = true
		return
	}
	if s.cap {
		return
	}
	// Decode choices[].delta.content out of the JSON object (best-effort; a
	// malformed object is skipped, BR-A2-8).
	_, _ = jsonparser.ArrayEach([]byte(payload), func(choice []byte, _ jsonparser.ValueType, _ int, _ error) {
		if content, err := jsonparser.GetString(choice, "delta", "content"); err == nil {
			s.appendText(content)
		}
	}, "choices")
}

func (s *StreamReassembler) appendText(content string) {
	if s.cap {
		return
	}
	if s.text.Len()+len(content) > maxAssistantText {
		// Append up to the cap, then stop (Partial).
		remain := maxAssistantText - s.text.Len()
		if remain > 0 {
			s.text.WriteString(content[:remain])
		}
		s.cap = true
		return
	}
	s.text.WriteString(content)
}

// Text returns the assistant text reassembled so far.
func (s *StreamReassembler) Text() string { return s.text.String() }

// SawDone reports whether an explicit `data: [DONE]` was observed.
func (s *StreamReassembler) SawDone() bool { return s.done }

// CapHit reports whether the assistant_text cap was reached.
func (s *StreamReassembler) CapHit() bool { return s.cap }

// Overflowed reports whether an over-long no-newline line was dropped to keep the
// raw buffer bounded (CAP-2). It implies the assistant_text is incomplete → the
// capture is lossy/partial.
func (s *StreamReassembler) Overflowed() bool { return s.overflow }

// AssistantTextFromNonStream extracts choices[0].message.content from a complete
// non-streaming chat-completions JSON body (BR-A2-9). Returns "" (with ok=true)
// for a tool-call-only turn with null content.
func AssistantTextFromNonStream(body []byte) string {
	var out string
	_, _ = jsonparser.ArrayEach(body, func(choice []byte, _ jsonparser.ValueType, _ int, _ error) {
		if out != "" {
			return
		}
		if content, err := jsonparser.GetString(choice, "message", "content"); err == nil {
			out = content
		}
	}, "choices")
	return out
}

// ParseCleanMessages parses the oikos-free CleanMessagesJSON (the messages array
// raw JSON) into flattened ChatMessages, OFF the hot path (M3-R3). It flattens
// string content and multimodal text parts; non-flattenable content yields an
// empty Content (never an oikos block recognizer trip — a pure-image message
// can't carry the sentinels).
func ParseCleanMessages(arrJSON []byte) []ChatMessage {
	var out []ChatMessage
	_, _ = jsonparser.ArrayEach(arrJSON, func(msg []byte, dt jsonparser.ValueType, _ int, _ error) {
		if dt != jsonparser.Object {
			return
		}
		role, _ := jsonparser.GetString(msg, "role")
		out = append(out, ChatMessage{Role: role, Content: flattenContent(msg)})
	})
	return out
}

// flattenContent extracts a message's text content (string, or concatenated
// text parts). Mirrors inject.flattenContent semantics but returns "" rather
// than an ok flag (an empty string is never an oikos block).
func flattenContent(msg []byte) string {
	v, typ, _, err := jsonparser.Get(msg, "content")
	if err != nil {
		return ""
	}
	switch typ {
	case jsonparser.String:
		s, err := jsonparser.ParseString(v)
		if err != nil {
			return ""
		}
		return s
	case jsonparser.Array:
		var sb strings.Builder
		_, _ = jsonparser.ArrayEach(v, func(part []byte, pdt jsonparser.ValueType, _ int, _ error) {
			if pdt != jsonparser.Object {
				return
			}
			if pt, _ := jsonparser.GetString(part, "type"); pt == "text" {
				if txt, err := jsonparser.GetString(part, "text"); err == nil {
					sb.WriteString(txt)
				}
			}
		})
		return sb.String()
	default:
		return ""
	}
}
