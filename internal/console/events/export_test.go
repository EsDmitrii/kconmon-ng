package events

import "time"

// SetConnectGrace overrides the promotion grace period; test-only (this file is compiled only into
// the package's test binary, so it is not part of the package's API).
func (i *Ingester) SetConnectGrace(d time.Duration) { i.connectGrace = d }

// PairScope exposes the emitted-scope renderer so a test can prove the normalizer's output equals
// what the column actually stores.
func PairScope(src, dst string) string { return pairScope(src, dst) }
