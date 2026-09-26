package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/config"
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
	// A set of peer-update streams per agent id: the id is client-supplied, so a second subscriber
	// under it must not displace the agent's own stream. A stream removes only its own watcher, and a
	// broadcast reaches all of them.
	watchers map[string]map[*peerWatcher]struct{}

	gate leaderGate

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

// defaultLeaderCheckInterval is how often an open server stream re-checks leadership.
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
		gate:                leaderGate{enabled: leaderElection, isLeader: isLeader},
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

// RegisterGatewayService registers only the agent-facing service on the external gateway. The
// EventStream subscription carries no identity the gateway could pin, and its events include the
// task ids a caller would need to answer another agent's diagnostics.
func (s *GRPCServer) RegisterGatewayService(srv *grpc.Server) {
	pb.RegisterAgentRegistryServer(srv, s)
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
	return slices.Contains(allowed, id)
}

// Register accepts an agent into the registry; leader-only when leader election is enabled, or the
// Service round-robin would split the agents across replicas and each would plan its own mesh.
func (s *GRPCServer) Register(ctx context.Context, req *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	if s.gate.lost() {
		return nil, errNotLeader
	}

	agentMeta := req.GetAgent()
	// GetAgent() is nil-safe, so an empty request (an agent started without the downward-API env)
	// would otherwise register AgentInfo{} and hand the fleet a peer with no address to probe.
	if err := validateAgentMeta(agentMeta); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	info := model.AgentInfo{
		ID:       agentMeta.GetId(),
		NodeName: agentMeta.GetNodeName(),
		PodName:  agentMeta.GetPodName(),
		PodIP:    agentMeta.GetPodIp(),
		Zone:     agentMeta.GetZone(),
		Labels:   registrantLabels(ctx, agentMeta.GetLabels()),
		// Retained so the diagnostics handler can gate external destinations on
		// what this agent build actually supports.
		Capabilities: agentMeta.GetCapabilities(),
		// 0 = the agent predates the port fields; peers then fall back to their own configured port.
		HTTPPort:    int(agentMeta.GetHttpPort()),
		UDPPort:     int(agentMeta.GetUdpPort()),
		MetricsPort: int(agentMeta.GetMetricsPort()),
	}

	resolved := s.registry.Register(info)

	peers := s.filterPeersByPlan(resolved.ID, s.registry.GetPeers(resolved.ID))
	pbPeers := make([]*pb.AgentMeta, 0, len(peers))
	for i := range peers {
		pbPeers = append(pbPeers, peerToProto(peers[i]))
	}

	return &pb.RegisterResponse{
		AgentId:     resolved.ID,
		Peers:       pbPeers,
		ServerTime:  timestamppb.Now(),
		Agent:       agentInfoToProto(resolved),
		FleetEchoes: fleetEchoes(s.registry.GetAll()),
	}, nil
}

// gatewayCallerKey marks a request that came in through the external gateway's interceptor.
type gatewayCallerKey struct{}

func withGatewayCaller(ctx context.Context) context.Context {
	return context.WithValue(ctx, gatewayCallerKey{}, true)
}

// registrantLabels is labels with model.LabelExternal set by the listener, not by the agent: every
// gateway registrant runs outside the cluster, and the in-cluster listener serves pods and nodes only.
// The agent's own value is never trusted, so it can neither publish itself as a scrape target nor
// pose as a DaemonSet agent.
func registrantLabels(ctx context.Context, labels map[string]string) map[string]string {
	gateway, _ := ctx.Value(gatewayCallerKey{}).(bool)
	if _, claimed := labels[model.LabelExternal]; !gateway && !claimed {
		return labels
	}
	out := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		if k != model.LabelExternal {
			out[k] = v
		}
	}
	if gateway {
		out[model.LabelExternal] = "true"
	}
	return out
}

// maxPort bounds the port fields of AgentMeta: they are uint32 on the wire, so a value no socket
// can bind has to be refused here rather than published to the fleet as a peer to dial.
const maxPort = 65535

// validateAgentMeta rejects a registration that cannot describe a probe target. The PodIP has to
// PARSE and be probeable: a peer with a malformed or loopback address is a checker target that can
// never connect, published to every other agent in the fleet. Ports are optional (0 = a pre-2.4.0
// agent), but never > 65535.
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
	case config.UnreachableAdvertiseAddress(net.ParseIP(m.GetPodIp())) != "":
		return fmt.Errorf("register: pod IP %q is %s, which peers cannot probe; "+
			"set agent.advertiseAddress to an address they can reach",
			m.GetPodIp(), config.UnreachableAdvertiseAddress(net.ParseIP(m.GetPodIp())))
	case m.GetHttpPort() > maxPort:
		return fmt.Errorf("register: http_port %d is out of range", m.GetHttpPort())
	case m.GetUdpPort() > maxPort:
		return fmt.Errorf("register: udp_port %d is out of range", m.GetUdpPort())
	case m.GetMetricsPort() > maxPort:
		return fmt.Errorf("register: metrics_port %d is out of range", m.GetMetricsPort())
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
	return &emptypb.Empty{}, nil
}

// WatchPeers server-streams an agent's peer list, which is the probe plan; leader-only when leader
// election is enabled.
func (s *GRPCServer) WatchPeers(req *pb.WatchPeersRequest, stream pb.AgentRegistry_WatchPeersServer) error {
	if s.gate.lost() {
		return errNotLeader
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
	initial := &pb.PeerUpdate{
		Type:        pb.PeerUpdate_FULL_SYNC,
		Peers:       pbPeers,
		Timestamp:   timestamppb.Now(),
		FleetEchoes: fleetEchoes(s.registry.GetAll()),
	}
	if err := s.boundedSend(stream.Context(), w.desynced, func() error { return stream.Send(initial) }); err != nil {
		return err
	}

	// A desynced stream ends so that the agent resubscribes: every update is a FULL_SYNC applied by
	// wholesale replacement, so a dropped one would leave the agent on a stale mesh indefinitely.
	return pumpStream(stream.Context(), s, w.ch, w.desynced, stream.Send)
}

var (
	errNotLeader      = status.Error(codes.Unavailable, notLeaderMsg)
	errLeadershipLost = status.Error(codes.Unavailable, "leadership lost")
	errShuttingDown   = status.Error(codes.Unavailable, "controller shutting down")
	errDesynced       = status.Error(codes.Unavailable, "peer update dropped: resubscribe for a full sync")
	errNotReading     = status.Error(codes.Unavailable,
		"stream update could not be written: the subscriber is not reading its stream")
)

// pumpStream sends what ch delivers until the client leaves, the controller shuts down, desynced
// fires (a nil one never does) or this replica loses the lease. kube-proxy leaves established
// connections alone, so a demoted replica has to end its streams itself for the client's reconnect
// loop to reach the new leader.
func pumpStream[T any](
	ctx context.Context, s *GRPCServer, ch <-chan T, desynced <-chan struct{}, send func(T) error,
) error {
	leaderCheck := time.NewTicker(s.leaderCheckInterval)
	defer leaderCheck.Stop()

	for {
		select {
		// No manager closes a subscription channel while its stream runs; the check only keeps a
		// closed one from spinning.
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			if err := s.boundedSend(ctx, desynced, func() error { return send(msg) }); err != nil {
				return err
			}
		case <-desynced:
			return errDesynced
		case <-leaderCheck.C:
			if s.gate.lost() {
				return errLeadershipLost
			}
		case <-s.stopCh:
			return errShuttingDown
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

/*
boundedSend performs ONE stream.Send with a bound on how long it may block. Send parks on the HTTP/2
flow-control window, so a subscriber that stops reading while keeping its connection alive would
hold the handler inside Send, where it observes neither desynced, the leader check nor Shutdown.

A Send that makes no progress within peerSendTimeout counts as the subscriber being gone: the
handler returns, which closes the stream and unblocks the goroutine below (it writes to a buffered
channel and exits). Only one Send is ever in flight, so the one-writer rule holds. desynced may be
nil.
*/
func (s *GRPCServer) boundedSend(ctx context.Context, desynced <-chan struct{}, send func() error) error {
	done := make(chan error, 1)
	go func() { done <- send() }()

	timer := time.NewTimer(peerSendTimeout)
	defer timer.Stop()

	select {
	case err := <-done:
		return err
	case <-timer.C:
		return errNotReading
	case <-desynced:
		return errDesynced
	case <-s.stopCh:
		return errShuttingDown
	case <-ctx.Done():
		return ctx.Err()
	}
}

// peerSendTimeout bounds ONE write to any server stream. It is generous next to a healthy client's
// read loop (microseconds) and short next to the interval at which topology changes arrive.
const peerSendTimeout = 10 * time.Second

// WatchTasks server-streams on-demand diagnostic tasks to an agent. It mirrors
// the WatchPeers lifecycle: register a subscription, count the connection, and
// clean up on stream close. Task fan-out is owned by the TaskManager.
func (s *GRPCServer) WatchTasks(req *pb.WatchTasksRequest, stream pb.AgentRegistry_WatchTasksServer) error {
	if s.gate.lost() {
		return errNotLeader
	}

	tasks, cleanup := s.taskMgr.Subscribe(req.GetAgentId())
	s.metrics.ControllerGRPCConnections.WithLabelValues().Inc()

	defer func() {
		cleanup()
		s.metrics.ControllerGRPCConnections.WithLabelValues().Dec()
	}()

	return pumpStream(stream.Context(), s, tasks, nil, stream.Send)
}

// WatchExternalChecks server-streams an agent's CONTINUOUS external-check assignment; a change
// landing in between is delivered twice.
func (s *GRPCServer) WatchExternalChecks(
	req *pb.WatchExternalChecksRequest,
	stream pb.AgentRegistry_WatchExternalChecksServer,
) error {
	if s.gate.lost() {
		return errNotLeader
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

	initial := s.externalMgr.Assignment(agentID)
	if err := s.boundedSend(stream.Context(), nil, func() error { return stream.Send(initial) }); err != nil {
		return err
	}

	return pumpStream(stream.Context(), s, updates, nil, stream.Send)
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
	if s.gate.lost() {
		return errNotLeader
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

	return pumpStream(stream.Context(), s, ch, nil, stream.Send)
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

A rollout lands N changes in a burst, and a broadcast per change would send O(N²) messages,
overflow peerWatcherBuffer and desync every stream. Collapsing is safe because every update is a
FULL_SYNC applied by wholesale replacement: the newest list supersedes anything a suppressed
broadcast would have said. The armed timer is deliberately NOT reset by later arrivals — a resetting debounce never
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
	if s.gate.lost() {
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
	echoes := fleetEchoes(agents)
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
			Type:        pb.PeerUpdate_FULL_SYNC,
			Peers:       filtered,
			Timestamp:   now,
			FleetEchoes: echoes,
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
		HttpPort:     portToProto(a.HTTPPort),
		UdpPort:      portToProto(a.UDPPort),
		MetricsPort:  portToProto(a.MetricsPort),
	}
}

// peerToProto is the NARROW projection for peer LISTS: exactly what the agent's protoToTargets
// reads — id, node_name, pod_ip, zone, http_port (TCP probe target) and udp_port (echo target).
// metrics_port is for scrape discovery, not for peers: no agent dials it, and at FULL_SYNC it would
// be one more varint per peer for nothing. PodName, Labels and Capabilities are controller-side
// concerns; in proto3 omitting them removes them from the wire entirely (an old agent decodes them
// as empty, which is what it ignored anyway), and the labels map is the dominant term of a
// FULL_SYNC's size at 100+ nodes. Anything that is NOT a peer list — RegisterResponse.Agent,
// TaskRequest.Target — keeps agentInfoToProto.
func peerToProto(a model.AgentInfo) *pb.AgentMeta { //nolint:gocritic // hugeParam: value copy is intentional for proto conversion
	return &pb.AgentMeta{
		Id:       a.ID,
		NodeName: a.NodeName,
		PodIp:    a.PodIP,
		Zone:     a.Zone,
		HttpPort: portToProto(a.HTTPPort),
		UdpPort:  portToProto(a.UDPPort),
	}
}

// fleetEchoes is every agent's echo endpoint. A sparse plan hides most of the fleet from an agent's
// peer list, and an echo that knows only its peers answers a stranger's echo: one forged datagram
// then bounces between the two until the echo limiter drops it.
func fleetEchoes(agents []model.AgentInfo) []*pb.EchoEndpoint {
	out := make([]*pb.EchoEndpoint, len(agents))
	for i := range agents {
		out[i] = &pb.EchoEndpoint{Address: agents[i].PodIP, Port: portToProto(agents[i].UDPPort)}
	}
	return out
}

// portToProto narrows a stored port for the wire. Registration already refused anything above
// maxPort, so an out-of-range value here can only come from controller-side code; it is sent as 0
// ("unknown") rather than wrapped into a port that does not exist.
func portToProto(p int) uint32 {
	if p < 0 || p > maxPort {
		return 0
	}
	return uint32(p) //nolint:gosec // G115: range-checked above
}
