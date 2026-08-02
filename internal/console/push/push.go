// Package push owns the Console's server-side snapshot pushers: the goroutines
// that recompute a full snapshot on a timer (or on demand) and hand it to the
// ws.Hub for local fan-out.
//
// Snapshot topics are deliberately LOCAL-ONLY: a pusher calls hub.Broadcast and
// never goes through cache.Bus. Every console replica computes its own
// snapshots, so publishing them to Valkey would make N replicas each deliver a
// full snapshot to every browser N times. Only the "live" event topic is
// bus-carried (see internal/console/events) — that asymmetry is intentional.
package push

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/events"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
)

// Nudger is something that can be asked to recompute right now. Both pushers
// implement it; RunNudgeRelay consumes it.
type Nudger interface{ Nudge() }

// signalNudge is the coalescing, non-blocking wake-up shared by both pushers.
// The channel has capacity 1 and is written with select/default, so a burst of
// N concurrent Nudge calls collapses into at most one pending recompute and no
// caller ever blocks — a nudge is a hint, never backpressure.
func signalNudge(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// RunNudgeRelay subscribes to the "live" bus topic and calls Nudge on every
// nudger when a topology_changed event arrives, so a node joining or leaving
// repaints immediately instead of waiting up to a full push interval.
//
// It reacts to topology_changed only. A TopologyChanged event is a refetch
// signal, not a payload, and the high-volume check_observed stream must never
// drive snapshot recomputes.
//
// Blocks until ctx is cancelled (or the bus closes the subscription), then
// unsubscribes.
func RunNudgeRelay(ctx context.Context, bus cache.Bus, nudgers ...Nudger) {
	msgs, unsubscribe := bus.Subscribe(ws.TopicLive)
	defer unsubscribe()

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-msgs:
			if !ok {
				// Defensive; unreachable under the bus contract (the channel
				// closes only via our own deferred unsubscribe, which cannot
				// have run while this loop is still reading).
				return
			}
			var ev events.LiveEvent
			if err := json.Unmarshal(msg.Data, &ev); err != nil {
				slog.Warn("nudge relay: dropping malformed live event", "error", err)
				continue
			}
			if ev.Type != events.TypeTopologyChanged {
				continue
			}
			for _, n := range nudgers {
				n.Nudge()
			}
		}
	}
}
