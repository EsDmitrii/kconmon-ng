package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"google.golang.org/protobuf/proto"
)

// subscribePeers opens a WatchPeers stream for agentID and consumes the initial
// FULL_SYNC, so the caller knows the watcher is registered and parked in its
// select loop before broadcasting anything at it.
func subscribePeers(t *testing.T, srv *GRPCServer, agentID string) *fakePeerStream {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream := newFakePeerStream(ctx)
	go func() { _ = srv.WatchPeers(&pb.WatchPeersRequest{AgentId: agentID}, stream) }()

	select {
	case <-stream.sent:
	case <-time.After(3 * time.Second):
		t.Fatalf("WatchPeers(%s) never sent the initial full sync", agentID)
	}
	return stream
}

// drainPeerUpdates collects updates from the stream until it has been quiet for
// the given period. The period must comfortably exceed the broadcast window so
// a still-armed trailing flush cannot land after collection stops.
func drainPeerUpdates(stream *fakePeerStream, quiet time.Duration) []*pb.PeerUpdate {
	var got []*pb.PeerUpdate
	for {
		select {
		case u := <-stream.sent:
			got = append(got, u)
		case <-time.After(quiet):
			return got
		}
	}
}

/*
TestSchedulePeerBroadcastCoalescesABurst is the M9-3 fan-out claim: during a rollout N registry
changes arrive as a burst and each used to trigger its own O(N) broadcast — M rapid changes must
now produce FAR fewer than M broadcasts, while the FINAL topology still reaches every watcher.
Collapsing is safe because every update is a FULL_SYNC applied by wholesale replacement.
*/
func TestSchedulePeerBroadcastCoalescesABurst(t *testing.T) {
	srv, _ := newTestGRPCServer()
	srv.peerBroadcastWindow = 20 * time.Millisecond

	watcherA := subscribePeers(t, srv, "watcher-a")
	watcherB := subscribePeers(t, srv, "watcher-b")

	const changes = 40
	agents := make([]model.AgentInfo, 0, changes)
	for i := 0; i < changes; i++ {
		agents = append(agents, model.AgentInfo{
			ID:       fmt.Sprintf("agent-%02d", i),
			NodeName: fmt.Sprintf("node-%02d", i),
			PodIP:    fmt.Sprintf("10.0.0.%d", i+1),
			Zone:     "z1",
		})
		// Each call is one registry change publishing its own snapshot, exactly
		// as the OnChange chain does during a registration burst.
		snapshot := make([]model.AgentInfo, len(agents))
		copy(snapshot, agents)
		srv.SchedulePeerBroadcast(snapshot)
	}

	quiet := 10 * srv.peerBroadcastWindow
	for name, stream := range map[string]*fakePeerStream{"watcher-a": watcherA, "watcher-b": watcherB} {
		got := drainPeerUpdates(stream, quiet)
		if len(got) == 0 {
			t.Fatalf("%s: the burst produced no broadcast at all", name)
		}
		// "Far fewer": a factor of 4 is the loosest bound worth pinning; in
		// practice a sub-window burst collapses to 1.
		if len(got) >= changes/4 {
			t.Errorf("%s: %d changes produced %d broadcasts, want far fewer", name, changes, len(got))
		}
		t.Logf("%s: %d rapid changes -> %d broadcast(s)", name, changes, len(got))

		final := got[len(got)-1].GetPeers()
		if len(final) != changes {
			t.Fatalf("%s: final FULL_SYNC lists %d peers, want %d", name, len(final), changes)
		}
		for i, p := range final {
			if want := fmt.Sprintf("agent-%02d", i); p.GetId() != want {
				t.Fatalf("%s: final peer[%d] = %q, want %q", name, i, p.GetId(), want)
			}
		}
	}
}

// TestSchedulePeerBroadcastFiresAgainAfterTheWindow pins the throttle shape: the trailing timer is
// NOT reset by later arrivals, so sustained churn still broadcasts once per window instead of
// starving forever the way a resetting debounce would.
func TestSchedulePeerBroadcastFiresAgainAfterTheWindow(t *testing.T) {
	srv, _ := newTestGRPCServer()
	// The window sizes both drains (quiet = 10 windows) and the exact-count assertions in them: a
	// flush that lands after its drain leaks into the next one and fails both. 15ms left ~135ms for
	// the flush goroutine to be scheduled — CI-sized margins need hundreds.
	srv.peerBroadcastWindow = 50 * time.Millisecond

	stream := subscribePeers(t, srv, "watcher")

	first := []model.AgentInfo{{ID: "agent-1", NodeName: "node-1", PodIP: "10.0.0.1"}}
	srv.SchedulePeerBroadcast(first)

	got := drainPeerUpdates(stream, 10*srv.peerBroadcastWindow)
	if len(got) != 1 || len(got[0].GetPeers()) != 1 {
		t.Fatalf("expected exactly one 1-peer broadcast from the first change, got %d", len(got))
	}

	// A change in a LATER window must arm a new flush, not be swallowed by the spent one.
	second := []model.AgentInfo{
		{ID: "agent-1", NodeName: "node-1", PodIP: "10.0.0.1"},
		{ID: "agent-2", NodeName: "node-2", PodIP: "10.0.0.2"},
	}
	srv.SchedulePeerBroadcast(second)

	got = drainPeerUpdates(stream, 10*srv.peerBroadcastWindow)
	if len(got) != 1 || len(got[0].GetPeers()) != 2 {
		t.Fatalf("expected exactly one 2-peer broadcast from the second change, got %+v", got)
	}
}

// TestControllerRegistrationBurstCoalescesFanOut proves the registry.OnChange->fan-out chain wired
// in New actually routes through the coalescer: a burst of registrations reaches a watcher as far
// fewer FULL_SYNCs than registrations, and the last one holds the whole fleet.
func TestControllerRegistrationBurstCoalescesFanOut(t *testing.T) {
	cfg := &config.Config{MetricsPrefix: "test"}
	cfg.Controller.AgentTTL = time.Minute
	cfg.Controller.LeaderElection = false

	c := New(cfg)
	c.grpcServer.peerBroadcastWindow = 20 * time.Millisecond

	stream := subscribePeers(t, c.grpcServer, "watcher")

	const registrations = 30
	for i := 0; i < registrations; i++ {
		c.registry.Register(model.AgentInfo{
			ID:       fmt.Sprintf("agent-%02d", i),
			NodeName: fmt.Sprintf("node-%02d", i),
			PodIP:    fmt.Sprintf("10.0.1.%d", i+1),
		})
	}

	got := drainPeerUpdates(stream, 10*c.grpcServer.peerBroadcastWindow)
	if len(got) == 0 {
		t.Fatal("the registration burst never reached the watcher")
	}
	if len(got) >= registrations/4 {
		t.Errorf("%d registrations produced %d broadcasts, want far fewer", registrations, len(got))
	}
	t.Logf("%d registrations -> %d broadcast(s)", registrations, len(got))

	if final := got[len(got)-1].GetPeers(); len(final) != registrations {
		t.Fatalf("final FULL_SYNC lists %d peers, want %d", len(final), registrations)
	}
}

// TestBroadcastPeerUpdateBuildsEachPeerProtoOnce pins the O(N^2)-conversions fix: within one
// broadcast the proto for a given agent is built once and SHARED by every watcher's filtered list
// (gRPC marshals without mutating, so sharing is safe).
func TestBroadcastPeerUpdateBuildsEachPeerProtoOnce(t *testing.T) {
	srv, _ := newTestGRPCServer()

	watcherA := subscribePeers(t, srv, "watcher-a")
	watcherB := subscribePeers(t, srv, "watcher-b")

	srv.BroadcastPeerUpdate([]model.AgentInfo{
		{ID: "watcher-a", NodeName: "node-a", PodIP: "10.0.0.1"},
		{ID: "watcher-b", NodeName: "node-b", PodIP: "10.0.0.2"},
		{ID: "agent-3", NodeName: "node-3", PodIP: "10.0.0.3"},
	})

	find := func(t *testing.T, stream *fakePeerStream, id string) *pb.AgentMeta {
		t.Helper()
		select {
		case u := <-stream.sent:
			for _, p := range u.GetPeers() {
				if p.GetId() == id {
					return p
				}
			}
			t.Fatalf("peer %s missing from the update: %+v", id, u.GetPeers())
		case <-time.After(3 * time.Second):
			t.Fatal("broadcast never arrived")
		}
		return nil
	}

	inA := find(t, watcherA, "agent-3")
	inB := find(t, watcherB, "agent-3")
	if inA != inB {
		t.Fatal("agent-3's proto was built per watcher; one broadcast must build each peer once and share it")
	}
}

// TestPeerListsCarryTheNarrowProjection: the agent's protoToTargets reads exactly id, node_name,
// pod_ip and zone from a peer, so every peer LIST the controller emits (Register response peers,
// the WatchPeers initial FULL_SYNC, broadcast FULL_SYNCs) omits pod_name, labels and capabilities.
// The agent's OWN resolved meta on RegisterResponse.Agent stays full — the agent reads its zone
// from it and the capability round-trip is pinned elsewhere.
func TestPeerListsCarryTheNarrowProjection(t *testing.T) {
	srv, _ := newTestGRPCServer()

	wide := func(i int) *pb.AgentMeta {
		return &pb.AgentMeta{
			Id:           fmt.Sprintf("agent-%d", i),
			NodeName:     fmt.Sprintf("node-%d", i),
			PodName:      fmt.Sprintf("kconmon-%d", i),
			PodIp:        fmt.Sprintf("10.0.0.%d", i),
			Zone:         "z1",
			Labels:       map[string]string{"role": "worker"},
			Capabilities: []string{"external-checks"},
		}
	}

	assertNarrow := func(t *testing.T, where string, peers []*pb.AgentMeta) {
		t.Helper()
		if len(peers) == 0 {
			t.Fatalf("%s: no peers to inspect", where)
		}
		for _, p := range peers {
			if p.GetId() == "" || p.GetNodeName() == "" || p.GetPodIp() == "" || p.GetZone() == "" {
				t.Errorf("%s: a field the agent reads is missing: %+v", where, p)
			}
			if p.GetPodName() != "" || len(p.GetLabels()) != 0 || len(p.GetCapabilities()) != 0 {
				t.Errorf("%s: peer %s carries fields no agent reads: %+v", where, p.GetId(), p)
			}
		}
	}

	if _, err := srv.Register(context.Background(), &pb.RegisterRequest{Agent: wide(1)}); err != nil {
		t.Fatalf("register agent-1: %v", err)
	}
	resp, err := srv.Register(context.Background(), &pb.RegisterRequest{Agent: wide(2)})
	if err != nil {
		t.Fatalf("register agent-2: %v", err)
	}
	assertNarrow(t, "Register response peers", resp.GetPeers())
	if got := resp.GetAgent(); got.GetPodName() == "" || len(got.GetCapabilities()) == 0 {
		t.Errorf("RegisterResponse.Agent must stay the FULL projection, got %+v", got)
	}

	stream := subscribePeersRaw(t, srv, "agent-2")
	select {
	case u := <-stream.sent:
		assertNarrow(t, "WatchPeers initial FULL_SYNC", u.GetPeers())
	case <-time.After(3 * time.Second):
		t.Fatal("WatchPeers never sent the initial full sync")
	}

	srv.BroadcastPeerUpdate([]model.AgentInfo{
		{
			ID: "agent-1", NodeName: "node-1", PodName: "kconmon-1", PodIP: "10.0.0.1",
			Zone: "z1", Labels: map[string]string{"role": "worker"}, Capabilities: []string{"external-checks"},
		},
		{
			ID: "agent-3", NodeName: "node-3", PodName: "kconmon-3", PodIP: "10.0.0.3",
			Zone: "z1", Labels: map[string]string{"role": "worker"}, Capabilities: []string{"external-checks"},
		},
	})
	select {
	case u := <-stream.sent:
		assertNarrow(t, "broadcast FULL_SYNC", u.GetPeers())
	case <-time.After(3 * time.Second):
		t.Fatal("broadcast never arrived")
	}
}

// subscribePeersRaw opens a WatchPeers stream WITHOUT consuming the initial FULL_SYNC.
func subscribePeersRaw(t *testing.T, srv *GRPCServer, agentID string) *fakePeerStream {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream := newFakePeerStream(ctx)
	go func() { _ = srv.WatchPeers(&pb.WatchPeersRequest{AgentId: agentID}, stream) }()

	deadline := time.After(3 * time.Second)
	for {
		srv.mu.RLock()
		_, subscribed := srv.watchers[agentID]
		srv.mu.RUnlock()
		if subscribed {
			return stream
		}
		select {
		case <-deadline:
			t.Fatalf("WatchPeers(%s) never subscribed", agentID)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// TestNarrowPeerProjectionWireCompat follows api/proto/compat_test.go's pattern: the narrow
// projection is plain field omission in proto3, so its wire bytes are exactly a full AgentMeta
// with those fields empty, both directions unmarshal cleanly, and round trips are byte-stable —
// no wire-format break for an old agent OR an old controller.
func TestNarrowPeerProjectionWireCompat(t *testing.T) {
	info := model.AgentInfo{
		ID: "a-1", NodeName: "node-a", PodName: "kconmon-abc", PodIP: "10.0.0.7",
		Zone: "z1", Labels: map[string]string{"role": "worker"}, Capabilities: []string{"external-checks"},
	}

	narrowRaw, err := proto.Marshal(peerToProto(info))
	if err != nil {
		t.Fatalf("marshal narrow: %v", err)
	}
	handRolled, err := proto.Marshal(&pb.AgentMeta{
		Id: "a-1", NodeName: "node-a", PodIp: "10.0.0.7", Zone: "z1",
	})
	if err != nil {
		t.Fatalf("marshal hand-rolled: %v", err)
	}
	if string(narrowRaw) != string(handRolled) {
		t.Fatalf("narrow projection is not plain field omission:\n narrow=%x\nrolled=%x", narrowRaw, handRolled)
	}

	// A new (narrowed) controller's peer decodes on any agent with the four read fields intact
	// and nothing materializing out of the omitted ones.
	var decoded pb.AgentMeta
	if err = proto.Unmarshal(narrowRaw, &decoded); err != nil {
		t.Fatalf("unmarshal narrow: %v", err)
	}
	if decoded.GetId() != "a-1" || decoded.GetNodeName() != "node-a" ||
		decoded.GetPodIp() != "10.0.0.7" || decoded.GetZone() != "z1" {
		t.Fatalf("a field the agent reads was lost: %+v", &decoded)
	}
	if decoded.GetPodName() != "" || len(decoded.GetLabels()) != 0 || len(decoded.GetCapabilities()) != 0 {
		t.Fatalf("omitted fields materialized out of a narrow message: %+v", &decoded)
	}
	again, err := proto.Marshal(&decoded)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(again) != string(narrowRaw) {
		t.Fatal("narrow message wire bytes changed across a round trip")
	}

	// And an OLD controller's full-projection peer still decodes with those same four fields —
	// the agent code path is identical either way.
	fullRaw, err := proto.Marshal(agentInfoToProto(info))
	if err != nil {
		t.Fatalf("marshal full: %v", err)
	}
	var fullDecoded pb.AgentMeta
	if err := proto.Unmarshal(fullRaw, &fullDecoded); err != nil {
		t.Fatalf("unmarshal full: %v", err)
	}
	if fullDecoded.GetId() != "a-1" || fullDecoded.GetNodeName() != "node-a" ||
		fullDecoded.GetPodIp() != "10.0.0.7" || fullDecoded.GetZone() != "z1" {
		t.Fatalf("full projection lost an agent-read field: %+v", &fullDecoded)
	}
}
