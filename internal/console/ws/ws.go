// Package ws is the Console's multiplexed WebSocket surface (ADR-003); snapshot topics ("topology",
// "matrix:*") never touch the bus.
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

// MatrixTopic returns the topic carrying the connectivity matrix for protocol; the ":pod" suffix is
// part of the topic NAME.
func MatrixTopic(protocol string) string { return "matrix:" + protocol + ":pod" }

// runTopicPrefix identifies run:{id} topics — RunTopic is the canonical
// constructor, and ringSize (hub.go) keys its append-only ring branch off it.
const runTopicPrefix = "run:"

// RunTopic returns the WebSocket topic carrying progress for one diagnostics run.
func RunTopic(runID string) string { return runTopicPrefix + runID }

// IsRunTopic reports whether topic is a run:{id} topic; exported because the run topics are the ONE
// class an authorizer has to be able to name (a runs:read-only subject may watch its own run and
// nothing else -- httpapi.wsTopicAuthorizer).
func IsRunTopic(topic string) bool { return strings.HasPrefix(topic, runTopicPrefix) }

// allowedTopics is the static allowlist.
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
