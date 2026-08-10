package events

import "time"

// SetConnectGrace overrides the promotion grace period; test-only (this file is compiled only into
// the package's test binary, so it is not part of the package's API).
func (i *Ingester) SetConnectGrace(d time.Duration) { i.connectGrace = d }
