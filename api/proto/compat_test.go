package proto_test

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
)

func TestTaskRequestM3ShapeRoundTripsWithoutNewFields(t *testing.T) {
	m3 := &pb.TaskRequest{
		TaskId:    "t-1",
		CheckType: "tcp",
		Target: &pb.AgentMeta{
			Id:       "a-1",
			NodeName: "node-a",
			PodName:  "kconmon-abc",
			PodIp:    "10.0.0.7",
			Zone:     "z1",
			Labels:   map[string]string{"role": "worker"},
		},
		Plane: "pod",
	}

	first, err := proto.Marshal(m3)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded pb.TaskRequest
	if err = proto.Unmarshal(first, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.GetExternalTarget() != nil {
		t.Fatal("external_target materialized out of an M3-shaped message")
	}
	if len(decoded.GetTarget().GetCapabilities()) != 0 {
		t.Fatalf("capabilities materialized out of an M3-shaped message: %v",
			decoded.GetTarget().GetCapabilities())
	}

	second, err := proto.Marshal(&decoded)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("round trip changed the wire bytes:\n first=%x\nsecond=%x", first, second)
	}

	// Field 5 of TaskRequest (external_target, wire type 2) would appear as tag byte 0x2a.
	if got, want := proto.Size(m3), len(first); got != want {
		t.Fatalf("proto.Size disagrees with marshal length: %d != %d", got, want)
	}
}

// TestOldAgentMetaUnmarshalsWithEmptyCapabilities proves the other direction of the same guarantee;
// an old agent can never accidentally advertise a capability.
func TestOldAgentMetaUnmarshalsWithEmptyCapabilities(t *testing.T) {
	old := &pb.AgentMeta{Id: "a-1", NodeName: "node-a", Zone: "z1"}
	raw, err := proto.Marshal(old)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded pb.AgentMeta
	if err := proto.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := decoded.GetCapabilities(); len(got) != 0 {
		t.Fatalf("expected no capabilities, got %v", got)
	}
}

// TestTaskRequestBothTargetsSetIsDetectable proves the mutual exclusion the schema deliberately
// does NOT enforce (no oneof -- see kconmon.proto's comment on external_target) is enforceable in
// code.
func TestTaskRequestBothTargetsSetIsDetectable(t *testing.T) {
	malformed := &pb.TaskRequest{
		TaskId:         "t-2",
		CheckType:      "http",
		Target:         &pb.AgentMeta{Id: "a-1", NodeName: "node-a"},
		ExternalTarget: &pb.ExternalTarget{Name: "corp-dns", Kind: "host", Address: "10.66.6.6", Port: 53},
		Plane:          "pod",
	}

	raw, err := proto.Marshal(malformed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded pb.TaskRequest
	if err := proto.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.GetTarget() == nil || decoded.GetExternalTarget() == nil {
		t.Fatalf("both-set request lost a side in transit: target=%v external=%v",
			decoded.GetTarget(), decoded.GetExternalTarget())
	}
	if !proto.Equal(malformed, &decoded) {
		t.Fatal("both-set request did not round-trip equal")
	}
}

func TestExistingMessagesSurviveTheWatchExternalChecksAddition(t *testing.T) {
	msgs := []proto.Message{
		&pb.PeerUpdate{
			Type:  pb.PeerUpdate_FULL_SYNC,
			Peers: []*pb.AgentMeta{{Id: "a-1", NodeName: "node-a", Zone: "z1"}},
		},
		&pb.TaskResult{
			TaskId: "t-1", AgentId: "a-1", Success: true, DetailsJson: []byte(`{"ok":true}`),
		},
	}
	for _, m := range msgs {
		raw, err := proto.Marshal(m)
		if err != nil {
			t.Fatalf("marshal %T: %v", m, err)
		}
		clone := m.ProtoReflect().New().Interface()
		if err = proto.Unmarshal(raw, clone); err != nil {
			t.Fatalf("unmarshal %T: %v", m, err)
		}
		again, err := proto.Marshal(clone)
		if err != nil {
			t.Fatalf("re-marshal %T: %v", m, err)
		}
		if !bytes.Equal(raw, again) {
			t.Errorf("%T wire bytes changed across a round trip", m)
		}
		if proto.Size(m) != len(raw) {
			t.Errorf("%T proto.Size disagrees with marshal length", m)
		}
	}
}

// TestExternalCheckAssignmentIsAbsoluteAndTyped pins the new messages' own
// shape: a two-spec assignment round-trips proto.Equal, and check_type/params
// survive as authored.
func TestExternalCheckAssignmentIsAbsoluteAndTyped(t *testing.T) {
	a := &pb.ExternalCheckAssignment{
		Specs: []*pb.ExternalCheckSpec{
			{
				DefinitionId: "d-1",
				Target:       &pb.ExternalTarget{Name: "corp-dns", Kind: "host", Address: "10.66.6.6", Port: 53},
				CheckType:    "tcp",
				IntervalNs:   int64(30 * 1e9),
				TimeoutNs:    int64(5 * 1e9),
				ParamsJson:   []byte(`{}`),
			},
			{DefinitionId: "d-2", CheckType: "icmp"},
		},
	}
	raw, err := proto.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded pb.ExternalCheckAssignment
	if err = proto.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !proto.Equal(a, &decoded) {
		t.Fatal("assignment did not round-trip equal")
	}
}
