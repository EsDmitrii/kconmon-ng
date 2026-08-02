// Package ws is the Console's multiplexed WebSocket surface (ADR-003): one
// socket per browser tab at GET /ws, N topics multiplexed over it, per-topic
// sequence numbers, and a bounded per-topic replay ring.
//
// The Hub is fed from two directions, and the asymmetry is deliberate. The
// "live" event topic arrives over cache.Bus, because every console replica
// ingests the controller's event stream and Valkey fans each event to all of
// them — so Run de-duplicates by events.LiveEvent.ID. Snapshot topics
// ("topology", "matrix:*") never touch the bus: internal/console/push calls
// Broadcast directly, since each replica computes its own snapshots and routing
// them through the bus would make N replicas deliver every snapshot N times.
package ws

import "encoding/json"

// Envelope is every server→client frame. Mirrored by hand as WsEnvelope in
// web/src/lib/ws.ts (repo convention: no codegen).
type Envelope struct {
	Topic string          `json:"topic"`
	Type  string          `json:"type"`
	Seq   uint64          `json:"seq"`
	Data  json.RawMessage `json:"data"`
}

// ClientMessage is every client→server frame. Mirrored by hand as ClientMessage
// in web/src/lib/ws.ts.
type ClientMessage struct {
	Action  string `json:"action"`
	Topic   string `json:"topic"`
	LastSeq uint64 `json:"lastSeq,omitempty"`
}

// Envelope.Type values. M2 emits snapshot, event and error; delta is reserved
// for the coalesced per-pair matrix updates SECURITY.md §12 describes.
const (
	TypeSnapshot = "snapshot"
	TypeDelta    = "delta"
	TypeEvent    = "event"
	TypeError    = "error"
)

// ClientMessage.Action values.
const (
	ActionSubscribe   = "subscribe"
	ActionUnsubscribe = "unsubscribe"
)

// Topic names. TopicLive is also the cache.Bus topic the events ingester
// publishes on.
const (
	TopicLive     = "live"
	TopicTopology = "topology"
)

// MatrixTopic returns the topic carrying the connectivity matrix for protocol.
// The ":pod" suffix is part of the topic NAME only (the shape
// docs/console/architecture/WEBSOCKET.md specifies); matrix.Compute has no plane
// parameter, because plane=pod is the only plane that exists.
func MatrixTopic(protocol string) string { return "matrix:" + protocol + ":pod" }

// allowedTopics is the M2 allowlist. run:{id} and mtr are named by ADR-003 but
// have no consumer before M3/M5, so subscribing to them is an error rather than
// a topic that silently never delivers.
var allowedTopics = map[string]struct{}{
	TopicLive:           {},
	TopicTopology:       {},
	MatrixTopic("tcp"):  {},
	MatrixTopic("udp"):  {},
	MatrixTopic("icmp"): {},
}

func topicAllowed(topic string) bool {
	_, ok := allowedTopics[topic]
	return ok
}

// errorPayload is the Data of an Envelope{Type: TypeError}.
type errorPayload struct {
	Error string `json:"error"`
}
