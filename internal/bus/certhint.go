package bus

import (
	"fmt"
	"strings"
)

// friendlyNetErr rewrites a transport error into an actionable message. A TLS
// certificate-verification failure (typically a minimal box or container with no
// ca-certificates installed) is the common onboarding footgun; surface the fix
// instead of raw x509 jargon. Other errors pass through unchanged.
func friendlyNetErr(err error) error {
	if err == nil {
		return nil
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "x509") || strings.Contains(s, "failed to verify certificate") || strings.Contains(s, "certificate signed by unknown authority") {
		return fmt.Errorf("could not establish a secure connection to the bus — the system has no trusted CA certificates. Install ca-certificates (e.g. `apt-get install ca-certificates` / `apk add ca-certificates`) and retry. (%w)", err)
	}
	return err
}
