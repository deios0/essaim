package capture

import (
	"strings"
	"testing"

	"essaim/internal/rules"
)

// feed re-chunks s into the reassembler at the given byte boundaries to simulate
// adversarial TCP/read splits.
func feedChunks(s string, sizes []int) *StreamReassembler {
	r := NewStreamReassembler()
	i := 0
	for _, n := range sizes {
		end := i + n
		if end > len(s) {
			end = len(s)
		}
		r.Write([]byte(s[i:end]))
		i = end
	}
	if i < len(s) {
		r.Write([]byte(s[i:]))
	}
	return r
}

// Test 39: a recorded SSE stream ⇒ AssistantText == concat(delta.content),
// Partial==false (saw [DONE]).
func TestStreamCaptureAssemblesAssistantText(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\", world\"}}]}\n\n" +
		"data: [DONE]\n\n"
	r := NewStreamReassembler()
	r.Write([]byte(sse))
	if got := r.Text(); got != "Hello, world" {
		t.Fatalf("AssistantText = %q, want %q", got, "Hello, world")
	}
	if !r.SawDone() {
		t.Fatal("must observe [DONE] → Partial false")
	}
}

// Test 40 (BR-A2-5): re-chunked mid-data:, mid-JSON, mid-multibyte-Cyrillic ⇒
// AssistantText identical to a whole-frame parse, no corrupted runes.
func TestStreamCaptureAdversarialChunkBoundaries(t *testing.T) {
	// Cyrillic content split across chunk boundaries.
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"Привет\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\" мир\"}}]}\n\n" +
		"data: [DONE]\n\n"
	whole := NewStreamReassembler()
	whole.Write([]byte(sse))
	want := whole.Text()
	if want != "Привет мир" {
		t.Fatalf("baseline whole parse = %q", want)
	}
	// Re-chunk at 1-byte boundaries (splits multibyte runes + JSON + data lines).
	one := make([]int, len(sse))
	for i := range one {
		one[i] = 1
	}
	r := feedChunks(sse, one)
	if got := r.Text(); got != want {
		t.Fatalf("1-byte re-chunk = %q, want %q (corrupted across boundaries)", got, want)
	}
	// And an awkward 7-byte chunking.
	seven := make([]int, 0)
	for n := 0; n < len(sse); n += 7 {
		seven = append(seven, 7)
	}
	r2 := feedChunks(sse, seven)
	if got := r2.Text(); got != want {
		t.Fatalf("7-byte re-chunk = %q, want %q", got, want)
	}
}

// Test 41 (BR-A2-6): stream ends at upstream EOF with no [DONE] ⇒ Partial true
// with the text-so-far (the reassembler holds text; SawDone false signals it).
func TestOllamaEOFWithoutDoneCaptures(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"partial answer\"}}]}\n\n"
	r := NewStreamReassembler()
	r.Write([]byte(sse))
	// No [DONE] — upstream just EOFs. The caller marks Partial = !SawDone.
	if r.SawDone() {
		t.Fatal("no [DONE] was sent; SawDone must be false (→ Partial true)")
	}
	if r.Text() != "partial answer" {
		t.Fatalf("text-so-far = %q", r.Text())
	}
}

// Test 42: data: [DONE], data:[DONE], CRLF and LF ⇒ all yield SawDone (Partial
// false).
func TestDoneWithAndWithoutSpaceCRLF(t *testing.T) {
	for _, done := range []string{"data: [DONE]\n\n", "data:[DONE]\n\n", "data: [DONE]\r\n\r\n", "data:[DONE]\r\n"} {
		r := NewStreamReassembler()
		r.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"))
		r.Write([]byte(done))
		if !r.SawDone() {
			t.Fatalf("variant %q must be recognized as [DONE]", done)
		}
	}
}

// A `: comment` line and a non-data field are tolerated.
func TestStreamToleratesCommentAndFields(t *testing.T) {
	sse := ": ping\n" +
		"event: message\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: [DONE]\n\n"
	r := NewStreamReassembler()
	r.Write([]byte(sse))
	if r.Text() != "ok" {
		t.Fatalf("text = %q, want ok", r.Text())
	}
}

// CAP-2: a hostile multi-MB `data:` line with NO newline must NOT grow the raw
// buffer without bound. The over-long line is dropped (bounded memory, no panic),
// the reassembler flags Overflowed, and after the next '\n' it resyncs and still
// captures a following legitimate line. The client stream is untouched by this
// (it is the OFF-PATH reassembler); this test guards the off-path OOM only.
func TestStreamReassemblerBoundsHostileNoNewlineLine(t *testing.T) {
	r := NewStreamReassembler()
	// A 5 MiB `data:` line prefix with NO newline, fed in many chunks.
	hostile := "data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("A", 5<<20)
	const chunk = 64 << 10
	for i := 0; i < len(hostile); i += chunk {
		end := i + chunk
		if end > len(hostile) {
			end = len(hostile)
		}
		r.Write([]byte(hostile[i:end])) // must never panic, never OOM
	}
	// The raw buffer must be bounded (<= maxRawLine + one chunk of slack), NOT 5MB.
	if got := r.buf.Len(); got > maxRawLine {
		t.Fatalf("raw buffer must stay bounded by ~maxRawLine; got %d bytes (cap %d)", got, maxRawLine)
	}
	if !r.Overflowed() {
		t.Fatal("an over-long no-newline line must set Overflowed (lossy)")
	}
	// Resync: a newline ends the hostile line; a following legit data line is
	// captured normally.
	r.Write([]byte("\ndata: {\"choices\":[{\"delta\":{\"content\":\"recovered\"}}]}\n\ndata: [DONE]\n\n"))
	if got := r.Text(); !strings.Contains(got, "recovered") {
		t.Fatalf("after dropping the hostile line the reassembler must resync and capture the next line; text=%q", got)
	}
	if !r.SawDone() {
		t.Fatal("the [DONE] after resync must be seen")
	}
}

// CAP-2 (complete-giant-line vector): a multi-MB `data:` line that DID end in a
// newline is dropped without the expensive parse (overflow), and the next line
// is still captured. Bounded processing, no panic, client stream unaffected.
func TestStreamReassemblerBoundsCompleteGiantLine(t *testing.T) {
	r := NewStreamReassembler()
	hostile := "data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("A", 2<<20) + "\"}}]}\n"
	good := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"
	r.Write([]byte(hostile + good)) // must never panic
	if !r.Overflowed() {
		t.Fatal("a complete multi-MB data line must set Overflowed (dropped, not parsed)")
	}
	if got := r.Text(); !strings.Contains(got, "ok") {
		t.Fatalf("a following legit line must still be captured; text=%q", got)
	}
	if !r.SawDone() {
		t.Fatal("the [DONE] after the giant line must be seen")
	}
}

// A malformed data line is skipped; valid lines still produce text (BR-A2-8).
func TestStreamSkipsMalformedLine(t *testing.T) {
	sse := "data: {not json}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"good\"}}]}\n\n" +
		"data: [DONE]\n\n"
	r := NewStreamReassembler()
	r.Write([]byte(sse))
	if r.Text() != "good" {
		t.Fatalf("text = %q, want good", r.Text())
	}
}

// Cap is enforced (BR-A2-7): a huge stream stops at the cap and sets CapHit.
func TestStreamCapEnforced(t *testing.T) {
	big := strings.Repeat("a", maxAssistantText+1000)
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"" + big + "\"}}]}\n\n"
	r := NewStreamReassembler()
	r.Write([]byte(sse))
	if !r.CapHit() {
		t.Fatal("cap must be hit for an oversized stream")
	}
	if len(r.Text()) != maxAssistantText {
		t.Fatalf("text len = %d, want exactly the cap %d", len(r.Text()), maxAssistantText)
	}
}

// Test 43: non-stream content "hello" ⇒ AssistantTextFromNonStream == "hello".
func TestNonStreamCapturePath(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`)
	if got := AssistantTextFromNonStream(body); got != "hello" {
		t.Fatalf("non-stream content = %q, want hello", got)
	}
}

// Test 44: non-stream tool-call-only with null content ⇒ "" (still capturable).
func TestNonStreamToolCallOnlyCapture(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1"}]}}]}`)
	if got := AssistantTextFromNonStream(body); got != "" {
		t.Fatalf("tool-call-only content must be empty, got %q", got)
	}
}

// Test 36: ParseCleanMessages over an essaim-free snapshot yields the user
// messages and NO essaim sentinels (the capture input is the clean snapshot).
func TestCaptureUsesCleanSnapshotNotInjected(t *testing.T) {
	clean := []byte(`[{"role":"user","content":"what database should I use?"}]`)
	msgs := ParseCleanMessages(clean)
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("parsed messages = %+v", msgs)
	}
	c := Capture{OriginalMessages: msgs}
	if strings.Contains(c.lastUserContent(), rules.ESSAIM_BEGIN) {
		t.Fatal("clean snapshot must carry no essaim block")
	}
	if c.lastUserContent() != "what database should I use?" {
		t.Fatalf("last user = %q", c.lastUserContent())
	}
}

// Test 50: a captured body with a COMPLETE essaim block trips the hard invariant;
// a lone (truncated) sentinel does NOT.
func TestHardInvariantRejectsCompleteBlockAllowsPartial(t *testing.T) {
	full := rules.WrapBlock("- [H] Use Postgres: always")
	c := Capture{OriginalMessages: []ChatMessage{{Role: "user", Content: "see: " + full}}}
	if !c.ViolatesHardInvariant() {
		t.Fatal("a complete essaim block must trip the hard invariant")
	}
	// A lone BEGIN sentinel (truncated) must NOT trip it.
	c2 := Capture{OriginalMessages: []ChatMessage{{Role: "user", Content: "stray " + rules.ESSAIM_BEGIN + " no end"}}}
	if c2.ViolatesHardInvariant() {
		t.Fatal("a lone/partial sentinel must NOT trip the hard invariant")
	}
}

// Test 47 (F-8): a key inside a message content is redacted to [REDACTED] while
// surrounding prose stays; OriginalMessages still flatten/parse fine.
func TestRedactionPreservesSurroundingJSONAndProse(t *testing.T) {
	c := Capture{OriginalMessages: []ChatMessage{
		{Role: "user", Content: "call https://api.example.com with api_key=sk-abcdefghijklmnop1234567890 please"},
	}}
	c.Redact()
	got := c.OriginalMessages[0].Content
	if !strings.Contains(got, "https://api.example.com") || !strings.Contains(got, "please") {
		t.Fatalf("surrounding prose/URL must survive: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") || strings.Contains(got, "sk-abcdefghijklmnop1234567890") {
		t.Fatalf("the key must be redacted: %q", got)
	}
}

// ToExchange wires the newest user message into the sigil-scan lines + T1 query.
func TestToExchangeBuildsSigilLines(t *testing.T) {
	c := Capture{OriginalMessages: []ChatMessage{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "answer"},
		{Role: "user", Content: "/remember prefer tabs\nand a second line"},
	}, AssistantText: "ok"}
	ex := c.ToExchange()
	if ex.UserText != "/remember prefer tabs\nand a second line" {
		t.Fatalf("UserText = %q", ex.UserText)
	}
	if len(ex.NewUserLines) != 2 || ex.NewUserLines[0] != "/remember prefer tabs" {
		t.Fatalf("NewUserLines = %v", ex.NewUserLines)
	}
}
