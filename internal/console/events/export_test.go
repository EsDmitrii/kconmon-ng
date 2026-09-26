package events

import "time"

// SetConnectGrace overrides the promotion grace period; test-only (this file is compiled only into
// the package's test binary, so it is not part of the package's API).
func (i *Ingester) SetConnectGrace(d time.Duration) { i.connectGrace = d }

// PairScope exposes the emitted-scope renderer so a test can prove the normalizer's output equals
// what the column actually stores.
func PairScope(src, dst string) string { return pairScope(src, dst) }

// SetBaselineInterval overrides how often a connected ingester re-records the topology baseline.
func (i *Ingester) SetBaselineInterval(d time.Duration) { i.baselineInterval = d }

// SetBackoff overrides the reconnect backoff bounds.
func (i *Ingester) SetBackoff(initial, maxBackoff time.Duration) {
	i.initialBackoff, i.maxBackoff = initial, maxBackoff
}
