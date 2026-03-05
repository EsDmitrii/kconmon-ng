package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type GRPCClient struct {
	conn             *grpc.ClientConn
	client           pb.AgentRegistryClient
	agentID          string
	onPeers          func([]checker.Target)
	onNeedReregister func()
}

func NewGRPCClient(address string) (*GRPCClient, error) {
	conn, err := grpc.NewClient(address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Second,
			Timeout:             5 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to controller: %w", err)
	}

	return &GRPCClient{
		conn:   conn,
		client: pb.NewAgentRegistryClient(conn),
	}, nil
}

func (c *GRPCClient) OnPeersUpdate(fn func([]checker.Target)) {
	c.onPeers = fn
}

func (c *GRPCClient) OnNeedReregister(fn func()) {
	c.onNeedReregister = fn
}

func (c *GRPCClient) Register(ctx context.Context, info model.AgentInfo, httpPort int) ([]checker.Target, error) { //nolint:gocritic // hugeParam: AgentInfo is passed by value intentionally
	resp, err := c.client.Register(ctx, &pb.RegisterRequest{
		Agent: &pb.AgentMeta{
			Id:       info.ID,
			NodeName: info.NodeName,
			PodName:  info.PodName,
			PodIp:    info.PodIP,
			Zone:     info.Zone,
			Labels:   info.Labels,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("registering agent: %w", err)
	}

	c.agentID = resp.GetAgentId()
	return protoToTargets(resp.GetPeers(), httpPort), nil
}

func (c *GRPCClient) StartHeartbeat(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			_, err := c.client.Heartbeat(ctx, &pb.HeartbeatRequest{
				AgentId:   c.agentID,
				Timestamp: timestamppb.Now(),
			})
			if err != nil {
				st, ok := grpcstatus.FromError(err)
				if ok && st.Code() == codes.NotFound {
					slog.Warn("agent not registered on controller, triggering re-registration")
					if c.onNeedReregister != nil {
						c.onNeedReregister()
					}
				} else {
					slog.Error("heartbeat failed", "error", err)
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func (c *GRPCClient) WatchPeers(ctx context.Context, httpPort int) error {
	stream, err := c.client.WatchPeers(ctx, &pb.WatchPeersRequest{
		AgentId: c.agentID,
	})
	if err != nil {
		return fmt.Errorf("watching peers: %w", err)
	}

	for {
		update, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("receiving peer update: %w", err)
		}

		targets := protoToTargets(update.GetPeers(), httpPort)
		slog.Info("peer update received", "type", update.GetType(), "count", len(targets))

		if c.onPeers != nil {
			c.onPeers(targets)
		}
	}
}

func (c *GRPCClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

func protoToTargets(peers []*pb.AgentMeta, httpPort int) []checker.Target {
	targets := make([]checker.Target, 0, len(peers))
	for _, p := range peers {
		targets = append(targets, checker.Target{
			AgentID:  p.GetId(),
			NodeName: p.GetNodeName(),
			PodIP:    p.GetPodIp(),
			Zone:     p.GetZone(),
			Port:     httpPort,
		})
	}
	return targets
}
