//go:build !race

package authn_test

import "time"

// timingBudget is TestHashPasswordStaysUnderTimingBudget's guard (see password_timing_test.go).
const timingBudget = 400 * time.Millisecond
