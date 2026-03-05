package controller

import (
	"context"
	"log/slog"
	"sync"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type GRPCServer struct {
	pb.UnimplementedAgentRegistryServer
	registry *Registry
	metrics  *metrics.PrometheusMetrics

	mu       sync.RWMutex
	watchers map[string]chan *pb.PeerUpdate
}

func NewGRPCServer(registry *Registry, m *metrics.PrometheusMetrics) *GRPCServer {
	return &GRPCServer{
		registry: registry,
		metrics:  m,
		watchers: make(map[string]chan *pb.PeerUpdate),
	}
}

func (s *GRPCServer) RegisterService(srv *grpc.Server) {
	pb.RegisterAgentRegistryServer(srv, s)
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
	}

	s.registry.Register(info)
	s.metrics.ControllerRegisteredAgents.WithLabelValues().Set(float64(s.registry.Count()))

	peers := s.registry.GetPeers(info.ID)
	pbPeers := make([]*pb.AgentMeta, 0, len(peers))
	for i := range peers {
		pbPeers = append(pbPeers, agentInfoToProto(peers[i]))
	}

	return &pb.RegisterResponse{
		AgentId:    info.ID,
		Peers:      pbPeers,
		ServerTime: timestamppb.Now(),
	}, nil
}

func (s *GRPCServer) Heartbeat(_ context.Context, req *pb.HeartbeatRequest) (*emptypb.Empty, error) {
	if !s.registry.Heartbeat(req.GetAgentId()) {
		slog.Warn("heartbeat from unknown agent", "id", req.GetAgentId())
		return nil, status.Errorf(codes.NotFound, "agent %s not registered", req.GetAgentId())
	}
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
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
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
		Id:       a.ID,
		NodeName: a.NodeName,
		PodName:  a.PodName,
		PodIp:    a.PodIP,
		Zone:     a.Zone,
		Labels:   a.Labels,
	}
}
