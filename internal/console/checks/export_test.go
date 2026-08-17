package checks

import "time"

/*
 * export_test.go — the seams the external test package needs and the production API must not carry.
 *
 * The reconcile harness lives in package checks_test (it wires fakes through the public
 * constructor), so a case about the resync memo has to reach two unexported things: the timestamp of
 * the last push, and the interval that ages it. Neither belongs on Reconciler's real surface — the
 * memo is an implementation detail of leaderTick — so they are exported HERE, in a file the compiler
 * only includes for tests.
 */

// ExternalResyncInterval is how stale the local push memo may be before the desired state is
// re-asserted regardless of equality.
const ExternalResyncInterval = externalResyncInterval

// SetLastPushedAt ages the reconciler's push memo, so a test can reach the resync branch without
// sleeping through it.
func (r *Reconciler) SetLastPushedAt(at time.Time) { r.lastPushedAt = at }
