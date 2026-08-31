package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/controller/meshplan"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type GRPCServer struct {
	pb.UnimplementedAgentRegistryServer
	pb.UnimplementedEventStreamServer
	registry    *Registry
	metrics     *metrics.PrometheusMetrics
	taskMgr     *TaskManager
	externalMgr *ExternalCheckManager

	mu sync.RWMutex
	/* A SET of peer-update streams per agent id, not one per id.

	   The id on a WatchPeers request is client-supplied and nothing authenticates the gRPC surface,
	   so with one entry per id the map was last-writer-wins: a second subscriber under an existing
	   agent's id displaced that agent's mailbox, the agent's own stream goroutine stayed parked on a
	   channel that would never be written to again, and its peer list froze — it kept probing pods
	   that had left the fleet and never learned of new ones, while heartbeats, the connection gauge
	   and registered==expected all read healthy and nothing was logged. When the second subscriber
	   disconnected, its deferred cleanup owned the mapped entry and deleted the id outright, so the
	   real agent stayed deaf even after it left.

	   A set cannot be displaced: a stream removes only its OWN watcher, and a broadcast reaches all
	   of them. It does not authenticate anything — that is the NetworkPolicy's job, and the shared
	   policy admits the gRPC port from agent pods of this release only — but no caller can take an
	   agent's subscription away from it. */
	watchers map[string]map[*peerWatcher]struct{}

	leaderElection bool
	isLeader       func() bool

	eventsEnabled bool
	eventsMu      sync.RWMutex
	eventSubs     map[string]chan *pb.Event
	eventSeq      atomic.Uint64

	// leaderCheckInterval bounds how long a demoted replica may keep streaming
	// events to already-connected subscribers. Shortened by tests.
	leaderCheckInterval time.Duration

	// stopCh is closed by Shutdown to end every open server-streaming handler.
	stopCh   chan struct{}
	stopOnce sync.Once

	// Trailing-edge coalescing of peer fan-out; see SchedulePeerBroadcast.
	broadcastMu    sync.Mutex
	pendingPeers   []model.AgentInfo
	broadcastArmed bool
	// peerBroadcastWindow bounds how long a coalesced FULL_SYNC may lag the registry. Shortened by
	// tests.
	peerBroadcastWindow time.Duration

	/* peerPlan is the sparse probe plan in force; nil means full mesh (the pre-M10 behavior,
	   untouched). Every peer list this server hands out — the Register response, the WatchPeers
	   initial FULL_SYNC, and each broadcast's filtered list — passes through it, so the agent code
	   needs no change: an agent just probes whatever list arrives. Replaced wholesale from the
	   registry OnChange chain, never mutated in place. */
	peerPlan atomic.Pointer[meshplan.Plan]
}

// defaultLeaderCheckInterval is how often an open WatchEvents stream re-checks
// leadership.
const defaultLeaderCheckInterval = 5 * time.Second

// defaultPeerBroadcastWindow is the coalescing window for peer fan-out: long enough to collapse a
// registration burst (a DaemonSet rollout lands many registers within tens of milliseconds), short
// enough that peer-plan staleness is invisible next to probe intervals and reconnect backoff.
const defaultPeerBroadcastWindow = 200 * time.Millisecond

func NewGRPCServer(
	registry *Registry,
	m *metrics.PrometheusMetrics,
	leaderElection bool,
	isLeader func() bool,
	eventsEnabled bool,
) *GRPCServer {
	return &GRPCServer{
		registry:            registry,
		metrics:             m,
		taskMgr:             NewTaskManager(),
		externalMgr:         NewExternalCheckManager(),
		watchers:            make(map[string]map[*peerWatcher]struct{}),
		leaderElection:      leaderElection,
		isLeader:            isLeader,
		eventsEnabled:       eventsEnabled,
		eventSubs:           make(map[string]chan *pb.Event),
		leaderCheckInterval: defaultLeaderCheckInterval,
		peerBroadcastWindow: defaultPeerBroadcastWindow,
		stopCh:              make(chan struct{}),
	}
}

// Shutdown ends every open server-streaming handler so a subsequent grpc.Server.GracefulStop can
// complete; it is idempotent and safe to call concurrently with PublishEvent and
// BroadcastPeerUpdate.
func (s *GRPCServer) Shutdown() {
	s.stopOnce.Do(func() { close(s.stopCh) })
}

// TaskManager exposes the task dispatcher so the HTTP diagnostics handler can
// dispatch on-demand tasks over the same streams agents watch.
func (s *GRPCServer) TaskManager() *TaskManager {
	return s.taskMgr
}

// ExternalCheckManager exposes the continuous external-check assignment store
// so the HTTP PUT handler can fan changes out over the same streams agents
// watch.
func (s *GRPCServer) ExternalCheckManager() *ExternalCheckManager {
	return s.externalMgr
}

// RegisterService registers the controller's gRPC services; EventStream is only registered when
// controller.events.enabled.
func (s *GRPCServer) RegisterService(srv *grpc.Server) {
	pb.RegisterAgentRegistryServer(srv, s)
	if s.eventsEnabled {
		pb.RegisterEventStreamServer(srv, s)
	}
}

// SetPeerPlan installs the probe plan applied to every peer list this server emits; nil restores
// full mesh. Called from the registry OnChange chain (under its notifyMu), so plan replacements
// arrive in mutation order and Register's own GetPeers below always sees a plan that already
// includes the agent it just accepted.
func (s *GRPCServer) SetPeerPlan(p meshplan.Plan) {
	if p == nil {
		s.peerPlan.Store(nil)
		return
	}
	s.peerPlan.Store(&p)
}

// CurrentPlan returns the probe plan in force; nil means full mesh. The returned map is shared and
// read-only by contract (a Plan is never mutated after meshplan.Build) — the topology snapshot
// reads it to render which pairs are intended to probe.
func (s *GRPCServer) CurrentPlan() meshplan.Plan {
	if p := s.peerPlan.Load(); p != nil {
		return *p
	}
	return nil
}

// filterPeersByPlan reduces a full peer list to the planned subset. A nil plan returns the input
// untouched. An agent MISSING from a non-nil plan gets nothing rather than everything: the plan is
// rebuilt on every registry change, so a missing entry means the agent is not in the registry
// snapshot the plan was built from, and its own (re-)registration is what repairs it.
func (s *GRPCServer) filterPeersByPlan(agentID string, peers []model.AgentInfo) []model.AgentInfo {
	plan := s.CurrentPlan()
	if plan == nil {
		return peers
	}
	allowed := plan[agentID]
	filtered := make([]model.AgentInfo, 0, len(allowed))
	for i := range peers {
		if planContains(allowed, peers[i].ID) {
			filtered = append(filtered, peers[i])
		}
	}
	return filtered
}

// planContains is a linear scan on purpose: a planned peer list is ringDegree+zoneChords entries
// (single digits), where a per-lookup map build would cost more than it saves.
func planContains(allowed []string, id string) bool {
	for _, a := range allowed {
		if a == id {
			return true
		}
	}
	return false
}

// Register accepts an agent into the registry; leader-only when leader election is enabled, or the
// Service round-robin would split the agents across replicas and each would plan its own mesh.
func (s *GRPCServer) Register(_ context.Context, req *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	if s.lostLeadership() {
		return nil, status.Error(codes.Unavailable, "not the leader")
	}

	agentMeta := req.GetAgent()
	/* An agent is only an agent if it says WHO and WHERE it is.
	   Nothing checked: GetAgent() is nil-safe, so an empty RegisterRequest produced AgentInfo{} —
	   stored under the key "" and broadcast to the whole fleet as a peer, which every agent then
	   turned into a probe against PodIP "" forever. It is not only reachable adversarially: an agent
	   started without the downward-API env registers exactly this. */
	if err := validateAgentMeta(agentMeta); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	info := model.AgentInfo{
		ID:       agentMeta.GetId(),
		NodeName: agentMeta.GetNodeName(),
		PodName:  agentMeta.GetPodName(),
		PodIP:    agentMeta.GetPodIp(),
		Zone:     agentMeta.GetZone(),
		Labels:   agentMeta.GetLabels(),
		// Retained so the diagnostics handler can gate external destinations on
		// what this agent build actually supports.
		Capabilities: agentMeta.GetCapabilities(),
	}

	resolved := s.registry.Register(info)
	s.metrics.ControllerRegisteredAgents.WithLabelValues().Set(float64(s.registry.Count()))

	peers := s.filterPeersByPlan(resolved.ID, s.registry.GetPeers(resolved.ID))
	pbPeers := make([]*pb.AgentMeta, 0, len(peers))
	for i := range peers {
		pbPeers = append(pbPeers, peerToProto(peers[i]))
	}

	return &pb.RegisterResponse{
		AgentId:    resolved.ID,
		Peers:      pbPeers,
		ServerTime: timestamppb.Now(),
		Agent:      agentInfoToProto(resolved),
	}, nil
}

// validateAgentMeta rejects a registration that cannot describe a probe target. The PodIP has to
// PARSE: a peer with a malformed address is a checker target that can never connect, published to
// every other agent in the fleet.
func validateAgentMeta(m *pb.AgentMeta) error {
	switch {
	case m == nil:
		return errors.New("register: no agent metadata")
	case m.GetId() == "":
		return errors.New("register: agent id is empty")
	case m.GetNodeName() == "":
		return errors.New("register: node name is empty")
	case m.GetPodIp() == "":
		return errors.New("register: pod IP is empty")
	case net.ParseIP(m.GetPodIp()) == nil:
		return fmt.Errorf("register: pod IP %q is not an IP address", m.GetPodIp())
	}
	return nil
}

// Heartbeat is deliberately not leader-gated: a non-leader holds no agents, so the lookup below
// already answers NotFound, which is the code that drives the agent's re-registration.
func (s *GRPCServer) Heartbeat(_ context.Context, req *pb.HeartbeatRequest) (*emptypb.Empty, error) {
	if !s.registry.Heartbeat(req.GetAgentId()) {
		slog.Warn("heartbeat from unknown agent", "id", req.GetAgentId())
		return nil, status.Errorf(codes.NotFound, "agent %s not registered", req.GetAgentId())
	}
	return &emptypb.Empty{}, nil
}

// Deregister removes an agent from the registry on graceful shutdown, so peers
// stop probing its dead pod IP immediately instead of waiting for TTL eviction.
// Unknown agent IDs are a no-op and do not return an error.
func (s *GRPCServer) Deregister(_ context.Context, req *pb.DeregisterRequest) (*emptypb.Empty, error) {
	s.registry.Deregister(req.GetAgentId())
	s.metrics.ControllerRegisteredAgents.WithLabelValues().Set(float64(s.registry.Count()))
	return &emptypb.Empty{}, nil
}

// WatchPeers server-streams an agent's peer list, which is the probe plan; leader-only when leader
// election is enabled.
func (s *GRPCServer) WatchPeers(req *pb.WatchPeersRequest, stream pb.AgentRegistry_WatchPeersServer) error {
	if s.lostLeadership() {
		return status.Error(codes.Unavailable, "not the leader")
	}

	agentID := req.GetAgentId()

	w := &peerWatcher{ch: make(chan *pb.PeerUpdate, peerWatcherBuffer), desynced: make(chan struct{})}
	s.mu.Lock()
	if s.watchers[agentID] == nil {
		s.watchers[agentID] = make(map[*peerWatcher]struct{}, 1)
	}
	s.watchers[agentID][w] = struct{}{}
	s.metrics.ControllerGRPCConnections.WithLabelValues().Inc()
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		// Only this handler's own watcher, never the id: a reconnecting agent (or any other caller)
		// may hold another stream under the same id, and removing the id would unsubscribe it —
		// that agent's peer list would then freeze with nothing said anywhere.
		if set, ok := s.watchers[agentID]; ok {
			delete(set, w)
			if len(set) == 0 {
				delete(s.watchers, agentID)
			}
		}
		s.metrics.ControllerGRPCConnections.WithLabelValues().Dec()
		s.mu.Unlock()
		close(w.ch)
	}()

	peers := s.filterPeersByPlan(agentID, s.registry.GetPeers(agentID))
	pbPeers := make([]*pb.AgentMeta, 0, len(peers))
	for i := range peers {
		pbPeers = append(pbPeers, peerToProto(peers[i]))
	}
	// Through the same bounded write as every later update: the FIRST send is the one a subscriber
	// that never reads blocks on.
	if err := s.sendPeerUpdate(stream, w, &pb.PeerUpdate{
		Type:      pb.PeerUpdate_FULL_SYNC,
		Peers:     pbPeers,
		Timestamp: timestamppb.Now(),
	}); err != nil {
		return err
	}

	// kube-proxy leaves established connections alone, so a demoted replica has to end the stream
	// itself for the agent's reconnect loop to move it to the new leader.
	leaderCheck := time.NewTicker(s.leaderCheckInterval)
	defer leaderCheck.Stop()

	for {
		select {
		case update, ok := <-w.ch:
			if !ok {
				return nil
			}
			if err := s.sendPeerUpdate(stream, w, update); err != nil {
				return err
			}
		case <-w.desynced:
			/* A peer update could not be queued for this stream, so what it holds is no longer what
			   the registry holds — and every update is a FULL_SYNC applied by wholesale replacement,
			   so the loss is not self-correcting. Ending the stream is the recovery: the agent's
			   reconnect loop re-subscribes and the first thing WatchPeers sends is a fresh
			   FULL_SYNC. Dropping the message instead left an agent probing a mesh that no longer
			   existed, indefinitely — the stream stayed healthy, heartbeats kept succeeding, and
			   nothing anywhere resynced it. */
			return status.Error(codes.Unavailable, "peer update dropped: resubscribe for a full sync")
		case <-leaderCheck.C:
			if s.lostLeadership() {
				return status.Error(codes.Unavailable, "leadership lost")
			}
		case <-s.stopCh:
			return status.Error(codes.Unavailable, "controller shutting down")
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}

/*
sendPeerUpdate performs ONE stream.Send with a bound on how long it may block.

stream.Send parks on the HTTP/2 stream's flow-control window, so a subscriber that stops reading
while keeping the connection alive froze the handler goroutine inside it — and a goroutine parked in
Send is a goroutine that will never reach its select again. It could not observe w.desynced, could
not observe the leadership check, and could not observe s.stopCh: BroadcastPeerUpdate went on filling
its 16-deep mailbox, logged "ending the stream so the agent resyncs" on every topology change after
that, and ended nothing. The connection gauge and the goroutine were held until the client's TCP
died, which for a frozen pod can be a very long time.

A Send that cannot make progress inside peerSendTimeout is treated as the subscriber being gone: the
handler returns, which closes the stream, which unblocks the goroutine below (it writes to a buffered
channel and exits). Only one Send is ever in flight, so this does not violate the one-writer rule.
*/
func (s *GRPCServer) sendPeerUpdate(
	stream pb.AgentRegistry_WatchPeersServer, w *peerWatcher, update *pb.PeerUpdate,
) error {
	done := make(chan error, 1)
	go func() { done <- stream.Send(update) }()

	timer := time.NewTimer(peerSendTimeout)
	defer timer.Stop()

	select {
	case err := <-done:
		return err
	case <-timer.C:
		return status.Error(codes.Unavailable,
			"peer update could not be written: the subscriber is not reading its stream")
	case <-w.desynced:
		return status.Error(codes.Unavailable, "peer update dropped: resubscribe for a full sync")
	case <-s.stopCh:
		return status.Error(codes.Unavailable, "controller shutting down")
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
}

// peerSendTimeout bounds ONE write to a peer-update subscriber. It is generous next to a healthy
// agent's read loop (microseconds) and short next to the interval at which topology changes arrive.
const peerSendTimeout = 10 * time.Second

// WatchTasks server-streams on-demand diagnostic tasks to an agent. It mirrors
// the WatchPeers lifecycle: register a subscription, count the connection, and
// clean up on stream close. Task fan-out is owned by the TaskManager.
func (s *GRPCServer) WatchTasks(req *pb.WatchTasksRequest, stream pb.AgentRegistry_WatchTasksServer) error {
	if s.lostLeadership() {
		return status.Error(codes.Unavailable, "not the leader")
	}

	agentID := req.GetAgentId()

	tasks, cleanup := s.taskMgr.Subscribe(agentID)
	s.metrics.ControllerGRPCConnections.WithLabelValues().Inc()

	defer func() {
		cleanup()
		s.metrics.ControllerGRPCConnections.WithLabelValues().Dec()
	}()

	/* The same demotion check WatchPeers and WatchEvents carry. Without it a replica demoted while
	   it stayed alive kept serving task subscriptions forever: its subscriber map and the connection
	   gauges kept counting agents it no longer owns, and a client that does not tear the whole
	   ClientConn down stayed pinned to the wrong replica. */
	leaderCheck := time.NewTicker(s.leaderCheckInterval)
	defer leaderCheck.Stop()

	for {
		select {
		// The TaskManager never closes the subscription channel (see Subscribe), so this branch does not
		// fire on teardown.
		case task, ok := <-tasks:
			if !ok {
				return nil
			}
			if err := stream.Send(task); err != nil {
				return err
			}
		case <-leaderCheck.C:
			if s.lostLeadership() {
				return status.Error(codes.Unavailable, "leadership lost")
			}
		case <-s.stopCh:
			return status.Error(codes.Unavailable, "controller shutting down")
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}

// WatchExternalChecks server-streams an agent's CONTINUOUS external-check assignment; a change
// landing in between is delivered twice.
func (s *GRPCServer) WatchExternalChecks(
	req *pb.WatchExternalChecksRequest,
	stream pb.AgentRegistry_WatchExternalChecksServer,
) error {
	if s.lostLeadership() {
		return status.Error(codes.Unavailable, "not the leader")
	}

	agentID := req.GetAgentId()

	updates, cleanup := s.externalMgr.Subscribe(agentID)
	s.metrics.ControllerExternalSubscribers.WithLabelValues().Inc()
	s.metrics.ControllerGRPCConnections.WithLabelValues().Inc()

	defer func() {
		cleanup()
		s.metrics.ControllerExternalSubscribers.WithLabelValues().Dec()
		s.metrics.ControllerGRPCConnections.WithLabelValues().Dec()
	}()

	if err := stream.Send(s.externalMgr.Assignment(agentID)); err != nil {
		return err
	}

	// See WatchTasks: a demoted replica has to end its own streams.
	leaderCheck := time.NewTicker(s.leaderCheckInterval)
	defer leaderCheck.Stop()

	for {
		select {
		// The ExternalCheckManager never closes the subscription channel (see Subscribe), so this branch
		// does not fire on teardown.
		case assignment, ok := <-updates:
			if !ok {
				return nil
			}
			if err := stream.Send(assignment); err != nil {
				return err
			}
		case <-leaderCheck.C:
			if s.lostLeadership() {
				return status.Error(codes.Unavailable, "leadership lost")
			}
		case <-s.stopCh:
			return status.Error(codes.Unavailable, "controller shutting down")
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}

// ReportTaskResult delivers a task outcome from an agent back to the waiting
// Dispatch caller. Unknown task IDs are dropped by the TaskManager and are not
// an error.
func (s *GRPCServer) ReportTaskResult(_ context.Context, res *pb.TaskResult) (*emptypb.Empty, error) {
	s.taskMgr.Report(res)
	return &emptypb.Empty{}, nil
}

// WatchEvents server-streams controller domain events to the Console; leader-only when leader
// election is enabled.
func (s *GRPCServer) WatchEvents(_ *pb.WatchEventsRequest, stream pb.EventStream_WatchEventsServer) error {
	if s.lostLeadership() {
		return status.Error(codes.Unavailable, "not the leader")
	}

	id := uuid.NewString()
	ch := make(chan *pb.Event, 64)
	s.eventsMu.Lock()
	s.eventSubs[id] = ch
	s.metrics.ControllerEventSubscribers.WithLabelValues().Inc()
	s.metrics.ControllerGRPCConnections.WithLabelValues().Inc()
	s.eventsMu.Unlock()

	defer func() {
		s.eventsMu.Lock()
		delete(s.eventSubs, id)
		s.metrics.ControllerEventSubscribers.WithLabelValues().Dec()
		s.metrics.ControllerGRPCConnections.WithLabelValues().Dec()
		s.eventsMu.Unlock()
		close(ch)
	}()

	leaderCheck := time.NewTicker(s.leaderCheckInterval)
	defer leaderCheck.Stop()

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(ev); err != nil {
				return err
			}
		case <-leaderCheck.C:
			if s.lostLeadership() {
				return status.Error(codes.Unavailable, "leadership lost")
			}
		case <-s.stopCh:
			return status.Error(codes.Unavailable, "controller shutting down")
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}

// lostLeadership reports whether leader election is on and this replica is not
// (or is no longer) the leader.
func (s *GRPCServer) lostLeadership() bool {
	return s.leaderElection && (s.isLeader == nil || !s.isLeader())
}

// PublishEvent assigns a sequence number and timestamp; callers construct ev with only the oneof
// Payload field set.
func (s *GRPCServer) PublishEvent(ev *pb.Event) {
	if !s.eventsEnabled {
		return
	}

	ev.Seq = s.eventSeq.Add(1)
	ev.Timestamp = timestamppb.Now()

	s.eventsMu.RLock()
	defer s.eventsMu.RUnlock()
	for id, ch := range s.eventSubs {
		select {
		case ch <- ev:
		default:
			slog.Warn("dropping event, subscriber channel full", "subscriber", id)
		}
	}
	s.metrics.ControllerEventsPublished.WithLabelValues(eventType(ev)).Inc()
}

// EventSubscriberCount reports the number of active WatchEvents streams.
// Intended for tests and diagnostics.
func (s *GRPCServer) EventSubscriberCount() int {
	s.eventsMu.RLock()
	defer s.eventsMu.RUnlock()
	return len(s.eventSubs)
}

// eventType returns a bounded-cardinality label for ControllerEventsPublished.
func eventType(ev *pb.Event) string {
	switch ev.GetPayload().(type) {
	case *pb.Event_TopologyChanged:
		return "topology_changed"
	case *pb.Event_CheckObserved:
		return "check_observed"
	case *pb.Event_MtrTriggered:
		return "mtr_triggered"
	case *pb.Event_MtrCompleted:
		return "mtr_completed"
	case *pb.Event_DiagnosticProgress:
		return "diagnostic_progress"
	default:
		return "unknown"
	}
}

/*
 * peerWatcher is ONE WatchPeers stream's mailbox, plus the signal that it fell behind.
 *
 * The buffer is not a queue to be trimmed: every PeerUpdate is a FULL_SYNC that the agent applies by
 * wholesale replacement, so a dropped one is not a missed increment, it is a permanently wrong probe
 * mesh. When the mailbox is full the stream is marked desynced and torn down, and the agent's
 * reconnect gets it a fresh full sync.
 */
type peerWatcher struct {
	ch       chan *pb.PeerUpdate
	desynced chan struct{}
	once     sync.Once
}

// peerWatcherBuffer is how many topology changes one stream may fall behind before the stream is
// declared desynced. A DaemonSet rollout on a large fleet is the burst this has to absorb.
const peerWatcherBuffer = 16

func (w *peerWatcher) markDesynced() {
	w.once.Do(func() { close(w.desynced) })
}

/*
SchedulePeerBroadcast coalesces peer fan-out: it records the newest snapshot and arms ONE
trailing-edge flush per window instead of broadcasting on every registry change.

During a rollout N changes arrive as a burst and each used to run its own O(N) broadcast — O(N²)
messages that overflowed peerWatcherBuffer, desynced every stream, and each desync then cost a
reconnect plus one more FULL_SYNC. Collapsing is safe because every update is a FULL_SYNC applied
by wholesale replacement: the newest list supersedes anything a suppressed broadcast would have
said. The armed timer is deliberately NOT reset by later arrivals — a resetting debounce never
fires under sustained churn — so staleness is bounded by one window.

Callers arrive in mutation order (the registry publishes under notifyMu), so "newest snapshot
wins" here preserves the ordering that lock exists to provide.
*/
func (s *GRPCServer) SchedulePeerBroadcast(agents []model.AgentInfo) {
	s.broadcastMu.Lock()
	s.pendingPeers = agents
	if s.broadcastArmed {
		s.broadcastMu.Unlock()
		return
	}
	s.broadcastArmed = true
	s.broadcastMu.Unlock()

	time.AfterFunc(s.peerBroadcastWindow, s.flushPeerBroadcast)
}

func (s *GRPCServer) flushPeerBroadcast() {
	s.broadcastMu.Lock()
	agents := s.pendingPeers
	s.pendingPeers = nil
	s.broadcastArmed = false
	s.broadcastMu.Unlock()

	select {
	case <-s.stopCh:
		// Shutdown outran the window; every stream is ending, nobody needs this snapshot.
		return
	default:
	}
	// A replica demoted inside the window has nothing to announce about a fleet it no longer owns
	// (ResetQuiet's reasoning); the new leader's own FULL_SYNC replaces the plan.
	if s.lostLeadership() {
		return
	}
	s.BroadcastPeerUpdate(agents)
}

func (s *GRPCServer) BroadcastPeerUpdate(agents []model.AgentInfo) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.watchers) == 0 {
		return
	}

	// Each agent's proto is built ONCE per broadcast and shared by every watcher's filtered list;
	// stream.Send only marshals, so sharing across streams is safe. Building inside the watcher
	// loop made one broadcast cost O(watchers × agents) conversions.
	protos := make([]*pb.AgentMeta, len(agents))
	for i := range agents {
		protos[i] = peerToProto(agents[i])
	}
	now := timestamppb.Now()

	// Read once per broadcast, not per watcher: a plan swap mid-loop must not hand half the fleet
	// the old mesh and half the new one within a single FULL_SYNC generation.
	plan := s.CurrentPlan()

	for watcherID, set := range s.watchers {
		var allowed []string
		if plan != nil {
			allowed = plan[watcherID]
		}
		filtered := make([]*pb.AgentMeta, 0, len(protos))
		for i := range agents {
			if agents[i].ID == watcherID {
				continue
			}
			if plan != nil && !planContains(allowed, agents[i].ID) {
				continue
			}
			filtered = append(filtered, protos[i])
		}
		// One update per id, shared by every stream open for that id.
		update := &pb.PeerUpdate{
			Type:      pb.PeerUpdate_FULL_SYNC,
			Peers:     filtered,
			Timestamp: now,
		}

		for w := range set {
			select {
			case w.ch <- update:
				s.metrics.ControllerPeerUpdates.WithLabelValues().Inc()
			default:
				// Not a drop: a desync. See peerWatcher.
				// Marked, not ended: the stream's own handler is what observes this and returns.
				slog.Warn("peer update could not be queued; marking the stream desynced so the agent resubscribes",
					"agent", watcherID, "buffer", peerWatcherBuffer)
				w.markDesynced()
			}
		}
	}
}

func agentInfoToProto(a model.AgentInfo) *pb.AgentMeta { //nolint:gocritic // hugeParam: value copy is intentional for proto conversion
	return &pb.AgentMeta{
		Id:           a.ID,
		NodeName:     a.NodeName,
		PodName:      a.PodName,
		PodIp:        a.PodIP,
		Zone:         a.Zone,
		Labels:       a.Labels,
		Capabilities: a.Capabilities,
	}
}

// peerToProto is the NARROW projection for peer LISTS: exactly the fields the agent's
// protoToTargets reads. PodName, Labels and Capabilities are controller-side concerns; in proto3
// omitting them removes them from the wire entirely (an old agent decodes them as empty, which is
// what it ignored anyway), and the labels map is the dominant term of a FULL_SYNC's size at 100+
// nodes. Anything that is NOT a peer list — RegisterResponse.Agent, TaskRequest.Target — keeps
// agentInfoToProto.
func peerToProto(a model.AgentInfo) *pb.AgentMeta { //nolint:gocritic // hugeParam: value copy is intentional for proto conversion
	return &pb.AgentMeta{
		Id:       a.ID,
		NodeName: a.NodeName,
		PodIp:    a.PodIP,
		Zone:     a.Zone,
	}
}
