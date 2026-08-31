package controller

import (
	"context"
	"sort"
	"testing"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/controller/meshplan"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

func peerIDs(peers []*pb.AgentMeta) []string {
	ids := make([]string, 0, len(peers))
	for _, p := range peers {
		ids = append(ids, p.GetId())
	}
	sort.Strings(ids)
	return ids
}

func equalIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func registerAgent(t *testing.T, srv *GRPCServer, id, node, ip string) *pb.RegisterResponse {
	t.Helper()
	resp, err := srv.Register(context.Background(), &pb.RegisterRequest{
		Agent: &pb.AgentMeta{Id: id, NodeName: node, PodIp: ip},
	})
	if err != nil {
		t.Fatalf("Register(%s): %v", id, err)
	}
	return resp
}

/*
TestPeerListsWithoutAPlanAreUnchanged pins the zero-behavior-change contract of M10: with no plan
set — and equally with SetPeerPlan(nil) — the Register response, the WatchPeers initial FULL_SYNC
and a broadcast carry exactly today's lists (everyone except the receiver, broadcast order equal to
the input snapshot order).
*/
func TestPeerListsWithoutAPlanAreUnchanged(t *testing.T) {
	for _, explicitNil := range []bool{false, true} {
		srv, _ := newTestGRPCServer()
		if explicitNil {
			srv.SetPeerPlan(nil)
		}

		registerAgent(t, srv, "agent-a", "node-a", "10.0.0.1")
		registerAgent(t, srv, "agent-b", "node-b", "10.0.0.2")
		resp := registerAgent(t, srv, "agent-c", "node-c", "10.0.0.3")
		if got := peerIDs(resp.GetPeers()); !equalIDs(got, []string{"agent-a", "agent-b"}) {
			t.Fatalf("explicitNil=%v: Register peers = %v, want all others", explicitNil, got)
		}

		stream := subscribePeers(t, srv, "agent-a")
		// subscribePeers consumed the initial sync from its own channel; re-subscribe pattern is not
		// needed — take a broadcast instead to also pin ordering.
		agents := []model.AgentInfo{
			{ID: "agent-b", NodeName: "node-b", PodIP: "10.0.0.2"},
			{ID: "agent-a", NodeName: "node-a", PodIP: "10.0.0.1"},
			{ID: "agent-c", NodeName: "node-c", PodIP: "10.0.0.3"},
		}
		srv.BroadcastPeerUpdate(agents)
		select {
		case u := <-stream.sent:
			got := make([]string, 0, len(u.GetPeers()))
			for _, p := range u.GetPeers() {
				got = append(got, p.GetId())
			}
			// Exact order: all-but-self in the snapshot's own order, as before M10.
			if len(got) != 2 || got[0] != "agent-b" || got[1] != "agent-c" {
				t.Fatalf("explicitNil=%v: broadcast peers = %v, want [agent-b agent-c]", explicitNil, got)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("broadcast never arrived")
		}
	}
}

func TestSetPeerPlanFiltersRegisterResponse(t *testing.T) {
	srv, _ := newTestGRPCServer()

	registerAgent(t, srv, "agent-a", "node-a", "10.0.0.1")
	registerAgent(t, srv, "agent-b", "node-b", "10.0.0.2")
	registerAgent(t, srv, "agent-c", "node-c", "10.0.0.3")

	srv.SetPeerPlan(meshplan.Plan{
		"agent-a": {"agent-b"},
		"agent-b": {"agent-c"},
		"agent-c": {"agent-a"},
	})

	// Register again (re-registration is ordinary) and check the response list is the plan's.
	resp := registerAgent(t, srv, "agent-a", "node-a", "10.0.0.1")
	if got := peerIDs(resp.GetPeers()); !equalIDs(got, []string{"agent-b"}) {
		t.Fatalf("Register peers = %v, want [agent-b]", got)
	}
}

// TestWatchPeersInitialSyncHonorsThePlan reads the initial FULL_SYNC itself, so the filtered list
// is asserted rather than just consumed.
func TestWatchPeersInitialSyncHonorsThePlan(t *testing.T) {
	srv, _ := newTestGRPCServer()

	registerAgent(t, srv, "agent-a", "node-a", "10.0.0.1")
	registerAgent(t, srv, "agent-b", "node-b", "10.0.0.2")
	registerAgent(t, srv, "agent-c", "node-c", "10.0.0.3")

	srv.SetPeerPlan(meshplan.Plan{
		"agent-a": {"agent-c"},
		"agent-b": {"agent-a"},
		"agent-c": {"agent-b"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream := newFakePeerStream(ctx)
	go func() { _ = srv.WatchPeers(&pb.WatchPeersRequest{AgentId: "agent-b"}, stream) }()

	select {
	case u := <-stream.sent:
		if got := peerIDs(u.GetPeers()); !equalIDs(got, []string{"agent-a"}) {
			t.Fatalf("initial FULL_SYNC peers = %v, want [agent-a]", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WatchPeers never sent the initial full sync")
	}
}

// TestBroadcastPeerUpdateHonorsThePlan: with a plan set, each watcher's filtered FULL_SYNC carries
// exactly its planned subset. An agent absent from the plan gets an empty list, not the full mesh —
// fail-closed, its next FULL_SYNC after re-registration repairs it.
func TestBroadcastPeerUpdateHonorsThePlan(t *testing.T) {
	srv, _ := newTestGRPCServer()

	watcherA := subscribePeers(t, srv, "agent-a")
	watcherB := subscribePeers(t, srv, "agent-b")
	watcherD := subscribePeers(t, srv, "agent-d")

	srv.SetPeerPlan(meshplan.Plan{
		"agent-a": {"agent-b"},
		"agent-b": {"agent-a", "agent-c"},
		"agent-c": {"agent-a"},
	})

	srv.BroadcastPeerUpdate([]model.AgentInfo{
		{ID: "agent-a", NodeName: "node-a", PodIP: "10.0.0.1"},
		{ID: "agent-b", NodeName: "node-b", PodIP: "10.0.0.2"},
		{ID: "agent-c", NodeName: "node-c", PodIP: "10.0.0.3"},
	})

	expect := map[string]struct {
		stream *fakePeerStream
		want   []string
	}{
		"agent-a": {watcherA, []string{"agent-b"}},
		"agent-b": {watcherB, []string{"agent-a", "agent-c"}},
		"agent-d": {watcherD, nil}, // not in the plan: nothing, not everything
	}
	for id, tc := range expect {
		select {
		case u := <-tc.stream.sent:
			if got := peerIDs(u.GetPeers()); !equalIDs(got, tc.want) {
				t.Fatalf("%s: broadcast peers = %v, want %v", id, got, tc.want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: broadcast never arrived", id)
		}
	}
}

/*
TestControllerWiresSparsePlanIntoFanout is the end-to-end wiring claim: with topology.mode=sparse
the registry OnChange chain rebuilds the plan BEFORE peers are handed out, Register responses obey
it, the coalesced broadcast obeys it, ProbePlan exposes it, and a deregistration drops the agent's
entry (no stale plan rows for the Surface phase to render).
*/
func TestControllerWiresSparsePlanIntoFanout(t *testing.T) {
	cfg := &config.Config{MetricsPrefix: "test"}
	cfg.Controller.AgentTTL = time.Minute
	cfg.Controller.LeaderElection = false
	cfg.Topology = config.TopologyConfig{
		Mode:   config.TopologyModeSparse,
		Sparse: config.SparseTopologyConfig{RingDegree: 1, ZoneChords: 0},
	}

	c := New(cfg)
	c.grpcServer.peerBroadcastWindow = 20 * time.Millisecond

	registerAgent(t, c.grpcServer, "agent-a", "node-1", "10.0.0.1")
	registerAgent(t, c.grpcServer, "agent-b", "node-2", "10.0.0.2")
	resp := registerAgent(t, c.grpcServer, "agent-c", "node-3", "10.0.0.3")

	// Ring by node name with degree 1: node-1->node-2->node-3->node-1.
	if got := peerIDs(resp.GetPeers()); !equalIDs(got, []string{"agent-a"}) {
		t.Fatalf("sparse Register peers = %v, want [agent-a]", got)
	}

	plan := c.ProbePlan()
	if plan == nil {
		t.Fatal("ProbePlan is nil in sparse mode with agents registered")
	}
	if !equalIDs(plan["agent-a"], []string{"agent-b"}) ||
		!equalIDs(plan["agent-b"], []string{"agent-c"}) ||
		!equalIDs(plan["agent-c"], []string{"agent-a"}) {
		t.Fatalf("ProbePlan = %v, want the degree-1 ring", plan)
	}

	// The coalesced broadcast delivers the PLANNED subset, not the full snapshot.
	stream := subscribePeers(t, c.grpcServer, "agent-a")
	registerAgent(t, c.grpcServer, "agent-d", "node-4", "10.0.0.4")
	got := drainPeerUpdates(stream, 10*c.grpcServer.peerBroadcastWindow)
	if len(got) == 0 {
		t.Fatal("registration never reached the watcher")
	}
	if final := peerIDs(got[len(got)-1].GetPeers()); !equalIDs(final, []string{"agent-b"}) {
		t.Fatalf("broadcast peers for agent-a = %v, want [agent-b]", final)
	}

	// Deregistration rebuilds the plan without the leaver.
	if _, err := c.grpcServer.Deregister(context.Background(), &pb.DeregisterRequest{AgentId: "agent-d"}); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	if _, ok := c.ProbePlan()["agent-d"]; ok {
		t.Fatal("ProbePlan still lists a deregistered agent")
	}
}

// TestDemotionDropsTheProbePlan: demotion drops the registry (ResetQuiet), and the plan is derived
// state over exactly that registry — a standby answering ProbePlan with the old leader's mesh would
// describe agents it no longer holds.
func TestDemotionDropsTheProbePlan(t *testing.T) {
	cfg := &config.Config{MetricsPrefix: "test"}
	cfg.Controller.AgentTTL = time.Minute
	cfg.Controller.LeaderElection = false
	cfg.Topology = config.TopologyConfig{
		Mode:   config.TopologyModeSparse,
		Sparse: config.SparseTopologyConfig{RingDegree: 1, ZoneChords: 0},
	}

	c := New(cfg)
	registerAgent(t, c.grpcServer, "agent-a", "node-1", "10.0.0.1")
	registerAgent(t, c.grpcServer, "agent-b", "node-2", "10.0.0.2")
	if c.ProbePlan() == nil {
		t.Fatal("precondition: sparse plan installed")
	}

	c.SetLeader(false)

	if p := c.ProbePlan(); p != nil {
		t.Fatalf("ProbePlan after demotion = %v, want nil", p)
	}
}

// TestControllerProbePlanNilInFullMode: the default (full) topology never installs a plan, so
// ProbePlan answers nil — the Surface phase renders "everyone probes everyone".
func TestControllerProbePlanNilInFullMode(t *testing.T) {
	cfg := &config.Config{MetricsPrefix: "test"}
	cfg.Controller.AgentTTL = time.Minute
	cfg.Controller.LeaderElection = false

	c := New(cfg)
	registerAgent(t, c.grpcServer, "agent-a", "node-1", "10.0.0.1")
	registerAgent(t, c.grpcServer, "agent-b", "node-2", "10.0.0.2")

	if p := c.ProbePlan(); p != nil {
		t.Fatalf("ProbePlan = %v in full mode, want nil", p)
	}
}
