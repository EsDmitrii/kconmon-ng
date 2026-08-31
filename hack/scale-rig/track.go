package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

/*
propTracker measures "registration -> every watcher holds the updated list" without O(N) work per
received update. Probes are registered strictly one at a time into a fleet of stable size, so each
probe grows every watcher's peer list to a known, strictly increasing length: a watcher proves it
received probe p the moment a FULL_SYNC's len reaches p's threshold. Coverage is an O(1) length
comparison per callback, so the measurement does not distort the CPU numbers it runs next to.
*/
type propTracker struct {
	mu     sync.RWMutex
	probes []*probe
}

type probe struct {
	threshold int
	start     time.Time
	remaining atomic.Int32
	// doneNanos is the wall clock when the LAST expected watcher covered the probe; 0 = not done.
	doneNanos atomic.Int64
}

func (t *propTracker) addProbe(threshold, expected int, start time.Time) *probe {
	p := &probe{threshold: threshold, start: start}
	p.remaining.Store(int32(expected)) //nolint:gosec // G115: expected is a fleet size, far below int32 max
	t.mu.Lock()
	t.probes = append(t.probes, p)
	t.mu.Unlock()
	return p
}

// cover advances one watcher's cursor over every probe its received list length proves delivered.
// The cursor belongs to the caller (one per watcher, guarded by the watcher), which is what makes
// each watcher count once per probe.
func (t *propTracker) cover(cursor *int, listLen int, now time.Time) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for *cursor < len(t.probes) {
		p := t.probes[*cursor]
		if listLen < p.threshold {
			return
		}
		if p.remaining.Add(-1) == 0 {
			p.doneNanos.CompareAndSwap(0, now.UnixNano())
		}
		*cursor++
	}
}

// results returns the completed probes' registration->fleet-covered durations and how many probes
// never completed (a watcher died or the grace period ran out) — reported, not hidden.
func (t *propTracker) results() (done []time.Duration, incomplete int) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	done = make([]time.Duration, 0, len(t.probes))
	for _, p := range t.probes {
		if d := p.doneNanos.Load(); d != 0 {
			done = append(done, time.Unix(0, d).Sub(p.start))
		} else {
			incomplete++
		}
	}
	return done, incomplete
}

// rigCounters is the driver-side ground truth: the rig counts the mutations it performs, so
// "changes" in the coalescing ratio needs no access to the controller's unexported registry.
type rigCounters struct {
	registerOK    atomic.Uint64 // successful Register RPCs (fleet + restarts + probes)
	registerRetry atomic.Uint64
	registerFail  atomic.Uint64 // agents that never managed to register
	deregisterOK  atomic.Uint64
	watchResubs   atomic.Uint64 // WatchPeers streams that ended and were re-subscribed
	reregEntries  atomic.Uint64 // entries into the re-registration loop (mirrors AgentControllerReconnects)
	peerCallbacks atomic.Uint64 // OnPeersUpdate invocations across the fleet
	activeAgents  atomic.Int64  // agents that completed their first registration
}

// changes is the number of registry mutations the rig has driven so far.
func (c *rigCounters) changes() uint64 {
	return c.registerOK.Load() + c.deregisterOK.Load()
}

/*
logCounter is the rig's slog handler: it silences the controller's and agents' per-event Info spam
(2000 registrations would print 2000 lines), counts every Warn+ record by message, and still prints
Error+ records. The counts surface real signals — the server's "peer update could not be queued"
desync warning, agents' "heartbeat failed" — in the final report without grepping logs.
*/
type logCounter struct {
	mu     sync.Mutex
	counts map[string]uint64
	errOut io.Writer
}

func newLogCounter(errOut io.Writer) *logCounter {
	return &logCounter{counts: make(map[string]uint64), errOut: errOut}
}

func (h *logCounter) Enabled(_ context.Context, lvl slog.Level) bool {
	return lvl >= slog.LevelWarn
}

func (h *logCounter) Handle(_ context.Context, r slog.Record) error { //nolint:gocritic // hugeParam: the slog.Handler interface fixes this signature
	h.mu.Lock()
	h.counts[r.Message]++
	h.mu.Unlock()
	if r.Level >= slog.LevelError && h.errOut != nil {
		_, _ = fmt.Fprintf(h.errOut, "%s %s %s\n", r.Time.Format(time.TimeOnly), r.Level, r.Message)
	}
	return nil
}

func (h *logCounter) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *logCounter) WithGroup(string) slog.Handler      { return h }

func (h *logCounter) snapshot() map[string]uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]uint64, len(h.counts))
	for k, v := range h.counts {
		out[k] = v
	}
	return out
}

// sortedLogLines renders the counted messages deterministically for the report.
func sortedLogLines(counts map[string]uint64) []string {
	lines := make([]string, 0, len(counts))
	for msg, n := range counts {
		lines = append(lines, fmt.Sprintf("%6d  %s", n, msg))
	}
	sort.Strings(lines)
	return lines
}
