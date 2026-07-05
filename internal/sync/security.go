package sync

import (
	"fmt"
	"regexp"
	"strings"

	"oikos/internal/extract"
	"oikos/internal/rules"
)

// This file holds the SECURITY CONTRACT of the sync primitive — the seed of the
// future paid Team-Rule-Sync. Four properties are enforced here (security review):
//
//   P0  QUARANTINE ON PULL — every incoming REMOTE rule lands as a status:draft
//       in _inbox/, never in the active/injectable set, until the local user
//       explicitly accepts it. A pull can NEVER auto-apply a remote rule into the
//       live index. This is the in-team supply-chain wall.
//   P1  TIER CLAMP — a synced/remote rule can never acquire a privileged tier
//       (guardrail/identity) on import; only a LOCAL author mints a guardrail.
//   P1  CREDENTIAL GATE — a rule whose body trips the credential check is refused
//       on PUSH and quarantined+flagged on PULL; a secret is never written to the
//       active set or pushed in the clear.
//   P1  GIT ARG INJECTION — the user-supplied remote URL + branch are validated
//       so a crafted `--upload-pack=…` / `-`-prefixed / `ext::`/`file://` value
//       cannot inject a git option or a transport command.

// --- P1: git argument / transport injection ----------------------------------

// allowedSchemes is the allowlist of URL schemes a remote may use. `ext::` (runs
// an arbitrary command), `file://` (local-FS read of an attacker-named path), and
// any other scheme are REJECTED. A scp-like `git@host:path` and a bare local
// path carry no scheme and are validated structurally instead.
var allowedSchemes = map[string]bool{
	"https": true,
	"http":  true, // plain http is allowed (a user's own LAN remote); egress is still the user's box
	"ssh":   true,
	"git":   true,
}

// scpLikeRe matches the scp-like ssh form `user@host:path` (no `://`). The user
// and host parts must each START with an alphanumeric (so neither can begin with
// `-` and be misread by ssh as an option — defense in depth atop the leading-`-`
// reject and the git `--` separator).
var scpLikeRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*@[A-Za-z0-9][A-Za-z0-9._-]*:.+$`)

// validateRemoteURL rejects a remote that could inject a git option or invoke a
// dangerous transport. Rules:
//   - non-empty, no leading `-` (else git parses it as an option), no whitespace;
//   - a scheme:// remote must use an allowlisted scheme (no ext::, no file://);
//   - an scp-like git@host:path is allowed (host/user must not start with `-`);
//   - a plain local path is allowed (must not start with `-`, must not look like
//     a scheme).
//
// The caller additionally passes `--` to git so even a value that slips a leading
// `-` past a future edit cannot be read as an option (defense in depth).
func validateRemoteURL(remote string) error {
	r := remote // do NOT trim: leading whitespace before a `-` is itself an attack tell
	if r == "" {
		return fmt.Errorf("oikos sync: empty git remote")
	}
	if strings.ContainsAny(r, " \t\r\n") {
		return fmt.Errorf("oikos sync: remote %q contains whitespace (rejected — possible argument injection)", remote)
	}
	if strings.HasPrefix(r, "-") {
		return fmt.Errorf("oikos sync: remote %q starts with '-' (rejected — git would read it as an option)", remote)
	}
	// Scheme form: scheme://...
	if i := strings.Index(r, "://"); i >= 0 {
		scheme := strings.ToLower(r[:i])
		if !allowedSchemes[scheme] {
			return fmt.Errorf("oikos sync: remote scheme %q is not allowed (use https/ssh/git or a local path)", scheme)
		}
		return nil
	}
	// A `scheme::` transport (e.g. ext::, fd::, file::) — never allowed: ext:: runs
	// an arbitrary command. Any `word::` prefix is a git transport helper.
	if transportHelperRe.MatchString(r) {
		return fmt.Errorf("oikos sync: remote %q uses a git transport helper (rejected — possible command execution)", remote)
	}
	// scp-like ssh: user@host:path
	if scpLikeRe.MatchString(r) {
		return nil
	}
	// A bare local path. It must not contain a `:` that could be misread as a
	// scheme/host separator (a `:` here is already handled by the scp-like branch).
	if strings.Contains(r, ":") {
		return fmt.Errorf("oikos sync: remote %q is not a recognized remote form (use https/ssh/git, git@host:path, or a local path)", remote)
	}
	return nil
}

// transportHelperRe matches a leading `word::` git transport-helper prefix
// (ext::, fd::, file::, …). Anchored at start; the scheme word is letters/digits.
var transportHelperRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*::`)

// branchRe is the allowlist for a branch/ref name: ordinary git ref characters
// only (letters, digits, `.`, `_`, `-`, `/`). It deliberately FORBIDS whitespace,
// `~^:?*[\`, leading `-`, and `..` so a branch can never be read as a git option
// or a malicious refspec.
var branchRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// validateBranch rejects a branch that could be parsed as a git option or is not
// a well-formed ref name. A leading `-` (option), whitespace, `..` (parent-ref /
// range), and the special ref characters are all rejected.
func validateBranch(branch string) error {
	if branch == "" {
		return fmt.Errorf("oikos sync: empty branch")
	}
	if strings.HasPrefix(branch, "-") {
		return fmt.Errorf("oikos sync: branch %q starts with '-' (rejected — git would read it as an option)", branch)
	}
	if strings.Contains(branch, "..") {
		return fmt.Errorf("oikos sync: branch %q contains '..' (rejected — not a valid ref name)", branch)
	}
	if !branchRe.MatchString(branch) {
		return fmt.Errorf("oikos sync: branch %q is not a valid ref name (allowed: letters, digits, . _ - /)", branch)
	}
	return nil
}

// --- P1: privileged-tier clamp ----------------------------------------------

// isPrivilegedKind reports whether a rule Kind maps to a PRIVILEGED tier
// (guardrail tier-2 or identity tier-1). Such tiers carry hard injection/immunity
// power and may only be minted by a LOCAL author — never acquired via sync.
// It defers to the hot-path tier mapping (internal/rules) so "privileged" means
// exactly what the injector means by it.
func isPrivilegedKind(kind string) bool {
	return rules.Tier(kind) > 0
}

// ordinaryKind is the tier-0 kind a clamped remote rule is rewritten to. An
// empty kind is already tier-0; a privileged kind is rewritten to "rule".
func clampKind(kind string) string {
	if isPrivilegedKind(kind) {
		return "rule"
	}
	return kind
}

// --- P0 + P1: quarantine on pull --------------------------------------------

// quarantineRemote turns an incoming REMOTE record into a quarantined DRAFT: it
// is clamped to draft status (so it can NEVER be injectable/live — see
// internal/rules.isInjectable) and to a non-privileged tier (so a remote can
// never mint a guardrail/identity rule). This is applied to EVERY remote record
// before it is written anywhere on the local box. The durable content (Title /
// Body / Identity / clock) is preserved so the user can review and explicitly
// accept it later via the M3 promote mechanism.
//
// P2-5 credential redaction: a remote rule whose Body/Title trips the credential
// gate has the secret REDACTED here (reusing the SAME extract redaction the
// capture path and emitter use) and is FLAGGED (CredentialFlagged →
// `credential_redacted: true`). This closes the leak where a credential-bearing
// remote rule previously landed in the local _inbox/ in plaintext: the comment
// claimed "quarantined + flagged" but nothing redacted or flagged it. A secret
// from a remote must never sit plaintext in the local vault — even in a draft
// that is never injected. The flag surfaces the strip to the human reviewer.
func quarantineRemote(r Record) Record {
	r.Status = rules.StatusDraft
	r.Kind = clampKind(r.Kind)
	r.RemoteOrigin = true // the lifecycle sweep must never auto-promote a remote rule
	if bodyHasCredential(r) {
		// Redact BOTH durable text fields (a secret can hide in either) with the
		// shared extract redactor, then flag. Redaction changes the durable content,
		// so the cid recomputed on write reflects the redacted (safe) content.
		r.Body = extract.RedactCredentials(r.Body)
		r.Title = extract.RedactCredentials(r.Title)
		r.CredentialFlagged = true
	}
	return r
}

// --- P1: credential gate ----------------------------------------------------

// bodyHasCredential reports whether a record's durable content carries a
// credential-shaped span (a private-key block/marker or any credentialPattern
// match). It reuses the SAME hard-reject predicate the capture redactor and the
// emitter use (internal/extract), so the sync gate can never drift from the rest
// of the system. Checks Body and Title (a secret can hide in either).
func bodyHasCredential(r Record) bool {
	for _, s := range []string{r.Body, r.Title} {
		if extract.ContainsCredential(s) || extract.ContainsPrivateKeyMarker(s) {
			return true
		}
	}
	return false
}

// firstCredentialBearing returns the first record in recs whose content trips the
// credential gate, or (Record{}, false). Used on the PUSH side to refuse syncing
// a credential-bearing local rule.
func firstCredentialBearing(recs []Record) (Record, bool) {
	for _, r := range recs {
		if bodyHasCredential(r) {
			return r, true
		}
	}
	return Record{}, false
}

// dropCredentialBearing returns recs with every credential-bearing record
// removed. Applied to the PUSH SET (the published union) so this machine never
// re-commits a secret under the user's git identity — even a credential that
// arrived in a remote-origin rule is excluded from the push. (A LOCAL
// credential-bearing rule has already hard-failed the whole sync upstream; this
// is the strict belt-and-braces gate on the union, covering the remote-origin
// case. It returns a new slice; the input is unmodified.)
func dropCredentialBearing(recs []Record) []Record {
	out := make([]Record, 0, len(recs))
	for _, r := range recs {
		if bodyHasCredential(r) {
			continue
		}
		out = append(out, r)
	}
	return out
}
