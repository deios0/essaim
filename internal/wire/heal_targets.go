package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"essaim/internal/config"
	"essaim/internal/heal"
)

// proxyV1URL is the base_url an OpenAI-compatible IDE config must hold to route
// through essaim (the /v1 suffix is what OpenAI clients expect). It is the value
// essaim writes — i.e. every base_url heal target's LastWritten.
const proxyV1URL = ProxyBaseURL + "/v1"

// The JSON keys an OpenAI-compatible IDE uses to hold a custom API base
// (Continue's "apiBase", generic clients' "base_url"/"baseURL"/"openai_base_url")
// are recognized directly by baseURLPairRe below — that regex is the single
// source of truth for the managed-key set.

// vendorDefaultHosts are the upstream API hosts an IDE update RESETS a base_url to
// when it factory-restores its provider config (the clobber the heal watcher
// undoes). A managed base_url found pointing at one of these — having previously
// held the essaim proxy — is an IDE reset, NOT a deliberate user choice, so it is
// healed. Crucially: a base_url pointing at ANY OTHER host (e.g. a user's own
// gateway / a self-hosted proxy) is a deliberate override and is LEFT ALONE.
//
// Matching is by host substring so the path/suffix ("/v1", "/api", trailing
// slash) an IDE writes does not matter.
var vendorDefaultHosts = []string{
	"api.openai.com",
	"api.anthropic.com",
	"openrouter.ai",
	"api.mistral.ai",
	"api.groq.com",
	"generativelanguage.googleapis.com",
}

// HealSink is called on each SUCCESSFUL heal (P1-BUG-3) with the tool name and the
// config path essaim just re-pointed at the proxy. It is the mirror of the heal
// watcher's onError sink: `serve` wires it to stderr so a heal is VISIBLE — the
// user who deliberately reverted their IDE to the vendor default is TOLD essaim
// re-applied the proxy URL and HOW to stop it (`essaim unwire <tool>`), instead of
// the watcher silently fighting them every ~750ms. A nil sink is a no-op.
type HealSink func(tool, path string)

// HealNotice is the exact, honest line `serve` should surface when essaim re-points
// a tool at the proxy (P1-BUG-3). It names the tool, the config path, and the
// escape hatch so the message is actionable — the user reverted to the vendor
// default on purpose and must learn WHY essaim keeps changing it back and HOW to
// stop. One source of truth so the test and the serve-side wiring agree.
func HealNotice(tool, path string) string {
	return fmt.Sprintf("essaim: re-pointed %s at the proxy (%s); to stop, run `essaim unwire %s`", tool, path, tool)
}

// HealTargets builds the self-heal targets for the wired base_url tools whose IDE
// config file we know how to repair. It is the standard constructor with NO
// on-heal notification (nil sink) — see HealTargetsWithSink for the P1-BUG-3
// variant that reports each heal. Native-file tools (Claude Code) are excluded —
// they have no base_url to keep alive. A tool whose config file does not exist is
// skipped (we only watch/heal files that are actually present).
//
// pathOverrides lets the caller (and tests) supply a tool→config-file path,
// overriding the platform default location. A nil/absent entry falls back to the
// known default for that tool.
func HealTargets(c config.Config, pathOverrides map[string]string) []heal.Target {
	return HealTargetsWithSink(c, pathOverrides, nil)
}

// HealTargetsWithSink is HealTargets plus an onHeal notification (P1-BUG-3). Every
// target's Repair closure calls onHeal(tool, path) whenever it actually heals
// (changed == true) — never on a healthy config or a deliberate user override, so
// the user is notified ONLY when essaim changed something under them. `serve` passes
// a sink that writes HealNotice to stderr; tests pass a recording sink. A nil sink
// makes this identical to HealTargets.
//
// Every target carries LastWritten = proxyV1URL — the exact value essaim writes — so
// the healer only ever re-applies over essaim's OWN clobbered value and never a
// value a user deliberately set.
func HealTargetsWithSink(c config.Config, pathOverrides map[string]string, onHeal HealSink) []heal.Target {
	var out []heal.Target
	for _, wt := range c.WiredTools {
		if wt.Channel != ChannelBaseURL {
			continue // native-file tools have no base_url to heal
		}
		path := pathOverrides[wt.Name]
		if path == "" {
			path = defaultConfigPath(wt.Name)
		}
		if path == "" {
			continue // we don't know where this tool's config lives — skip
		}
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			continue // no config file present — nothing to watch/heal
		}
		out = append(out, heal.Target{
			Tool:        wt.Name,
			Path:        path,
			ExpectedURL: proxyV1URL,
			LastWritten: proxyV1URL,
			// Bind the healer to THIS target's LastWritten — the exact value essaim
			// wrote — so "already healthy" is judged against essaim's own recorded
			// value, not a global constant. (Today every base_url target's
			// LastWritten is proxyV1URL, but threading it keeps the wire-record
			// load-bearing and correct if a target ever records a different one.)
			// notifyOnHeal wraps the pure repair with the onHeal side-effect so a
			// heal is never silent (P1-BUG-3).
			Repair: notifyOnHeal(repairBaseURLFor(proxyV1URL), wt.Name, path, onHeal),
		})
	}
	return out
}

// notifyOnHeal decorates a pure Repair func so that whenever it reports a heal
// (changed == true, no error) it calls onHeal(tool, path) — the P1-BUG-3 seam that
// makes a heal VISIBLE instead of the watcher silently fighting a user who reverted
// to the vendor default. It is a pure decorator: the returned bytes/changed/err are
// exactly the inner func's, and a nil sink is a no-op (so HealTargets is byte-for-
// byte the prior behavior). The notify fires only on an actual change, never on a
// healthy config or a deliberate user override (both yield changed == false).
func notifyOnHeal(inner func([]byte) ([]byte, bool, error), tool, path string, onHeal HealSink) func([]byte) ([]byte, bool, error) {
	return func(cur []byte) ([]byte, bool, error) {
		out, changed, err := inner(cur)
		if changed && err == nil && onHeal != nil {
			onHeal(tool, path)
		}
		return out, changed, err
	}
}

// baseURLPairRe matches a managed base_url key→string-value pair in a JSON/JSONC
// config: `"apiBase"` (or another managed key), the colon, and the double-quoted
// value. Capture group 1 is the key name; group 2 is the value's INNER text (no
// quotes). It tolerates arbitrary whitespace around the colon and inside the
// JSON. We rewrite ONLY group 2's bytes in place, so key order, indentation,
// trailing commas, and // or /* */ comments elsewhere are all preserved.
var baseURLPairRe = regexp.MustCompile(`"(apiBase|base_url|baseURL|openai_base_url)"\s*:\s*"((?:[^"\\]|\\.)*)"`)

// repairBaseURLFor builds the surgical healer for a target whose essaim-written
// value is lastWritten (the wire-record — the exact base_url essaim put in the
// file). The returned func is what heal.Target.Repair calls. Binding lastWritten
// makes the "already healthy / ours" check judge against essaim's OWN recorded
// value rather than a global constant.
//
// The healer surgically re-points essaim's OWN clobbered base_url back at the
// proxy, preserving the rest of the file byte-for-byte (P1). It NEVER rewrites
// the whole config (no unmarshal→MarshalIndent — that lost key order, formatting
// and comments). It returns (healed, changed, err):
//
//   - changed == false: every managed key is either already on the essaim proxy
//     (healthy) OR holds a value the user DELIBERATELY set (a non-essaim,
//     non-vendor-default URL — a user override). Left byte-untouched.
//   - changed == true: at least one managed key held a VENDOR DEFAULT (an IDE
//     factory-reset of essaim's value) or an EMPTY value (essaim's value dropped);
//     those — and only those — are surgically replaced with the proxy URL.
//   - err != nil: the file is not parseable JSON/JSONC. We surface it (the
//     watcher reports it at startup) rather than silently no-op'ing — an
//     un-healable config the user thinks essaim is guarding must be visible.
//
// repairBaseURL is the default healer (lastWritten = the essaim proxy url every
// base_url target records today). It is the convenience entry for the standard
// case; repairBaseURLFor binds a specific recorded value.
func repairBaseURL(cur []byte) ([]byte, bool, error) {
	return repairBaseURLFor(proxyV1URL)(cur)
}

// Pure, in-process, local file content transform — no network.
func repairBaseURLFor(lastWritten string) func([]byte) ([]byte, bool, error) {
	return func(cur []byte) ([]byte, bool, error) {
		// Scan once: validate (comment-tolerant) AND learn where the comments are. A
		// genuinely broken config surfaces an error (it must be VISIBLE, not silently
		// skipped). A config with no managed key still parses — that's fine, it just
		// yields no change.
		comments, err := scanJSONC(cur)
		if err != nil {
			return cur, false, fmt.Errorf("essaim heal: config is not valid JSON/JSONC, cannot safely heal: %w", err)
		}

		changed := false
		// Find managed key→value pairs over the ORIGINAL bytes (so the rewrite
		// preserves formatting/comments), but SKIP any match that begins inside a
		// comment — a `"apiBase":"api.openai.com"` written in a // comment is not a
		// real key and must never be rewritten. Replace right-to-left so earlier byte
		// offsets stay valid as we splice.
		locs := baseURLPairRe.FindAllSubmatchIndex(cur, -1)
		healed := cur
		for i := len(locs) - 1; i >= 0; i-- {
			m := locs[i]
			matchStart := m[0]
			if inAnySpan(comments, matchStart) {
				continue // the pair lives inside a comment — not a managed key
			}
			valStart, valEnd := m[4], m[5] // capture group 2: the value's inner text
			// DECODE the JSON string before classifying it: a valid config may escape
			// the slashes (`https:\/\/api.openai.com\/v1`), and the host check must see
			// the real URL, not the escaped bytes. We still splice the ORIGINAL byte
			// range, so formatting is preserved regardless of how it was encoded.
			value := decodeJSONString(cur[valStart:valEnd])
			if !shouldHealValue(value, lastWritten) {
				continue // healthy, or a deliberate user override — leave it byte-exact
			}
			// Surgically replace ONLY the value's inner bytes; the key, colon, exact
			// whitespace and quotes are untouched. lastWritten needs no JSON escaping.
			out := make([]byte, 0, len(healed)+len(lastWritten))
			out = append(out, healed[:valStart]...)
			out = append(out, lastWritten...)
			out = append(out, healed[valEnd:]...)
			healed = out
			changed = true
		}
		if !changed {
			return cur, false, nil
		}
		return healed, true, nil
	}
}

// shouldHealValue decides, for a managed base_url's CURRENT value, whether essaim
// should re-apply its own recorded value (lastWritten). This is the heart of
// "don't fight the user":
//
//   - already-ours   → false (healthy; the value essaim wrote is still there).
//   - empty          → true  (essaim's value was dropped — re-apply).
//   - vendor default → true  (an IDE factory-reset of essaim's value — re-apply).
//   - anything else  → false (a URL the user deliberately set — a user override
//     we must NEVER stomp, e.g. their own gateway).
func shouldHealValue(value, lastWritten string) bool {
	if value == lastWritten || strings.Contains(value, ProxyBaseURL) {
		return false // still essaim's own value — healthy, no churn
	}
	if strings.TrimSpace(value) == "" {
		return true // dropped/blanked — essaim's value is gone
	}
	return isVendorDefault(value)
}

// isVendorDefault reports whether value's URL HOST is a known upstream API host
// an IDE resets a provider base_url to. It matches on the parsed host — exactly
// the vendor host or a subdomain of it — NOT a raw substring, so a deliberate
// look-alike a user set (e.g. https://api.openai.com.evil/v1, or a self-hosted
// https://proxy.internal/api.openai.com/v1) is NOT mistaken for a vendor default
// and is left alone.
func isVendorDefault(value string) bool {
	host := urlHost(value)
	if host == "" {
		return false // not a URL with a host we can trust — treat as a user value
	}
	for _, h := range vendorDefaultHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// decodeJSONString turns the INNER bytes of a JSON string (the text between the
// quotes, group 2 of baseURLPairRe) into the decoded Go string — so a value that
// escapes its slashes (`https:\/\/host\/v1`) or any char is classified by its
// real content, not the on-disk escaping. It is best-effort: if the bytes don't
// round-trip as a JSON string (they came from a regex that already excluded a bare
// unescaped quote, so this is rare), it falls back to the raw bytes.
func decodeJSONString(inner []byte) string {
	var s string
	if err := json.Unmarshal(append(append([]byte{'"'}, inner...), '"'), &s); err == nil {
		return s
	}
	return string(inner)
}

// urlHost extracts the lowercased host (no port) from a URL string, or "" if it
// has no parseable host. It tolerates a SCHEME-LESS value (e.g.
// "api.openai.com/v1" or "api.openai.com:443/v1") — which url.Parse would treat
// as path-only with an empty Hostname — by re-parsing it with a synthetic scheme
// so an IDE that writes a bare host is still recognized as a vendor default.
func urlHost(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	u, err := url.Parse(value)
	if err == nil && u.Hostname() != "" {
		return strings.ToLower(u.Hostname())
	}
	// Scheme-less (or otherwise host-less) → retry with a synthetic scheme so the
	// host token is parsed as an authority rather than a path.
	if !strings.Contains(value, "://") {
		if u2, err2 := url.Parse("essaim-scheme://" + value); err2 == nil {
			return strings.ToLower(u2.Hostname())
		}
	}
	return ""
}

// span is a half-open byte range [start,end) of cur that is a comment.
type span struct{ start, end int }

func inAnySpan(spans []span, pos int) bool {
	for _, s := range spans {
		if pos >= s.start && pos < s.end {
			return true
		}
	}
	return false
}

// scanJSONC does ONE string-aware pass over cur and (a) validates it parses as
// JSON once comments are removed, returning an error if not — including an
// UNTERMINATED block comment, which must NOT be silently accepted — and (b)
// returns the byte spans that are comments, so the surgical rewrite can skip
// managed-key pairs that live inside a comment. A "//" or "/*" inside a string
// literal (e.g. inside "https://…") is NOT a comment. An empty/whitespace file is
// valid-but-empty (no spans, no error). Block comments are replaced with a single
// space during validation so they act as a token separator (e.g. `[1/*x*/2]` is
// rejected, not silently merged into the number 12).
func scanJSONC(b []byte) ([]span, error) {
	if strings.TrimSpace(string(b)) == "" {
		return nil, nil
	}
	stripped := make([]byte, 0, len(b))
	var spans []span
	inString := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inString {
			stripped = append(stripped, c)
			if c == '\\' && i+1 < len(b) {
				stripped = append(stripped, b[i+1]) // copy the escaped char verbatim
				i++
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
			stripped = append(stripped, c)
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			start := i
			for i < len(b) && b[i] != '\n' {
				i++
			}
			spans = append(spans, span{start, i})
			if i < len(b) {
				stripped = append(stripped, '\n') // keep the newline so lines don't merge
			}
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			start := i
			i += 2
			closed := false
			for i+1 < len(b) {
				if b[i] == '*' && b[i+1] == '/' {
					closed = true
					break
				}
				i++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated block comment starting at byte %d", start)
			}
			i++ // skip the closing '/'
			spans = append(spans, span{start, i + 1})
			stripped = append(stripped, ' ') // a separator, so adjacent tokens don't merge
		default:
			stripped = append(stripped, c)
		}
	}
	if inString {
		return nil, fmt.Errorf("unterminated string literal")
	}
	var v any
	if err := json.Unmarshal(stripped, &v); err != nil {
		return nil, err
	}
	return spans, nil
}

// vendorDefaultBaseURL is the base_url `essaim unwire` restores an OpenAI-compatible
// tool's config to once the heal watcher has repointed it at the (now dead) proxy,
// WHEN essaim never captured the user's real original upstream (an old wire-record
// with no OriginalBaseURL). It is the OpenAI vendor default — the value an IDE
// factory-reset would itself write — so a user who deletes essaim is left pointing
// at a LIVE upstream, not a dead loopback. (The heal watcher treats exactly this
// host as "vendor default → re-heal", so the inverse is symmetric.)
const vendorDefaultBaseURL = "https://api.openai.com/v1"

// captureOriginalBaseURL reads the tool's IDE config at path and returns the
// FIRST managed base_url value that is not already essaim's own proxy URL — i.e.
// the user's real upstream BEFORE essaim wired/healed it. This is called at WIRE
// time so `essaim unwire` can later restore the user's exact original provider (a
// non-OpenAI vendor, their own gateway) rather than a hardcoded OpenAI default.
//
// It returns "" (and no error) when: path is empty/absent, the config is
// unparseable, there is no managed base_url key, or the only value present is
// already the essaim proxy (nothing pristine to capture). A parse error is NOT
// fatal — capturing the original is best-effort; unwire falls back to the vendor
// default when we couldn't capture it.
func captureOriginalBaseURL(path string) string {
	if path == "" {
		return ""
	}
	cur, err := os.ReadFile(path)
	if err != nil {
		return "" // missing/unreadable — nothing to capture
	}
	comments, err := scanJSONC(cur)
	if err != nil {
		return "" // unparseable — best-effort, don't guess
	}
	for _, m := range baseURLPairRe.FindAllSubmatchIndex(cur, -1) {
		if inAnySpan(comments, m[0]) {
			continue // inside a comment — not a real key
		}
		value := decodeJSONString(cur[m[4]:m[5]])
		if strings.TrimSpace(value) == "" {
			continue // blank — no upstream recorded here
		}
		if isProxyURL(value) {
			continue // already essaim's own value — not the pristine original
		}
		return value // the user's real upstream, verbatim
	}
	return ""
}

// baseURLConfigPath resolves the IDE config file `essaim unwire` must un-heal for a
// base_url tool. It is the same resolver the heal watcher uses to find what to
// keep pointed at the proxy, so unwire acts on exactly the file wire's healer
// wrote. "" means essaim ships no known location for the tool (the caller then
// emits a manual-recovery hint instead of silently claiming success).
func baseURLConfigPath(tool string) string {
	return defaultConfigPath(tool)
}

// RestoreStatus reports the outcome of un-healing a base_url tool's IDE config on
// unwire. Changed is true when essaim removed its own proxy URL from the config.
// NeedsManualHint is true when essaim could not auto-restore the config (it doesn't
// know where the tool stores its config, or no config file was present) — the CLI
// then prints an honest recovery hint rather than claiming "original config
// restored".
type RestoreStatus struct {
	Changed         bool
	NeedsManualHint bool
	// Path is the config file essaim acted on (or would have), for the CLI message.
	Path string
}

// restoreBaseURLConfig is the INVERSE of the heal repair (P1-BUG-2). The heal
// watcher writes the essaim proxy URL into an OpenAI-compatible IDE config; once the
// user deletes the essaim binary that URL points at a DEAD loopback with no
// recovery. On unwire, restoreBaseURLConfig surgically replaces any managed
// base_url key that currently holds the essaim proxy URL with the vendor default
// (api.openai.com), preserving the rest of the file byte-for-byte exactly like the
// forward healer. A value the user set to something else is LEFT ALONE (unwire
// never fights the user). It returns a RestoreStatus so the CLI can print an
// honest message (and a manual-recovery hint when the config location is unknown
// or the file is absent).
func restoreBaseURLConfig(path string) (RestoreStatus, error) {
	return restoreBaseURLConfigTo(path, "")
}

// restoreBaseURLConfigTo is restoreBaseURLConfig with an explicit restoreTo value
// — the user's ORIGINAL upstream captured at wire time (config.WiredTool.
// OriginalBaseURL). When restoreTo is non-empty, essaim's proxy URL is replaced
// with THAT exact value, so the user is returned to their real provider (a
// non-OpenAI vendor, their own gateway). When it is empty (an old wire-record with
// no captured original, or the capture failed), it falls back to the vendor
// default — the previous behavior — and flags a hint so the CLI can tell the user
// to double-check the restored base_url.
func restoreBaseURLConfigTo(path, restoreTo string) (RestoreStatus, error) {
	if path == "" {
		// essaim doesn't know where this tool's config lives — it never wrote one, but
		// the heal watcher only healed files it knew; so there is nothing to undo, but
		// we still flag a hint in case the user pointed some other config at the proxy.
		return RestoreStatus{NeedsManualHint: true}, nil
	}
	cur, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		// No config file present: nothing the heal watcher could have written here.
		return RestoreStatus{Path: path}, nil
	}
	if err != nil {
		return RestoreStatus{Path: path}, err
	}

	// Pick the value to restore to: the captured original when known, else the
	// vendor default (with a hint that essaim guessed).
	target := restoreTo
	guessed := false
	if strings.TrimSpace(target) == "" {
		target = vendorDefaultBaseURL
		guessed = true
	}

	healed, changed, rerr := unhealBaseURLTo(cur, target)
	if rerr != nil {
		// The config is present but unparseable — we can't safely rewrite it. Surface
		// a hint so the user knows to fix the base_url by hand (don't corrupt it).
		return RestoreStatus{Path: path, NeedsManualHint: true}, nil
	}
	if !changed {
		return RestoreStatus{Path: path}, nil
	}
	if err := writeFilePreservingMode(path, healed); err != nil {
		return RestoreStatus{Path: path}, err
	}
	// A restore we had to GUESS (no captured original) still warrants a hint so the
	// user can confirm the base_url is right for their provider.
	return RestoreStatus{Changed: true, Path: path, NeedsManualHint: guessed}, nil
}

// unhealBaseURLTo surgically replaces every managed base_url key CURRENTLY holding
// the essaim proxy URL with restoreTo, preserving formatting/comments exactly like
// the forward healer. A key holding any other value (a user override, or already a
// vendor default) is left byte-untouched. restoreTo is JSON-string-safe only if it
// contains no characters needing escaping; base_url values (URLs) do not, so a raw
// splice is safe here exactly as the forward healer splices lastWritten. It returns
// (out, changed, err); err is a parse failure the caller turns into a manual hint.
func unhealBaseURLTo(cur []byte, restoreTo string) ([]byte, bool, error) {
	comments, err := scanJSONC(cur)
	if err != nil {
		return cur, false, fmt.Errorf("essaim unwire: config is not valid JSON/JSONC, cannot safely restore: %w", err)
	}
	locs := baseURLPairRe.FindAllSubmatchIndex(cur, -1)
	healed := cur
	changed := false
	for i := len(locs) - 1; i >= 0; i-- {
		m := locs[i]
		if inAnySpan(comments, m[0]) {
			continue // inside a comment — not a real key
		}
		valStart, valEnd := m[4], m[5]
		value := decodeJSONString(cur[valStart:valEnd])
		if !isProxyURL(value) {
			continue // only undo essaim's OWN proxy URL; leave everything else alone
		}
		out := make([]byte, 0, len(healed)+len(restoreTo))
		out = append(out, healed[:valStart]...)
		out = append(out, restoreTo...)
		out = append(out, healed[valEnd:]...)
		healed = out
		changed = true
	}
	if !changed {
		return cur, false, nil
	}
	return healed, true, nil
}

// isProxyURL reports whether value is essaim's OWN proxy URL (the value the heal
// watcher writes). Matching on the loopback host+port substring tolerates the /v1
// suffix, a trailing slash, or the exact recorded value.
func isProxyURL(value string) bool {
	return strings.Contains(value, ProxyBaseURL)
}

// writeFilePreservingMode writes content to path, preserving the file's existing
// permission bits (default 0644 if it can't be stat'd). It is the direct-write
// counterpart to heal's atomic rename — unwire is a one-shot user action off any
// hot path, so a straightforward write is sufficient and keeps the tool's mode.
func writeFilePreservingMode(path string, content []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return os.WriteFile(path, content, mode)
}

// defaultConfigPath returns the conventional config file path for a known
// base_url tool, or "" if essaim doesn't ship a default for it. Continue is the
// one with a well-known cross-platform location (~/.continue/config.json). For
// others we have no safe default and rely on a pathOverride.
func defaultConfigPath(tool string) string {
	switch strings.ToLower(tool) {
	case "continue":
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		// Continue keeps its config under ~/.continue across platforms.
		return filepath.Join(home, ".continue", "config.json")
	default:
		// No shipped default. (Cursor stores its model config in an internal
		// SQLite/state store, not a plain file we can safely rewrite, so we don't
		// claim to heal it here — the env-export path is the supported wiring.)
		_ = runtime.GOOS
		return ""
	}
}
