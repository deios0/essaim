package extract

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"oikos/internal/rules"
)

// errCredentialBody is returned by writeDraft when a private-key marker survived
// redaction (P1-b): the body must NEVER be persisted. Process maps it to a clean
// "rejected" Result, not a draft.
var errCredentialBody = errors.New("oikos: credential body — refusing to write draft")

// M3 constants (spec §2.1).
const (
	OIKOS_T1_FLOOR    = 0.45           // classify_quality score floor to stage a T1 draft
	OIKOS_T1_PREF_MIN = 1              // M3-R5: ALSO require >=1 preference hit
	OIKOS_DRAFT_DIR   = rules.InboxDir // drafts (status:draft) — NEVER indexed, NEVER emitted; single source of truth
	OIKOS_ACTIVE_DIR  = "remembered"
)

// T1DedupWindow is how long an identical T1 correction is treated as a
// NON-INDEPENDENT repeat (a retry / duplicate send) and deduped (P2-3). A retry
// arrives within seconds; a genuine independent restatement of the same
// preference in a later conversation is far outside this window and counts as a
// fresh, promotable sighting. Chosen generously (well above any client
// retry/backoff horizon) while staying below a realistic gap between two
// independent conversations.
const T1DedupWindow = 2 * time.Minute

// sigilTokens are the exact, case-sensitive trigger tokens (BR-A0). A sigil is a
// user line whose TRIMMED text starts with one of these followed by whitespace
// and a non-empty payload.
var sigilTokens = []string{"/remember", "/rule", "@rule"}

// Exchange is the extractor's input: the user correction text (the newest user
// turn's message) paired with the prior assistant answer, plus the lossy flag
// (M3-R11) and the per-turn user lines for the sigil scan. Built off the hot
// path from the M2 Snapshot.CleanMessagesJSON (oikos-free, never the injected
// array) + the reassembled assistant text.
type Exchange struct {
	// UserText is the newest user turn's flattened message text (the T1 query +
	// the assistant pairing). Comes from CleanMessagesJSON, NEVER from the tee →
	// never lossy.
	UserText string
	// AssistantText is the reassembled assistant response (off-path SSE tee).
	AssistantText string
	// NewUserLines are the lines of the per-turn-delta user message(s) — the T0
	// sigil scan reads these (the NEWEST turn only, BR-A1).
	NewUserLines []string
	// Lossy reports the tee dropped bytes for this capture (AssistantText is
	// incomplete) → T2 distillation is refused (BR-A2-14), T1 still runs.
	Lossy bool
}

// Result reports what the extractor did with an exchange (for /health + tests).
type Result struct {
	Tier      string // "T0" | "T1" | "T2" | ""
	WrotePath string // the rule file written (empty if nothing)
	Status    string // "active" | "draft" | ""
	Skipped   string // a human reason when nothing was written
	// Hint is the quality classification of the correction that produced this
	// result ("validated" | "new" | "rejected" | ""). The lifecycle reinforce
	// reads it to gate a promote on the LATEST hint being >= new (RISK-4 /
	// BR-A1.5-2). A T0 sigil is a deliberate user /remember → treated as
	// "validated"; a T1/T2 draft carries classify_quality's hint.
	Hint string
}

// Extractor writes rules into a vault. It is the single off-path consumer of the
// capture queue. T2 is gated by Config (default OFF).
type Extractor struct {
	vault string
	cfg   Config
	now   func() time.Time
	// seen is the LRU-ish seen-set of sigil-line-hashes (BR-A1 idempotency
	// backstop), scoped to this extractor.
	seen *seenSet
	// t1seen is the TIME-WINDOWED dedup for T1 draft corrections (P2-3): it drops a
	// NON-INDEPENDENT repeat (a request retry / duplicate send of the identical
	// correction seen within a short window) so a retry does not re-report Status
	// "draft" and drive a spurious lifecycle reinforce (which could wrongly promote
	// the draft). Keyed on the correction identity (normalized title) + content.
	//
	// The window is the discriminator the spec's "INDEPENDENT reinforce"
	// (BR-A1.5-2) needs: a retry lands within seconds and is deduped, while the
	// SAME preference genuinely restated in a LATER conversation (minutes/hours on)
	// falls outside the window and counts as an independent sighting — so real
	// promotion is preserved. Uses the extractor's `now` seam so tests are
	// deterministic.
	t1seen *ttlSeen
}

// New constructs an Extractor over the given vault dir.
func New(vault string, cfg Config) *Extractor {
	cfg.ensureMeter()
	return &Extractor{vault: vault, cfg: cfg, now: time.Now, seen: newSeenSet(4096), t1seen: newTTLSeen(4096, T1DedupWindow)}
}

// SetNow is a test seam for deterministic dated paths.
func (e *Extractor) SetNow(fn func() time.Time) { e.now = fn }

// CostToday returns the extractor's T2 per-day spend (so the Learner/health can
// read the SAME meter the extractor writes).
func (e *Extractor) CostToday() float64 { return e.cfg.CostToday(e.now().UTC()) }

// CostCap returns the configured per-day cost cap.
func (e *Extractor) CostCap() float64 { return e.cfg.CostCapPerDay }

// TitleForReinforce derives the title an exchange would produce, so the
// lifecycle reinforce keys on the SAME title-hash the extractor writes. For a
// sigil it is the sigil payload's first line; otherwise the user text's first
// line.
func TitleForReinforce(ex Exchange) string {
	for _, line := range ex.NewUserLines {
		if payload, ok := parseSigil(line); ok {
			return titleFromPayload(payload)
		}
	}
	return titleFromPayload(ex.UserText)
}

// Process runs the layered T0 → T1 → T2 extraction on one exchange. It is the
// async consumer entry point. Returns what it did (for tests + /health).
func (e *Extractor) Process(ex Exchange) Result {
	// T0 — sigil (deterministic, primary, writes ACTIVE).
	if r := e.runT0(ex); r.WrotePath != "" || r.Status == "rejected" {
		return r
	}

	// T1 — heuristic (zero-token). Pairs the user correction with the assistant
	// answer for the quality score.
	q := classifyQuality(ex.UserText, ex.AssistantText)
	if q.Hint == "rejected" {
		return Result{Tier: "T1", Skipped: "rejected: " + strings.Join(q.Flags, ",")}
	}
	// BR-A7 / M3-R5 staging gate: score>=floor AND pref_hits>=1.
	if q.Score < OIKOS_T1_FLOOR || q.PrefHits < OIKOS_T1_PREF_MIN {
		return Result{Tier: "T1", Skipped: fmt.Sprintf("below gate (score=%.3f pref=%d)", q.Score, q.PrefHits)}
	}

	t1Path, terr := e.writeDraft(ex.UserText, q)
	if errors.Is(terr, errCredentialBody) {
		// P1-b: a residual private-key marker survived redaction → hard-reject,
		// write nothing (status reflects rejection, NOT a draft).
		return Result{Tier: "T1", Status: "rejected", Skipped: "credential body — dropped"}
	}
	if terr != nil {
		return Result{Tier: "T1", Skipped: "write error: " + terr.Error()}
	}
	// P2-3 T1 dedup: a NON-INDEPENDENT repeat of the identical correction (a
	// client retry / duplicate send within a short window) must not re-report
	// Status "draft" and drive a second, spurious lifecycle reinforce (which can
	// wrongly promote the draft). The T0 sigil scan already dedups by line-hash;
	// T1 had no such backstop, so a resend reinforced every time. Key on the
	// correction identity (normalized title) + content, and gate on a TIME WINDOW:
	// a retry lands within seconds → deduped; the same preference genuinely
	// restated in a LATER conversation is outside the window → an INDEPENDENT
	// sighting that legitimately reinforces (spec BR-A1.5-2). On a dedup, return a
	// clean no-op (WrotePath=="" and non-draft/active) — exactly the shape a
	// duplicate sigil returns — so the learn loop skips the reinforce. The draft
	// file still exists on disk (writeDraft is file-idempotent).
	t1key := t1DedupKey(ex.UserText)
	if e.t1seen.seenWithin(t1key, e.now()) {
		return Result{Tier: "T1", Skipped: "duplicate T1 correction (retry window)"}
	}
	res := Result{Tier: "T1", WrotePath: t1Path, Status: "draft", Hint: q.Hint}

	// T2 — cheap async LLM gate (opt-in, default OFF). Only when T1 cleared AND
	// allow_cloud AND not lossy.
	if e.cfg.AllowCloud && !ex.Lossy {
		if refined, ok := e.runT2(ex, t1Path); ok {
			res.Tier = "T2"
			res.WrotePath = refined
		}
	}
	return res
}

// runT0 scans the newest user turn's lines for sigils (BR-A0/A1). It handles
// EVERY sigil in the message independently (a user may write several /remember
// lines in one message — P1-learn): for each it applies the same seen-set
// idempotency, the same credential hard-zero (BR-A3), and writes ONE active rule
// per sigil to remembered/<date>/ (BR-A4 — one file per rule). The scan never
// short-circuits on the first match, so no later sigil is silently dropped.
//
// The single returned Result summarizes the message for Process + /health:
//   - if any sigil WROTE a rule → T0/active with the LAST written path and
//     hint=validated (a write short-circuits Process past T1/T2);
//   - else if any sigil was credential-REJECTED → the rejected Result (Process
//     also short-circuits on Status=="rejected", so T1 is skipped);
//   - else if sigils were present but all duplicates/no-ops → the skip Result
//     (WrotePath=="" and non-rejected → Process falls through to T1, exactly as
//     the single-duplicate-sigil case did before);
//   - else (no sigils at all) → the zero Result → Process runs T1.
func (e *Extractor) runT0(ex Exchange) Result {
	var (
		wrote    Result // last successful active write (highest priority)
		rejected Result // a credential rejection (2nd priority)
		skipped  Result // a duplicate / write-error no-op (lowest priority)
		sawSigil bool
	)
	for _, line := range ex.NewUserLines {
		payload, ok := parseSigil(line)
		if !ok {
			continue
		}
		sawSigil = true
		// Credential hard-zero (BR-A3) FIRST — before the seen-set (SECURITY, gemini
		// review). A credential-bearing sigil must be REJECTED on EVERY send,
		// including a resend: if it were added to the seen-set, a resend would hit
		// the duplicate-skip below, produce a "skipped" (not "rejected") result, and
		// Process would fall THROUGH to T1 with the raw credential-bearing text —
		// leaking the secret to the LLM. So we reject-and-continue here and NEVER add
		// a credential sigil to the seen-set (re-evaluate it as rejected every time).
		// The marker check is P1-b defense in depth (a partially-redacted private key
		// leaves an orphan BEGIN/END marker that must still drop the sigil).
		if credentialPattern.MatchString(payload) || pemMarkerPattern.MatchString(payload) {
			rejected = Result{Tier: "T0", Status: "rejected", Skipped: "credential in sigil — dropped"}
			continue
		}
		// Idempotency: a clean sigil line already processed (resent history, or a
		// repeat within THIS message) is skipped. Applied PER sigil.
		h := titleHash(line)
		if e.seen.has(h) {
			if skipped.Skipped == "" {
				skipped = Result{Tier: "T0", Skipped: "duplicate sigil (seen-set)"}
			}
			continue
		}
		e.seen.add(h)
		path, werr := e.writeActive(payload)
		if werr != nil {
			if skipped.Skipped == "" {
				skipped = Result{Tier: "T0", Skipped: "write error: " + werr.Error()}
			}
			continue
		}
		// A sigil is a deliberate user /remember → its quality hint is validated
		// (>= new), so a repeated sigil satisfies the promote hint-gate.
		wrote = Result{Tier: "T0", WrotePath: path, Status: "active", Hint: "validated"}
	}
	switch {
	case wrote.WrotePath != "":
		// At least one rule was written → active wins (Process skips T1/T2).
		return wrote
	case rejected.Status == "rejected":
		// No write, but a credential was dropped → report the rejection so Process
		// short-circuits and T1 never sees the credential-bearing text.
		return rejected
	case sawSigil:
		// Sigils were present but nothing was written or rejected (all duplicates /
		// write no-ops) → the skip Result. WrotePath=="" and non-rejected, so
		// Process falls through to T1 exactly as the single-duplicate case did.
		return skipped
	default:
		return Result{}
	}
}

// parseSigil parses a sigil line (BR-A0): trimmed text starting with an exact
// token + whitespace + a non-empty payload. Case-sensitive on the token; the
// token must be LINE-LEADING after trim. Returns (payload, ok).
func parseSigil(line string) (string, bool) {
	t := strings.TrimSpace(line)
	for _, tok := range sigilTokens {
		if !strings.HasPrefix(t, tok) {
			continue
		}
		rest := t[len(tok):]
		// Must be followed by whitespace (so "/rulebook" is not "/rule book").
		if rest == "" || !isSpace(rest[0]) {
			continue
		}
		payload := strings.TrimSpace(rest)
		if payload == "" {
			continue
		}
		return payload, true
	}
	return "", false
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n' || b == '\f' || b == '\v'
}

// titleFromPayload derives the rule title from a payload: the first ≤80 chars of
// the first line (for the title-hash dedup, §3.1).
func titleFromPayload(payload string) string {
	first := payload
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	first = strings.TrimSpace(first)
	if len(first) > 80 {
		// trim on a rune boundary
		r := []rune(first)
		if len(r) > 80 {
			first = string(r[:80])
		}
	}
	return first
}

var slugNonWord = regexp.MustCompile(`[^a-z0-9]+`)

// slug builds a stable, filesystem-safe slug from a title (lowercase, words
// joined by '-', capped). Empty → "rule".
func slug(title string) string {
	s := slugNonWord.ReplaceAllString(strings.ToLower(title), "-")
	s = strings.Trim(s, "-")
	if len(s) > 48 {
		s = s[:48]
		s = strings.Trim(s, "-")
	}
	if s == "" {
		return "rule"
	}
	return s
}

// idFor builds a content-stable id: slug + short title-hash (so the same title
// maps to the same file → reinforce-not-duplicate at the filesystem level too).
func idFor(title string) string {
	return slug(title) + "-" + titleHash(title)[:8]
}

// t1DedupKey is the seen-set key for a T1 draft correction (P2-3). It hashes the
// derived TITLE (normalized) joined with the FULL correction text, so a
// byte-identical resend dedups (same key → skip the reinforce) while a genuinely
// different correction — even one sharing the same title first line — keys
// differently and is treated as an independent sighting. A NUL separator makes
// the (title, text) pre-image unambiguous.
func t1DedupKey(userText string) string {
	title := titleFromPayload(userText)
	return titleHash(title + "\x00" + userText)
}

// writeActive writes ONE active rule for a sigil to remembered/<YYYY-MM-DD>/<id>.md
// (BR-A4). Frontmatter: status:active, confidence:0.8, weight:1, kind:preference,
// last_reinforced_at:now, criticality:"" (→ treated as 5). One rule per file.
func (e *Extractor) writeActive(payload string) (string, error) {
	title := titleFromPayload(payload)
	date := e.now().UTC().Format("2006-01-02")
	dir := filepath.Join(e.vault, OIKOS_ACTIVE_DIR, date)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	id := idFor(title)
	path := filepath.Join(dir, id+".md")
	doc := renderRuleDoc(ruleDoc{
		ID:               id,
		Title:            title,
		Kind:             "preference",
		Status:           "active",
		Confidence:       0.8,
		Weight:           1,
		HalfLife:         DefaultHalfLifeDays, // P2-2: decay can retire an unreinforced rule
		LastReinforcedAt: e.now().UTC().Format(time.RFC3339),
		Body:             payload,
	})
	if err := atomicWrite(path, []byte(doc)); err != nil {
		return "", err
	}
	return path, nil
}

// writeDraft writes a T1/T2 draft rule to _inbox/<id>.md (status:draft). Drafts
// are NEVER indexed nor emitted (M3-R1). The body is the user correction text;
// credentials are already hard-rejected upstream (classifyQuality), but we
// redact belt-and-suspenders before persisting.
func (e *Extractor) writeDraft(body string, q Quality) (string, error) {
	body = RedactCredentials(body)
	// P1-b belt: if a private-key BEGIN/END marker survived redaction (a partial
	// or malformed block), REFUSE to write — a local store never persists a key.
	if pemMarkerPattern.MatchString(body) {
		return "", errCredentialBody
	}
	title := titleFromPayload(body)
	// Create _inbox/ AND drop its .gitignore on first use so a git-tracked vault
	// never accidentally commits this unreviewed draft or the .counts.json sidecar
	// (review fix).
	dir, err := rules.EnsureInboxDir(e.vault)
	if err != nil {
		return "", err
	}
	id := idFor(title)
	path := filepath.Join(dir, id+".md")
	// Idempotency at the file level: if a draft for this title already exists, do
	// not clobber it (the lifecycle sweep reinforces it instead).
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	doc := renderRuleDoc(ruleDoc{
		ID:               id,
		Title:            title,
		Kind:             "preference",
		Status:           "draft",
		Confidence:       q.Score,
		Weight:           1,
		HalfLife:         DefaultHalfLifeDays, // P2-2: a wrongly-promoted draft must be able to decay
		LastReinforcedAt: e.now().UTC().Format(time.RFC3339),
		Body:             body,
	})
	if err := atomicWrite(path, []byte(doc)); err != nil {
		return "", err
	}
	return path, nil
}

// refineDraft replaces the BODY of an existing draft `.md` with the T2-distilled
// lesson, preserving its frontmatter (BR-A11 — T2 refines, never creates a
// second draft). The lesson is credential-redacted belt-and-suspenders.
func (e *Extractor) refineDraft(path, lesson string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	s := strings.ReplaceAll(string(raw), "\r\n", "\n")
	// Keep everything up to and including the closing frontmatter fence; replace
	// the body. The doc is "---\n<fm>\n---\n<body>"; find the SECOND "---" line.
	const fence = "---\n"
	if !strings.HasPrefix(s, fence) {
		// No frontmatter — just overwrite the body (defensive).
		return atomicWrite(path, []byte(RedactCredentials(lesson)+"\n"))
	}
	rest := s[len(fence):]
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return atomicWrite(path, []byte(RedactCredentials(lesson)+"\n"))
	}
	// header = up to and including the closing fence line.
	closeStart := len(fence) + idx + 1 // position of the closing "---"
	afterClose := closeStart + len("---")
	header := s[:afterClose]
	body := RedactCredentials(lesson)
	doc := header + "\n" + body
	if !strings.HasSuffix(doc, "\n") {
		doc += "\n"
	}
	return atomicWrite(path, []byte(doc))
}
