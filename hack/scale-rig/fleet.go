package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/agent"
	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// fleetAgentInfo builds the identity a fleet member asserts, mirroring resolveIdentity's contract
// (ID = nodeName-podName, IP must parse). IPs live in 10.128/10.129+ so fleet and probe ranges can
// never collide.
func fleetAgentInfo(idx int) model.AgentInfo {
	node := fmt.Sprintf("scale-node-%05d", idx)
	pod := fmt.Sprintf("kconmon-agent-%05d-r0", idx)
	return model.AgentInfo{
		ID:       node + "-" + pod,
		NodeName: node,
		PodName:  pod,
		PodIP:    rigIP(idx),
		// Three zones, as a typical production topology spreads a fleet.
		Zone:   fmt.Sprintf("zone-%d", idx%3),
		Labels: map[string]string{"app.kubernetes.io/name": "kconmon-ng", "rig": "scale"},
	}
}

// probeAgentInfo is a propagation-probe registration: a real agent to the controller, but the rig
// never subscribes or heartbeats it. Offset keeps its IP range disjoint from the fleet's.
func probeAgentInfo(p int) model.AgentInfo {
	node := fmt.Sprintf("probe-node-%05d", p)
	pod := fmt.Sprintf("probe-pod-%05d", p)
	return model.AgentInfo{
		ID:       node + "-" + pod,
		NodeName: node,
		PodName:  pod,
		PodIP:    rigIP(1<<17 + p),
		Zone:     "zone-probe",
	}
}

// restartedAgentInfo is the identity after a simulated pod replacement: same node, new pod name and
// therefore a new agent ID, exactly what a DaemonSet rollout produces. The IP is kept stable so the
// rig's uniqueness bookkeeping stays trivial; the controller does not key anything by IP.
func restartedAgentInfo(base model.AgentInfo, idx, restarts int) model.AgentInfo { //nolint:gocritic // hugeParam: value copy intended, the result is a new identity
	base.PodName = fmt.Sprintf("kconmon-agent-%05d-r%d", idx, restarts)
	base.ID = base.NodeName + "-" + base.PodName
	return base
}

func rigIP(idx int) string {
	return fmt.Sprintf("10.%d.%d.%d", 128+(idx>>16), (idx>>8)&255, idx&255)
}

/*
rigAgent drives ONE real agent gRPC client the way internal/agent.Run does — dial, Register (with
backoff and redial-on-Unavailable), a WatchPeers loop that re-registers on disconnect, and the real
StartHeartbeat loop — without the probe schedulers and sockets. The rig measures the control plane,
so the peer callback only records coverage and counts.
*/
type rigAgent struct {
	rig *Rig
	idx int

	mu       sync.Mutex
	info     model.AgentInfo
	client   *agent.GRPCClient
	cancel   context.CancelFunc
	cursor   int
	restarts int
}

func newRigAgent(r *Rig, idx int) *rigAgent {
	return &rigAgent{rig: r, idx: idx, info: fleetAgentInfo(idx)}
}

// launch dials a fresh client, registers, and starts the watch and heartbeat loops under a
// sub-context the next restart cancels. Returns only when the first registration succeeded or the
// context died.
func (a *rigAgent) launch(ctx context.Context, firstEver bool) {
	client, err := agent.NewGRPCClient(a.rig.grpcAddr, agent.ClientSecurity{})
	if err != nil {
		a.rig.counters.registerFail.Add(1)
		return
	}
	client.OnPeersUpdate(a.onPeers)

	// Mirror the real agent: a heartbeat NotFound triggers re-registration.
	reregCh := make(chan struct{}, 1)
	client.OnNeedReregister(func() {
		select {
		case reregCh <- struct{}{}:
		default:
		}
	})

	sub, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	a.client = client
	a.cancel = cancel
	info := a.info
	a.mu.Unlock()

	if !a.registerWithRetry(sub, client, info) {
		cancel()
		_ = client.Close()
		a.rig.counters.registerFail.Add(1)
		return
	}
	if firstEver {
		a.rig.counters.activeAgents.Add(1)
	}

	go a.watchLoop(sub, client)
	go client.StartHeartbeat(sub, a.rig.cfg.HeartbeatInterval)
	go func() {
		for {
			select {
			case <-sub.Done():
				return
			case <-reregCh:
				a.rig.counters.reregEntries.Add(1)
				a.reregisterLoop(sub, client)
			}
		}
	}()
}

// registerWithRetry mirrors the agent's startup registration: bounded attempts with exponential
// backoff, redialling on Unavailable so a fresh connection gets load-balanced again.
func (a *rigAgent) registerWithRetry(ctx context.Context, client *agent.GRPCClient, info model.AgentInfo) bool { //nolint:gocritic // hugeParam: value copy intended
	backoff := time.Second
	const maxBackoff = 15 * time.Second
	for {
		attempt := time.Now()
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, _, err := client.Register(rctx, info, a.rig.httpPort)
		cancel()
		if err == nil {
			a.rig.counters.registerOK.Add(1)
			a.rig.regLat.add(time.Since(attempt))
			return true
		}
		if grpcstatus.Code(err) == codes.InvalidArgument {
			// The payload itself is refused; retrying can never succeed. A rig bug, not load.
			return false
		}
		a.rig.counters.registerRetry.Add(1)
		if grpcstatus.Code(err) == codes.Unavailable {
			_ = client.Reconnect()
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// watchLoop mirrors internal/agent.Run's peer-watch goroutine: a dead stream means the agent must
// re-register (its desync recovery), then re-subscribe.
func (a *rigAgent) watchLoop(ctx context.Context, client *agent.GRPCClient) {
	for {
		_ = client.WatchPeers(ctx, a.rig.httpPort)
		if ctx.Err() != nil {
			return
		}
		a.rig.counters.watchResubs.Add(1)
		a.rig.counters.reregEntries.Add(1)
		a.reregisterLoop(ctx, client)
		if ctx.Err() != nil {
			return
		}
	}
}

// reregisterLoop mirrors the agent's reregister(): wait, register, redial on Unavailable, back off.
// Deterministic jitter (by agent index) stands in for the real rand jitter; the spread it exists
// for is preserved.
func (a *rigAgent) reregisterLoop(ctx context.Context, client *agent.GRPCClient) {
	wait := 2 * time.Second
	const maxWait = 30 * time.Second
	for {
		jitter := time.Duration(a.idx%500) * time.Millisecond
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait + jitter):
		}
		a.mu.Lock()
		info := a.info
		a.mu.Unlock()
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, _, err := client.Register(rctx, info, a.rig.httpPort)
		cancel()
		if err == nil {
			a.rig.counters.registerOK.Add(1)
			return
		}
		a.rig.counters.registerRetry.Add(1)
		if grpcstatus.Code(err) == codes.Unavailable {
			_ = client.Reconnect()
		}
		wait = min(wait*2, maxWait)
	}
}

// onPeers is the whole agent-side measurement: count the callback and let the tracker advance this
// agent's probe cursor by the received list's length. O(1) per probe, no per-peer work.
func (a *rigAgent) onPeers(targets []checker.Target) {
	a.rig.counters.peerCallbacks.Add(1)
	now := time.Now()
	a.mu.Lock()
	a.rig.tracker.cover(&a.cursor, len(targets), now)
	a.mu.Unlock()
}

// restart simulates the agent's pod being replaced: stop the loops, deregister gracefully, close
// the transport, then come back with a new pod identity — the rolling-churn event the controller
// has to absorb.
func (a *rigAgent) restart(ctx context.Context) {
	a.mu.Lock()
	cancel, client := a.cancel, a.client
	a.restarts++
	a.info = restartedAgentInfo(a.info, a.idx, a.restarts)
	a.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if client != nil {
		dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
		if err := client.Deregister(dctx); err == nil {
			a.rig.counters.deregisterOK.Add(1)
		}
		dcancel()
		_ = client.Close()
	}
	a.launch(ctx, false)
}

// stop tears the agent down at the end of the run (no dereg: the process exits anyway).
func (a *rigAgent) stop() {
	a.mu.Lock()
	cancel, client := a.cancel, a.client
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if client != nil {
		_ = client.Close()
	}
}
