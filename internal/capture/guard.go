package capture

import (
	"strings"

	"oikos/internal/extract"
	"oikos/internal/rules"
)

// ContainsCompleteOikosBlock reports whether s contains a COMPLETE oikos block
// (BOTH sentinels, BEGIN before END). The HARD INVARIANT (§4.1) rejects a
// capture whose any persisted body or assistant text carries a complete block —
// the B2 echo-poison defense. A lone/partial sentinel does NOT trip it (M2 A-6):
// both sentinels, in order, are required.
func ContainsCompleteOikosBlock(s string) bool {
	bi := strings.Index(s, rules.OIKOS_BEGIN)
	if bi < 0 {
		return false
	}
	ei := strings.Index(s[bi+len(rules.OIKOS_BEGIN):], rules.OIKOS_END)
	return ei >= 0
}

// Redact runs the shared credentialPattern over each FLATTENED message content
// and over the assistant text (§4.7 / M3-R3), replacing credential spans with
// [REDACTED]. It operates on the already-flattened strings, NEVER on raw JSON,
// so surrounding structure is preserved. Mutates the Capture in place.
func (c *Capture) Redact() {
	for i := range c.OriginalMessages {
		// P1-b: a private-key BEGIN/END marker in ANY body means this exchange
		// carried a secret. Note it BEFORE redaction (which removes the marker) so
		// ViolatesHardInvariant can whole-message-drop the capture — Redact alone
		// scrubs the key in place but a key-bearing exchange must never be learned.
		if extract.ContainsPrivateKeyMarker(c.OriginalMessages[i].Content) {
			c.credentialDropped = true
		}
		c.OriginalMessages[i].Content = extract.RedactCredentials(c.OriginalMessages[i].Content)
	}
	if extract.ContainsPrivateKeyMarker(c.AssistantText) {
		c.credentialDropped = true
	}
	c.AssistantText = extract.RedactCredentials(c.AssistantText)
}

// ViolatesHardInvariant reports whether the capture must be DROPPED (not
// enqueued): any original message body OR the assistant text contains a complete
// oikos block. Run AFTER Redact, BEFORE enqueue.
func (c *Capture) ViolatesHardInvariant() bool {
	// P1-b: a private-key-bearing exchange (flagged by Redact) is whole-message-
	// dropped — never enqueued, never learned from.
	if c.credentialDropped {
		return true
	}
	for _, m := range c.OriginalMessages {
		if ContainsCompleteOikosBlock(m.Content) {
			return true
		}
	}
	return ContainsCompleteOikosBlock(c.AssistantText)
}

// ToExchange builds the extractor Exchange from a finished capture: the newest
// user turn's text (the last user message) as the T1 query + sigil source, the
// assistant text, and the lossy flag. The sigil scan reads the newest user
// message's lines (BR-A1 per-turn newest-turn scan; the seen-set backstops
// resends).
func (c *Capture) ToExchange() extract.Exchange {
	user := c.lastUserContent()
	return extract.Exchange{
		UserText:      user,
		AssistantText: c.AssistantText,
		NewUserLines:  strings.Split(user, "\n"),
		Lossy:         c.Lossy,
	}
}

// lastUserContent returns the flattened content of the LAST role:"user" message
// (the newest turn's correction text).
func (c *Capture) lastUserContent() string {
	for i := len(c.OriginalMessages) - 1; i >= 0; i-- {
		if c.OriginalMessages[i].Role == "user" {
			return c.OriginalMessages[i].Content
		}
	}
	return ""
}
