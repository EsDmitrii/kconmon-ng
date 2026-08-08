package controller

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
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

	mu       sync.RWMutex
	watchers map[string]chan *pb.PeerUpdate

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
	// grpc.Server.GracefulStop waits for handlers to return but never cancels
	// their stream contexts, so a handler that only selects on its own channel
	// and stream.Context() blocks shutdown forever. stopCh is the missing
	// out-of-band signal. stopOnce keeps the close idempotent.
	stopCh   chan struct{}
	stopOnce sync.Once
}

// defaultLeaderCheckInterval is how often an open WatchEvents stream re-checks
// leadership.
const defaultLeaderCheckInterval = 5 * time.Second

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
		watchers:            make(map[string]chan *pb.PeerUpdate),
		leaderElection:      leaderElection,
		isLeader:            isLeader,
		eventsEnabled:       eventsEnabled,
		eventSubs:           make(map[string]chan *pb.Event),
		leaderCheckInterval: defaultLeaderCheckInterval,
		stopCh:              make(chan struct{}),
	}
}

// Shutdown ends every open server-streaming handler so a subsequent
// grpc.Server.GracefulStop can complete. It is idempotent and safe to call
// concurrently with PublishEvent and BroadcastPeerUpdate: it only closes a
// signalling channel and touches no subscriber map, so publishers racing it
// keep taking their existing non-blocking paths and cannot panic.
//
// Handlers answer codes.Unavailable, the same code a non-leader replica
// returns, so the agent's WatchTasks reconnect loop and the Console ingester
// both treat it as an ordinary retryable stream end.
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

// RegisterService registers the controller's gRPC services. EventStream is only
// registered when controller.events.enabled is on: leaving it unregistered makes
// gRPC answer a subscribing Console with codes.Unimplemented, which is the
// honest answer and needs no per-RPC gate.
func (s *GRPCServer) RegisterService(srv *grpc.Server) {
	pb.RegisterAgentRegistryServer(srv, s)
	if s.eventsEnabled {
		pb.RegisterEventStreamServer(srv, s)
	}
}

func (s *GRPCServer) Register(_ context.Context, req *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	agentMeta := req.GetAgent()

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

	peers := s.registry.GetPeers(resolved.ID)
	pbPeers := make([]*pb.AgentMeta, 0, len(peers))
	for i := range peers {
		pbPeers = append(pbPeers, agentInfoToProto(peers[i]))
	}

	return &pb.RegisterResponse{
		AgentId:    resolved.ID,
		Peers:      pbPeers,
		ServerTime: timestamppb.Now(),
		Agent:      agentInfoToProto(resolved),
	}, nil
}

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

func (s *GRPCServer) WatchPeers(req *pb.WatchPeersRequest, stream pb.AgentRegistry_WatchPeersServer) error {
	agentID := req.GetAgentId()

	ch := make(chan *pb.PeerUpdate, 16)
	s.mu.Lock()
	s.watchers[agentID] = ch
	s.metrics.ControllerGRPCConnections.WithLabelValues().Inc()
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.watchers, agentID)
		s.metrics.ControllerGRPCConnections.WithLabelValues().Dec()
		s.mu.Unlock()
		close(ch)
	}()

	peers := s.registry.GetPeers(agentID)
	pbPeers := make([]*pb.AgentMeta, 0, len(peers))
	for i := range peers {
		pbPeers = append(pbPeers, agentInfoToProto(peers[i]))
	}
	if err := stream.Send(&pb.PeerUpdate{
		Type:      pb.PeerUpdate_FULL_SYNC,
		Peers:     pbPeers,
		Timestamp: timestamppb.Now(),
	}); err != nil {
		return err
	}

	for {
		select {
		case update, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(update); err != nil {
				return err
			}
		case <-s.stopCh:
			return status.Error(codes.Unavailable, "controller shutting down")
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}

// WatchTasks server-streams on-demand diagnostic tasks to an agent. It mirrors
// the WatchPeers lifecycle: register a subscription, count the connection, and
// clean up on stream close. Task fan-out is owned by the TaskManager.
func (s *GRPCServer) WatchTasks(req *pb.WatchTasksRequest, stream pb.AgentRegistry_WatchTasksServer) error {
	agentID := req.GetAgentId()

	tasks, cleanup := s.taskMgr.Subscribe(agentID)
	s.metrics.ControllerGRPCConnections.WithLabelValues().Inc()

	defer func() {
		cleanup()
		s.metrics.ControllerGRPCConnections.WithLabelValues().Dec()
	}()

	for {
		select {
		// The TaskManager never closes the subscription channel (see Subscribe),
		// so this branch does not fire on teardown; the loop exits via the
		// stream context or the server-wide stopCh below. The ok check is kept
		// as belt-and-braces.
		case task, ok := <-tasks:
			if !ok {
				return nil
			}
			if err := stream.Send(task); err != nil {
				return err
			}
		case <-s.stopCh:
			return status.Error(codes.Unavailable, "controller shutting down")
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}

// WatchExternalChecks server-streams an agent's CONTINUOUS external-check
// assignment. It mirrors the WatchTasks lifecycle — subscribe, count the
// connection, clean up on stream close — with one addition borrowed from
// WatchPeers: the agent's CURRENT assignment (an empty one when it has none)
// is sent immediately on subscribe, exactly as WatchPeers opens with a
// FULL_SYNC. That is what lets a restarting agent converge without waiting for
// the next operator change, and what stops a stale assignment surviving a
// controller restart: the empty send tells the agent to drop everything.
//
// Subscribing BEFORE reading the current assignment is deliberate: no change
// can slip through the gap. A change landing in between is delivered twice
// instead, which is harmless because every assignment is absolute state, never
// a delta.
func (s *GRPCServer) WatchExternalChecks(
	req *pb.WatchExternalChecksRequest,
	stream pb.AgentRegistry_WatchExternalChecksServer,
) error {
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

	for {
		select {
		// The ExternalCheckManager never closes the subscription channel (see
		// Subscribe), so this branch does not fire on teardown; the loop exits
		// via the stream context or the server-wide stopCh below. The ok check
		// is kept as belt-and-braces.
		case assignment, ok := <-updates:
			if !ok {
				return nil
			}
			if err := stream.Send(assignment); err != nil {
				return err
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

// WatchEvents server-streams controller domain events to the Console.
// Leader-only when leader election is enabled: a non-leader replica fails the
// call immediately with codes.Unavailable so the caller's reconnect loop
// retries (and may land on the leader on a subsequent dial), mirroring
// DiagnosticsHandler's HTTP 503-on-non-leader behavior.
//
// Leadership is also re-checked on a ticker while the stream is open, so a
// replica demoted mid-stream stops fanning out events instead of serving a
// stale view forever. A ticker rather than a check before each Send: staleness
// must stay bounded even when no events are flowing.
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

// PublishEvent assigns a sequence number and timestamp, then fans ev out to
// every subscribed WatchEvents stream. Callers construct ev with only the
// oneof Payload field set.
//
// With controller.events.enabled off this is a no-op taken before anything is
// stamped or counted: a disabled controller must not burn sequence numbers or
// move the published-events counter.
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

func (s *GRPCServer) BroadcastPeerUpdate(agents []model.AgentInfo) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for watcherID, ch := range s.watchers {
		filtered := make([]*pb.AgentMeta, 0, len(agents))
		for i := range agents {
			if agents[i].ID != watcherID {
				filtered = append(filtered, agentInfoToProto(agents[i]))
			}
		}

		update := &pb.PeerUpdate{
			Type:      pb.PeerUpdate_FULL_SYNC,
			Peers:     filtered,
			Timestamp: timestamppb.New(time.Now()),
		}

		select {
		case ch <- update:
			s.metrics.ControllerPeerUpdates.WithLabelValues().Inc()
		default:
			slog.Warn("dropping peer update, channel full", "agent", watcherID)
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
