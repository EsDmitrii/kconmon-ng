package httpapi

import (
	"strconv"
	"strings"
	"testing"
)

// The sign-in 429s state the per-address budget as it is computed, loginIPBurstFactor times the
// per-username one, so the message follows the constant.
func TestSignInRateLimitDetailsStateTheAddressBudget(t *testing.T) {
	factor := "x " + strconv.Itoa(loginIPBurstFactor) + " per source address"
	for name, detail := range map[string]string{
		"login":      loginRateLimitDetail,
		"oidc start": oidcStartRateLimitDetail,
	} {
		if !strings.Contains(detail, factor) {
			t.Errorf("%s 429 detail %q does not state the per-address budget as %q", name, detail, factor)
		}
	}
}
