package controller

import (
	"context"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/prometheus/client_golang/prometheus/testutil"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// newElectionController builds a controller with leader election on and nothing wired to an
// apiserver yet, which is the state every replica starts in.
func newElectionController(t *testing.T) *Controller {
	t.Helper()

	cfg := &config.Config{MetricsPrefix: "test"}
	cfg.Controller.AgentTTL = 30 * time.Second
	cfg.Controller.LeaderElection = true
	return New(cfg)
}

// testElectionOptions returns lease timings short enough for a unit test.
func testElectionOptions(client *fake.Clientset, identity string) *electionOptions {
	return &electionOptions{
		client:        client,
		namespace:     "kconmon",
		leaseName:     "kconmon-controller",
		identity:      identity,
		leaseDuration: 600 * time.Millisecond,
		renewDeadline: 400 * time.Millisecond,
		retryPeriod:   50 * time.Millisecond,
	}
}

// waitForLeadership polls until want is observed or the deadline passes.
func waitForLeadership(t *testing.T, c *Controller, want bool) {
	t.Helper()

	deadline := time.After(5 * time.Second)
	for c.IsLeader() != want {
		select {
		case <-deadline:
			t.Fatalf("IsLeader stayed %v, want %v", c.IsLeader(), want)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestNewDoesNotClaimLeadershipWhenElectionEnabled pins the split-brain regression: every replica
// declared itself leader at construction, so N replicas reported leader=1 and planned N probe
// meshes over N disjoint agent sets.
func TestNewDoesNotClaimLeadershipWhenElectionEnabled(t *testing.T) {
	c := newElectionController(t)

	if c.IsLeader() {
		t.Error("a replica with leader election on must not be leader before it holds the lease")
	}
	if got := testutil.ToFloat64(c.metrics.ControllerLeader.WithLabelValues()); got != 0 {
		t.Errorf("controller_leader = %v, want 0 before the lease is acquired", got)
	}
}

// TestNewClaimsLeadershipWithoutElection guards the single-replica default: with election off the
// replica is the only brain and must behave exactly as it did before.
func TestNewClaimsLeadershipWithoutElection(t *testing.T) {
	cfg := &config.Config{MetricsPrefix: "test"}
	cfg.Controller.AgentTTL = 30 * time.Second
	cfg.Controller.LeaderElection = false

	c := New(cfg)

	if !c.IsLeader() {
		t.Error("with leader election off the replica must be leader from construction")
	}
	if got := testutil.ToFloat64(c.metrics.ControllerLeader.WithLabelValues()); got != 1 {
		t.Errorf("controller_leader = %v, want 1 with leader election off", got)
	}
}

// TestRunLeaderElectionAcquiresLease asserts the winner both flips its own state and leaves the
// Lease behind under its identity, which is what the standby reads to stay a standby.
func TestRunLeaderElectionAcquiresLease(t *testing.T) {
	c := newElectionController(t)
	client := fake.NewClientset()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.runLeaderElection(ctx, testElectionOptions(client, "pod-a"))

	waitForLeadership(t, c, true)

	if got := testutil.ToFloat64(c.metrics.ControllerLeader.WithLabelValues()); got != 1 {
		t.Errorf("controller_leader = %v, want 1 once the lease is held", got)
	}

	lease, err := client.CoordinationV1().Leases("kconmon").Get(ctx, "kconmon-controller", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the elected leader left no Lease behind: %v", err)
	}
	if got := lease.Spec.HolderIdentity; got == nil || *got != "pod-a" {
		t.Errorf("lease holder = %v, want pod-a", got)
	}
}

// TestRunLeaderElectionYieldsToLiveLeaseHolder is the exclusivity guarantee: a second replica that
// finds an unexpired lease must stay a standby rather than declare itself a second leader.
func TestRunLeaderElectionYieldsToLiveLeaseHolder(t *testing.T) {
	holder := "pod-a"
	duration := int32(3600)
	now := metav1.NewMicroTime(time.Now())
	client := fake.NewClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "kconmon-controller", Namespace: "kconmon"},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &holder,
			LeaseDurationSeconds: &duration,
			AcquireTime:          &now,
			RenewTime:            &now,
		},
	})

	c := newElectionController(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.runLeaderElection(ctx, testElectionOptions(client, "pod-b"))

	// Several retry periods is long enough for a replica that was going to grab the lease to do so.
	deadline := time.After(time.Second)
	for {
		select {
		case <-deadline:
			return
		case <-time.After(20 * time.Millisecond):
		}
		if c.IsLeader() {
			t.Fatal("a replica became leader while another holds an unexpired lease (split brain)")
		}
		if got := testutil.ToFloat64(c.metrics.ControllerLeader.WithLabelValues()); got != 0 {
			t.Fatalf("standby reported controller_leader = %v, want 0", got)
		}
	}
}

// TestSetLeaderDemotionDropsRegistry covers the demoted-but-alive replica: agents it had accepted
// belong to the new leader now, and keeping them would keep it planning a second probe mesh.
func TestSetLeaderDemotionDropsRegistry(t *testing.T) {
	c := newElectionController(t)

	c.SetLeader(true)
	c.registry.Register(model.AgentInfo{ID: "agent-1", NodeName: "node-1"})
	c.registry.Register(model.AgentInfo{ID: "agent-2", NodeName: "node-2"})
	if c.registry.Count() != 2 {
		t.Fatalf("setup: registry holds %d agents, want 2", c.registry.Count())
	}

	c.SetLeader(false)

	if got := c.registry.Count(); got != 0 {
		t.Errorf("registry holds %d agents after demotion, want 0", got)
	}
	if got := testutil.ToFloat64(c.metrics.ControllerRegisteredAgents.WithLabelValues()); got != 0 {
		t.Errorf("registered_agents = %v after demotion, want 0", got)
	}
	if got := testutil.ToFloat64(c.metrics.ControllerLeader.WithLabelValues()); got != 0 {
		t.Errorf("controller_leader = %v after demotion, want 0", got)
	}
}
