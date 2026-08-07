//go:build race

package authn_test

import "time"

// timingBudget is TestHashPasswordStaysUnderTimingBudget's guard (see
// password_timing_test.go), widened for the race detector's instrumentation
// overhead: the exact same argon2.IDKey(m=65536,t=3,p=2,...) call measured
// ~500ms under `go test -race` on this task's dev hardware (~7-10x the
// non-race ~50-70ms), which is a property of the instrumentation, not of
// the parameters this test exists to guard. Still a real, live budget under
// -race -- not skipped -- just wide enough not to be flaky from
// instrumentation overhead alone (observed one >2s spike on a cold build
// cache under parallel compilation; 4s keeps the guard live without that
// flake — the tight 400ms guard is the non-race build's).
const timingBudget = 4 * time.Second
