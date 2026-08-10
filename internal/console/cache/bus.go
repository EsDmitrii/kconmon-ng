// Package cache is the Console's pub/sub abstraction (DATA.md §5.3).
package cache

import (
	"context"
	"encoding/json"
)

// Message is the payload carried between a publisher (events.Ingester — the ONLY bus publisher;
// push.MatrixPusher/TopologyPusher deliberately bypass the bus and broadcast hub-locally, see
// internal/console/push) and the ws.Hub.
type Message struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// Publish and Subscribe both take a logical topic name identical to the WebSocket topic ("live",
// "topology", "matrix:tcp:pod", ...).
type Bus interface {
	Publish(ctx context.Context, topic string, msg Message) error
	// The channel is closed only by unsubscribe (idempotent).
	Subscribe(topic string) (msgs <-chan Message, unsubscribe func())
}
