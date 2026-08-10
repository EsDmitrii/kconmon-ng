// Package push owns the Console's server-side snapshot pushers; snapshot topics are deliberately
// LOCAL-ONLY.
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

// signalNudge is the coalescing, non-blocking wake-up shared by both pushers; the channel has
// capacity 1 and is written with select/default.
func signalNudge(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// RunNudgeRelay subscribes to the "live" bus topic and calls Nudge on every nudger when a
// topology_changed event arrives.
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
