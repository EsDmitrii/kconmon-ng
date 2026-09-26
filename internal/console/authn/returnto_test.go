package authn_test

import (
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
)

func TestIsSafeReturnTo(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"/":                       true,
		"/matrix?protocol=pmtu":   true,
		"/investigate#incident-1": true,
		"/%2F%2Fevil.example":     true,
		"":                        false,
		"matrix":                  false,
		"//evil.example":          false,
		"/\\evil.example":         false,
		"/\t/evil.example":        false,
		"/\n/evil.example":        false,
		"https://evil.example/":   false,
		"\\\\evil.example":        false,
		"/a\\b":                   false,
	}
	for in, want := range cases {
		if got := authn.IsSafeReturnTo(in); got != want {
			t.Errorf("IsSafeReturnTo(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSafeReturnToFallsBackForAnUnsafeTarget(t *testing.T) {
	t.Parallel()
	if got := authn.SafeReturnTo("/matrix", "/"); got != "/matrix" {
		t.Errorf("SafeReturnTo(/matrix) = %q, want /matrix", got)
	}
	if got := authn.SafeReturnTo("//evil.example", "/"); got != "/" {
		t.Errorf("SafeReturnTo(//evil.example) = %q, want the fallback /", got)
	}
}
