package authn_test

import (
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
)

// TestHashPasswordStaysUnderTimingBudget guards against someone "hardening"
// the argon2id parameters into a login timeout: RFC 9106's second
// recommended option (64 MiB, t=3, p=2) is chosen specifically to be cheap
// enough for a console pod's 256Mi default resource limit. This test now
// ALWAYS runs, including under `go test -race` (M-5): it previously split
// into two build-tag-gated files, one of which (the race variant) asserted
// no timing budget at all -- silently disabling the guard for exactly the
// build this repo's own verify command uses (`go test ./internal/console/authn/... -race`).
// timingBudget itself is still build-tag split (password_timing_budget_norace_test.go
// / password_timing_budget_race_test.go), because the race detector's
// instrumentation genuinely does inflate this call's wall-clock cost by
// roughly 7-10x on this hardware -- but the assertion that guards it is now
// one test, live in both builds, not a dead variant in one of them.
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
