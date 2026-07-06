package bus

import (
	"errors"
	"strings"
	"testing"
)

// A TLS certificate-verification failure (common on a minimal box with no
// ca-certificates installed) must surface an actionable hint, not raw x509
// jargon, so an onboarding user knows the fix.
func TestFriendlyNetErrHintsCACertsOnX509(t *testing.T) {
	raw := errors.New(`Get "https://bus/x": tls: failed to verify certificate: x509: certificate signed by unknown authority`)
	msg := friendlyNetErr(raw).Error()
	if !strings.Contains(strings.ToLower(msg), "ca-certificates") {
		t.Fatalf("x509 failure should hint at ca-certificates; got %q", msg)
	}
}

// A plain connection error passes through (no spurious cert hint).
func TestFriendlyNetErrPassThroughNonTLS(t *testing.T) {
	raw := errors.New("dial tcp: connection refused")
	msg := friendlyNetErr(raw).Error()
	if strings.Contains(strings.ToLower(msg), "ca-certificates") {
		t.Fatalf("a non-TLS error must not get a ca-certificates hint; got %q", msg)
	}
}
