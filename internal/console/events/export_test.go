package events

import "time"

// SetConnectGrace overrides the promotion grace period. Test-only (this file is
// compiled only into the package's test binary, so it is not part of the
// package's API): it lets a test stretch the grace far beyond the test's own
// runtime, so that observing Healthy() == true can only mean an event promoted
// the ingester and not that the grace timer fired. Call before Run.
func (i *Ingester) SetConnectGrace(d time.Duration) { i.connectGrace = d }
