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

import (
	"encoding/json"
	"strings"
)

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
// for the coalesced per-pair matrix updates SECURITY.md §12 describes. closed
// is Task 20's addition (hub.go CloseTopic): the terminal signal on a run:{id}
// topic, sent once, so a subscribed browser tab learns the run is over instead
// of the topic silently going idle. It carries no Data payload — the type is
// the signal — and, like every other data frame, an ordinary increasing Seq
// (unlike error, which is not a data frame and always carries Seq 0).
const (
	TypeSnapshot = "snapshot"
	TypeDelta    = "delta"
	TypeEvent    = "event"
	TypeError    = "error"
	TypeClosed   = "closed"
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
// The ":pod" suffix is part of the topic NAME only — a forward-compatibility
// slot in the topic shape; matrix.Compute has no plane parameter, because
// plane=pod is the only plane that exists.
func MatrixTopic(protocol string) string { return "matrix:" + protocol + ":pod" }

// runTopicPrefix identifies run:{id} topics — RunTopic is the canonical
// constructor, and ringSize (hub.go) keys its append-only ring branch off it.
const runTopicPrefix = "run:"

// RunTopic returns the WebSocket topic carrying progress for one diagnostics
// run. ADR-003 names run:{id}; M2 deferred it for want of a consumer
// (WEBSOCKET.md lines 79-82), and the M3 runner is that consumer.
func RunTopic(runID string) string { return runTopicPrefix + runID }

// IsRunTopic reports whether topic is a run:{id} topic. Exported because the
// run topics are the ONE class an authorizer has to be able to name (a
// runs:read-only subject may watch its own run and nothing else --
// httpapi.wsTopicAuthorizer), and a caller re-deriving that from a literal
// "run:" prefix of its own would silently diverge the day this constructor
// changes. Deliberately a predicate rather than an exported prefix constant,
// so the topic grammar stays this package's to define.
func IsRunTopic(topic string) bool { return strings.HasPrefix(topic, runTopicPrefix) }

// allowedTopics is the M2 static allowlist. mtr is named by ADR-003 but has no
// consumer before M5, so subscribing to it is an error rather than a topic
// that silently never delivers. run:{id} topics are NOT here — they are
// ephemeral, registered per-run via Hub.OpenTopic, and Hub.topicAllowed checks
// both this map and the ephemeral registry.
var allowedTopics = map[string]struct{}{
	TopicLive:           {},
	TopicTopology:       {},
	MatrixTopic("tcp"):  {},
	MatrixTopic("udp"):  {},
	MatrixTopic("icmp"): {},
}

// errorPayload is the Data of an Envelope{Type: TypeError}.
type errorPayload struct {
	Error string `json:"error"`
}
