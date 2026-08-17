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

	/* CrossReplica reports whether a Publish here can reach ANOTHER console process.

	   It exists because "there is a bus" and "there is a way to talk to the other replica" are not
	   the same statement, and one caller bet its correctness on confusing them. Runner.Cancel
	   forwards a cancel it cannot service locally by publishing it, and its only guard was
	   `bus == nil`. The in-process bus is not nil: its Publish fans out to local subscribers and
	   returns nil unconditionally, so on a console that fell back to it (redis unreachable at
	   startup, which newBus does silently while the deployment still runs two replicas) the cancel
	   was swallowed, the log said "cancel forwarded to the replica that owns the run", and the API
	   answered 204 while the run went on dispatching to the fleet for its full duration.

	   A publish that only ever reaches this process must be able to say so. */
	CrossReplica() bool
}
