package events_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/console/events"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fixedNanos is a constant controller timestamp so the derived id is asserted
// literally rather than recomputed by the test.
const fixedNanos int64 = 1753400000000000000

func fixedTime() *timestamppb.Timestamp { return timestamppb.New(time.Unix(0, fixedNanos).UTC()) }

func TestToLiveEventEveryPayloadType(t *testing.T) {
	tests := []struct {
		name        string
		event       *pb.Event
		wantID      string
		wantType    string
		wantSever   string
		wantScope   string
		wantSummary string
		wantDetails string
	}{
		{
			name: "topology_changed with a node",
			event: &pb.Event{Seq: 3, Timestamp: fixedTime(), Payload: &pb.Event_TopologyChanged{
				TopologyChanged: &pb.TopologyChanged{
					Reason: "agent_registered", NodeName: "node-a", AgentId: "agent-a", Zone: "zone-a",
				},
			}},
			wantID:      "3-1753400000000000000",
			wantType:    "topology_changed",
			wantSever:   "info",
			wantScope:   "node-a",
			wantSummary: "topology changed: agent_registered (node-a)",
			wantDetails: `{"reason":"agent_registered","nodeName":"node-a","agentId":"agent-a","zone":"zone-a"}`,
		},
		{
			// zone_updated's whole subject is the new zone, so it is the one
			// reason whose details would be meaningless without it.
			name: "topology_changed carries the new zone on a zone_updated",
			event: &pb.Event{Seq: 5, Timestamp: fixedTime(), Payload: &pb.Event_TopologyChanged{
				TopologyChanged: &pb.TopologyChanged{
					Reason: "zone_updated", NodeName: "node-a", AgentId: "agent-a", Zone: "zone-b",
				},
			}},
			wantID:      "5-1753400000000000000",
			wantType:    "topology_changed",
			wantSever:   "info",
			wantScope:   "node-a",
			wantSummary: "topology changed: zone_updated (node-a)",
			wantDetails: `{"reason":"zone_updated","nodeName":"node-a","agentId":"agent-a","zone":"zone-b"}`,
		},
		{
			name: "topology_changed without a node scopes to the cluster",
			event: &pb.Event{Seq: 4, Timestamp: fixedTime(), Payload: &pb.Event_TopologyChanged{
				TopologyChanged: &pb.TopologyChanged{Reason: "agent_evicted"},
			}},
			wantID:      "4-1753400000000000000",
			wantType:    "topology_changed",
			wantSever:   "info",
			wantScope:   "cluster",
			wantSummary: "topology changed: agent_evicted",
			wantDetails: `{"reason":"agent_evicted","nodeName":"","agentId":"","zone":""}`,
		},
		{
			name: "check_observed failure is severity error",
			event: &pb.Event{Seq: 17, Timestamp: fixedTime(), Payload: &pb.Event_CheckObserved{
				CheckObserved: &pb.CheckObserved{
					TaskId: "t-1", CheckType: "tcp", SourceNode: "node-a", DestinationNode: "node-b",
					Plane: "pod", Success: false, DurationNs: 1200000, Error: "dial timeout",
				},
			}},
			wantID:      "17-1753400000000000000",
			wantType:    "check_observed",
			wantSever:   "error",
			wantScope:   "node-a→node-b",
			wantSummary: "tcp check node-a→node-b failed",
			wantDetails: `{"taskId":"t-1","checkType":"tcp","plane":"pod","success":false,"durationNs":1200000,"error":"dial timeout"}`,
		},
		{
			name: "check_observed success is severity info",
			event: &pb.Event{Seq: 18, Timestamp: fixedTime(), Payload: &pb.Event_CheckObserved{
				CheckObserved: &pb.CheckObserved{
					TaskId: "t-2", CheckType: "udp", SourceNode: "node-a", DestinationNode: "node-b",
					Plane: "pod", Success: true, DurationNs: 5000,
				},
			}},
			wantID:      "18-1753400000000000000",
			wantType:    "check_observed",
			wantSever:   "info",
			wantScope:   "node-a→node-b",
			wantSummary: "udp check node-a→node-b succeeded",
			wantDetails: `{"taskId":"t-2","checkType":"udp","plane":"pod","success":true,"durationNs":5000,"error":""}`,
		},
		{
			name: "mtr_triggered",
			event: &pb.Event{Seq: 19, Timestamp: fixedTime(), Payload: &pb.Event_MtrTriggered{
				MtrTriggered: &pb.MTRTriggered{TaskId: "t-3", SourceNode: "node-a", DestinationNode: "node-b"},
			}},
			wantID:      "19-1753400000000000000",
			wantType:    "mtr_triggered",
			wantSever:   "info",
			wantScope:   "node-a→node-b",
			wantSummary: "mtr triggered node-a→node-b",
			wantDetails: `{"taskId":"t-3"}`,
		},
		{
			name: "mtr_completed carries hops",
			event: &pb.Event{Seq: 20, Timestamp: fixedTime(), Payload: &pb.Event_MtrCompleted{
				MtrCompleted: &pb.MTRCompleted{
					TaskId: "t-4", SourceNode: "node-a", DestinationNode: "node-b", Success: true,
					Hops: []*pb.MTRHop{{Number: 1, Ip: "10.0.0.1", Hostname: "hop-1", RttNs: 1000000, LossRatio: 0}},
				},
			}},
			wantID:      "20-1753400000000000000",
			wantType:    "mtr_completed",
			wantSever:   "info",
			wantScope:   "node-a→node-b",
			wantSummary: "mtr node-a→node-b succeeded with 1 hops",
			wantDetails: `{"taskId":"t-4","success":true,"error":"","hops":[{"number":1,"ip":"10.0.0.1","hostname":"hop-1","rttNs":1000000,"lossRatio":0}]}`,
		},
		{
			name: "mtr_completed failure without hops still emits an empty array",
			event: &pb.Event{Seq: 21, Timestamp: fixedTime(), Payload: &pb.Event_MtrCompleted{
				MtrCompleted: &pb.MTRCompleted{
					TaskId: "t-5", SourceNode: "node-a", DestinationNode: "node-b",
					Success: false, Error: "no route",
				},
			}},
			wantID:      "21-1753400000000000000",
			wantType:    "mtr_completed",
			wantSever:   "error",
			wantScope:   "node-a→node-b",
			wantSummary: "mtr node-a→node-b failed with 0 hops",
			wantDetails: `{"taskId":"t-5","success":false,"error":"no route","hops":[]}`,
		},
		{
			name: "diagnostic_progress dispatched is info",
			event: &pb.Event{Seq: 22, Timestamp: fixedTime(), Payload: &pb.Event_DiagnosticProgress{
				DiagnosticProgress: &pb.DiagnosticProgress{
					TaskId: "t-6", CheckType: "tcp", SourceNode: "node-a", DestinationNode: "node-b", State: "dispatched",
				},
			}},
			wantID:      "22-1753400000000000000",
			wantType:    "diagnostic_progress",
			wantSever:   "info",
			wantScope:   "node-a→node-b",
			wantSummary: "tcp diagnostic node-a→node-b dispatched",
			wantDetails: `{"taskId":"t-6","checkType":"tcp","state":"dispatched"}`,
		},
		{
			name: "diagnostic_progress timeout is warn",
			event: &pb.Event{Seq: 23, Timestamp: fixedTime(), Payload: &pb.Event_DiagnosticProgress{
				DiagnosticProgress: &pb.DiagnosticProgress{
					TaskId: "t-7", CheckType: "mtr", SourceNode: "node-a", DestinationNode: "node-b", State: "timeout",
				},
			}},
			wantID:      "23-1753400000000000000",
			wantType:    "diagnostic_progress",
			wantSever:   "warn",
			wantScope:   "node-a→node-b",
			wantSummary: "mtr diagnostic node-a→node-b timeout",
			wantDetails: `{"taskId":"t-7","checkType":"mtr","state":"timeout"}`,
		},
		{
			name: "diagnostic_progress error is warn",
			event: &pb.Event{Seq: 24, Timestamp: fixedTime(), Payload: &pb.Event_DiagnosticProgress{
				DiagnosticProgress: &pb.DiagnosticProgress{
					TaskId: "t-8", CheckType: "tcp", SourceNode: "node-a", DestinationNode: "node-b", State: "error",
				},
			}},
			wantID:      "24-1753400000000000000",
			wantType:    "diagnostic_progress",
			wantSever:   "warn",
			wantScope:   "node-a→node-b",
			wantSummary: "tcp diagnostic node-a→node-b error",
			wantDetails: `{"taskId":"t-8","checkType":"tcp","state":"error"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := events.ToLiveEvent(tc.event)
			if err != nil {
				t.Fatalf("ToLiveEvent: %v", err)
			}
			if got.ID != tc.wantID {
				t.Errorf("ID = %q, want %q", got.ID, tc.wantID)
			}
			if got.Seq != tc.event.GetSeq() {
				t.Errorf("Seq = %d, want %d", got.Seq, tc.event.GetSeq())
			}
			if got.Type != tc.wantType {
				t.Errorf("Type = %q, want %q", got.Type, tc.wantType)
			}
			if got.Severity != tc.wantSever {
				t.Errorf("Severity = %q, want %q", got.Severity, tc.wantSever)
			}
			if got.Scope != tc.wantScope {
				t.Errorf("Scope = %q, want %q", got.Scope, tc.wantScope)
			}
			if got.Summary != tc.wantSummary {
				t.Errorf("Summary = %q, want %q", got.Summary, tc.wantSummary)
			}
			if string(got.Details) != tc.wantDetails {
				t.Errorf("Details =\n  %s\nwant\n  %s", got.Details, tc.wantDetails)
			}
			if !got.Timestamp.Equal(time.Unix(0, fixedNanos).UTC()) {
				t.Errorf("Timestamp = %v, want %v", got.Timestamp, time.Unix(0, fixedNanos).UTC())
			}
		})
	}
}

func TestToLiveEventRejectsNilAndUnknownPayload(t *testing.T) {
	tests := []struct {
		name  string
		event *pb.Event
	}{
		{name: "nil event", event: nil},
		{name: "no payload", event: &pb.Event{Seq: 1, Timestamp: fixedTime()}},
		// The reason the payload switch tests GetX() != nil instead of type
		// switching on GetPayload(): a wrapper carrying a nil inner message
		// satisfies a type switch and then panics on the first field read.
		{name: "check_observed wrapper with a nil inner message",
			event: &pb.Event{Seq: 2, Timestamp: fixedTime(), Payload: &pb.Event_CheckObserved{}}},
		{name: "topology_changed wrapper with a nil inner message",
			event: &pb.Event{Seq: 3, Timestamp: fixedTime(), Payload: &pb.Event_TopologyChanged{}}},
		{name: "mtr_completed wrapper with a nil inner message",
			event: &pb.Event{Seq: 4, Timestamp: fixedTime(), Payload: &pb.Event_MtrCompleted{}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := events.ToLiveEvent(tc.event)
			if !errors.Is(err, events.ErrUnknownPayload) {
				t.Errorf("error = %v, want ErrUnknownPayload", err)
			}
			// LiveEvent holds a json.RawMessage, so it is not comparable; check
			// the fields a partially-filled projection would have leaked.
			if got.ID != "" || got.Seq != 0 || got.Type != "" || got.Details != nil {
				t.Errorf("a rejected event must yield the zero LiveEvent, got %+v", got)
			}
		})
	}
}

// A missing controller timestamp must still produce a replica-stable id: the
// hub's dedupe key would be worthless if it came from a local clock.
func TestToLiveEventWithoutTimestampIsStillDeterministic(t *testing.T) {
	ev := &pb.Event{Seq: 9, Payload: &pb.Event_TopologyChanged{
		TopologyChanged: &pb.TopologyChanged{Reason: "zone_updated", NodeName: "node-a"},
	}}
	first, err := events.ToLiveEvent(ev)
	if err != nil {
		t.Fatalf("ToLiveEvent: %v", err)
	}
	second, err := events.ToLiveEvent(ev)
	if err != nil {
		t.Fatalf("ToLiveEvent: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("the same event produced two ids: %q and %q", first.ID, second.ID)
	}
	if first.ID != "9-0" {
		t.Errorf("ID = %q, want %q (epoch nanos, not a local clock)", first.ID, "9-0")
	}
}

// The wire shape is what web/src/lib/types.ts mirrors by hand; a renamed key is
// a silent frontend break, so the key set is asserted.
func TestLiveEventJSONKeys(t *testing.T) {
	live, err := events.ToLiveEvent(&pb.Event{Seq: 1, Timestamp: fixedTime(), Payload: &pb.Event_TopologyChanged{
		TopologyChanged: &pb.TopologyChanged{Reason: "agent_registered", NodeName: "node-a"},
	}})
	if err != nil {
		t.Fatalf("ToLiveEvent: %v", err)
	}
	raw, err := json.Marshal(live)
	if err != nil {
		t.Fatalf("marshal LiveEvent: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal LiveEvent: %v", err)
	}
	for _, key := range []string{"id", "seq", "type", "severity", "scope", "timestamp", "summary", "details"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("LiveEvent JSON is missing key %q; got %s", key, raw)
		}
	}
	if len(decoded) != 8 {
		t.Errorf("LiveEvent JSON has %d keys, want exactly 8: %s", len(decoded), raw)
	}
}

// subMicroNanos is a controller timestamp whose nanosecond part is NOT a whole
// number of microseconds (…000123456 ns → 123456 ns past the second, i.e. 456 ns
// past microsecond 123). Postgres TIMESTAMPTZ cannot hold those trailing 456 ns.
const subMicroNanos int64 = 1753400000000123456

// subMicroTruncatedNanos is what a µs-resolution store can hold, and therefore
// what ToLiveEvent must already have produced.
const subMicroTruncatedNanos int64 = 1753400000000123000

// storeRoundTrip models the ONE lossy step between ToLiveEvent and the scrollback endpoint.
func storeRoundTrip(ts time.Time) time.Time { return ts.UTC().Truncate(time.Microsecond) }

// rebuildID is httpapi.toLiveEvent's id expression (internal/console/httpapi/ events.go).
func rebuildID(seq uint64, ts time.Time) string {
	return fmt.Sprintf("%d-%d", seq, ts.UnixNano())
}

// A LiveEvent's id must survive persistence.
func TestToLiveEventIDSurvivesAMicrosecondStoreRoundTrip(t *testing.T) {
	ev := &pb.Event{
		Seq:       77,
		Timestamp: timestamppb.New(time.Unix(0, subMicroNanos).UTC()),
		Payload: &pb.Event_TopologyChanged{
			TopologyChanged: &pb.TopologyChanged{Reason: "agent_registered", NodeName: "node-a"},
		},
	}

	live, err := events.ToLiveEvent(ev)
	if err != nil {
		t.Fatalf("ToLiveEvent: %v", err)
	}

	// The strong form of the guarantee: the projection is ALREADY µs-aligned,
	// so ANY µs-resolution store is an identity on it, not just this one.
	if got := live.Timestamp.Nanosecond() % 1000; got != 0 {
		t.Errorf("Timestamp has %d sub-microsecond nanos; it must be truncated so a TIMESTAMPTZ round trip is lossless", got)
	}
	if want := time.Unix(0, subMicroTruncatedNanos).UTC(); !live.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %v, want %v", live.Timestamp, want)
	}
	if want := rebuildID(77, time.Unix(0, subMicroTruncatedNanos).UTC()); live.ID != want {
		t.Errorf("ID = %q, want %q", live.ID, want)
	}

	// And the round trip itself, end to end: write → read → rebuild.
	persisted := storeRoundTrip(live.Timestamp)
	if rebuilt := rebuildID(live.Seq, persisted); rebuilt != live.ID {
		t.Errorf("scrollback rebuilt id %q, want the live id %q — the Live page would render this event twice", rebuilt, live.ID)
	}
}

// The truncation must not disturb a timestamp that is already µs-aligned: the
// pinned ids elsewhere in this file depend on it being an exact no-op there.
func TestToLiveEventLeavesMicrosecondAlignedTimestampsAlone(t *testing.T) {
	live, err := events.ToLiveEvent(&pb.Event{Seq: 5, Timestamp: fixedTime(), Payload: &pb.Event_TopologyChanged{
		TopologyChanged: &pb.TopologyChanged{Reason: "agent_registered", NodeName: "node-a"},
	}})
	if err != nil {
		t.Fatalf("ToLiveEvent: %v", err)
	}
	if want := time.Unix(0, fixedNanos).UTC(); !live.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %v, want %v (truncation must be a no-op here)", live.Timestamp, want)
	}
	if want := rebuildID(5, time.Unix(0, fixedNanos).UTC()); live.ID != want {
		t.Errorf("ID = %q, want %q", live.ID, want)
	}
}

// TestNormalizePairScopeMirrorsTheClient pins the Go normalizer against web/src/lib/utils.ts
// normalizePairInput: the API contract must accept every form the console's own scope box does, or
// a direct consumer sending "a->b" gets 200 and an empty list.
func TestNormalizePairScopeMirrorsTheClient(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"canonical arrow is untouched", "node-a→node-b", "node-a→node-b"},
		{"ascii arrow", "node-a->node-b", "node-a→node-b"},
		{"long ascii arrow", "node-a-->node-b", "node-a→node-b"},
		{"fat arrow", "node-a=>node-b", "node-a→node-b"},
		{"long fat arrow", "node-a==>node-b", "node-a→node-b"},
		{"bare gt", "node-a>node-b", "node-a→node-b"},
		{"spaces around the arrow collapse", "node-a -> node-b", "node-a→node-b"},
		{"spaces around the canonical arrow collapse", "node-a → node-b", "node-a→node-b"},
		{"outer whitespace is trimmed", "  node-a->node-b  ", "node-a→node-b"},
		{"half written pair", "->node-b", "→node-b"},
		{"a single name is untouched", "edge-gw-01", "edge-gw-01"},
		{"a name with spaces stays one name", "ns/pod a&b", "ns/pod a&b"},
		{"a hyphen without gt stays a hyphen", "node-a-node-b", "node-a-node-b"},
		{"empty stays empty", "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := events.NormalizePairScope(tc.in); got != tc.want {
				t.Errorf("NormalizePairScope(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizePairScopeMatchesEmittedScopes closes the loop: what the normalizer produces must
// equal what pairScope writes into the column the filter compares against.
func TestNormalizePairScopeMatchesEmittedScopes(t *testing.T) {
	emitted := events.PairScope("node-a", "node-b")
	for _, typed := range []string{"node-a->node-b", "node-a → node-b", "node-a=>node-b"} {
		if got := events.NormalizePairScope(typed); got != emitted {
			t.Errorf("NormalizePairScope(%q) = %q, want the emitted scope %q", typed, got, emitted)
		}
	}
}
