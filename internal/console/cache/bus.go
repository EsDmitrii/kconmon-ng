// Package cache is the Console's pub/sub abstraction (DATA.md §5.3): a Bus
// fans domain messages out to every local WebSocket-serving replica. Valkey
// (ValkeyBus) is the real cross-replica implementation; InProcessBus is the
// documented single-replica fallback for console.valkey.mode=disabled
// (ADR-002).
package cache

import (
	"context"
	"encoding/json"
)

// Message is the payload carried between a publisher (events.Ingester — the
// ONLY bus publisher; push.MatrixPusher/TopologyPusher deliberately bypass the
// bus and broadcast hub-locally, see internal/console/push) and the ws.Hub.
// Type is one of snapshot|delta|event; Hub assigns the per-topic Seq on the
// way out to browsers.
type Message struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// Bus is the pub/sub seam. Publish and Subscribe both take a logical topic
// name identical to the WebSocket topic ("live", "topology", "matrix:tcp:pod",
// ...) — backends are free to namespace it internally (ValkeyBus prefixes
// with "events:" per DATA.md's events:* channel convention).
type Bus interface {
	Publish(ctx context.Context, topic string, msg Message) error
	// Subscribe returns a channel of messages for topic and an unsubscribe
	// func. The channel is closed only by unsubscribe (idempotent); Bus
	// shutdown (ValkeyBus.Close) stops delivery but leaves subscriber
	// channels open — consumers stop on their own context, never by
	// waiting for channel close.
	Subscribe(topic string) (msgs <-chan Message, unsubscribe func())
}
