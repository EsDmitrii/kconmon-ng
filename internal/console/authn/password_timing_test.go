package authn_test

import (
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
)

// TestHashPasswordStaysUnderTimingBudget guards against someone "hardening" the argon2id parameters
// into a login timeout.
func TestHashPasswordStaysUnderTimingBudget(t *testing.T) {
	t.Parallel()

	start := time.Now()
	if _, err := authn.HashPassword("timing budget check"); err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if elapsed := time.Since(start); elapsed > timingBudget {
		t.Errorf("HashPassword took %v, want under %v on CI-class hardware", elapsed, timingBudget)
	}
}
