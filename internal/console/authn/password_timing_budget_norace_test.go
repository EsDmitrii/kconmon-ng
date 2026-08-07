//go:build !race

package authn_test

import "time"

// timingBudget is TestHashPasswordStaysUnderTimingBudget's guard (see
// password_timing_test.go): RFC 9106's second recommended option (64 MiB,
// t=3, p=2) measured ~50-70ms on this task's dev hardware in a normal
// (non-race) build, well under this budget.
const timingBudget = 400 * time.Millisecond
