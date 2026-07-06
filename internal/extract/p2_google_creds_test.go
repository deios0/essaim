package extract

import (
	"strings"
	"testing"
)

// P2-4: the credential redactor missed unlabeled Google provider tokens. Google
// OAuth ACCESS tokens (ya29.…) and REFRESH tokens (1//…) are high-entropy keyed
// tokens with distinctive prefixes but no api_key=/token= label, so the keyed
// `(api_key|token|…): …` branch never caught them and they landed in a draft /
// were syncable in the clear.
//
// (Slack xoxb-/xoxp-, GitHub fine-grained PATs github_pat_, and OpenAI project
// keys sk-proj-… are ALREADY covered by the existing patterns — see
// TestExistingProviderTokensStillRedacted below — so no redundant/over-broad
// pattern is added for them.)

func TestGoogleTokensRedacted(t *testing.T) {
	// Realistic-shape FAKE tokens (never real secrets).
	googleAccess := "ya29.a0AfB_byC1234567890abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOP-_"
	googleRefresh := "1//0eXaMPLerefreshTOKEN1234567890abcdefghijklmnopqrABCDEF-_"

	for name, tok := range map[string]string{
		"google_oauth_access": googleAccess,
		"google_refresh":      googleRefresh,
	} {
		if !ContainsCredential(tok) {
			t.Errorf("%s: ContainsCredential must detect %q", name, tok)
		}
		red := RedactCredentials("here is the token " + tok + " keep it safe")
		if strings.Contains(red, tok) {
			t.Errorf("%s: RedactCredentials must remove the token; got %q", name, red)
		}
		if !strings.Contains(red, "[REDACTED]") {
			t.Errorf("%s: RedactCredentials must leave a [REDACTED] marker; got %q", name, red)
		}
		// Surrounding prose is preserved.
		if !strings.Contains(red, "here is the token ") || !strings.Contains(red, " keep it safe") {
			t.Errorf("%s: surrounding prose must survive; got %q", name, red)
		}
	}
}

// The added Google patterns must NOT false-positive on ordinary prose — anchored
// on the distinctive prefixes, not a broad high-entropy gate.
func TestGoogleTokenPatternsNoFalsePositive(t *testing.T) {
	prose := []string{
		"the answer is 1//2 of the total budget",               // a fraction, not 1//<token>
		"drive on the D1//D2 road toward the coast",            // 1//D2 is short, not a token
		"I prefer to use the ya29 highway near home",           // ya29 without a dot
		"see figure ya29.1 in the appendix for details",        // ya29. but short numeric, not a token
		"always avoid inheritance; prefer composition instead", // ordinary preference
		"use PostgreSQL not MySQL because it is the rule",      // ordinary preference
		"the ratio was 1//3 then 1//4 across the quarters",     // fractions
	}
	for _, p := range prose {
		if ContainsCredential(p) {
			t.Errorf("prose must NOT trip the credential gate: %q", p)
		}
		if RedactCredentials(p) != p {
			t.Errorf("prose must be returned unchanged: %q -> %q", p, RedactCredentials(p))
		}
	}
}

// Guard: the provider tokens the bug listed as "missed" that are ALREADY covered
// must stay covered (regression anchor so a future edit can't silently drop them).
func TestExistingProviderTokensStillRedacted(t *testing.T) {
	covered := map[string]string{
		"slack_bot_xoxb":      "xoxb-EXAMPLE-EXAMPLE-REDACTEDTESTFIXTURE",
		"slack_user_xoxp":     "xoxp-EXAMPLE-EXAMPLE-EXAMPLE-REDACTEDTESTFIXTURE",
		"github_fine_grained": "github_pat_11ABCDEFG0abcdefghijklmnop_qRsTuVwXyZ1234567890abcdefghijklmnopqrstuvwxyz",
		"openai_project_key":  "sk-proj-abcdefABCDEF1234567890abcdefABCDEF1234567890Tz",
	}
	for name, tok := range covered {
		if !ContainsCredential(tok) {
			t.Errorf("%s: must remain covered by the existing patterns: %q", name, tok)
		}
	}
}
