package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

/*
ClientSecurity carries FILE PATHS, not parsed material: dialController loads them on every dial, so
a Reconnect after certificate or token rotation picks the new files up without a restart. The zero
value is the in-cluster contract — plaintext, no credentials — unchanged.
*/
type ClientSecurity struct {
	TLS       config.AgentTLSConfig
	TokenFile string
}

// clientSecurityFromConfig maps the agent config block onto the dialer's inputs; the config loader
// has already validated the combinations (cert+key together, token only with TLS).
func clientSecurityFromConfig(cfg *config.Config) ClientSecurity {
	return ClientSecurity{TLS: cfg.Agent.TLS, TokenFile: cfg.Agent.BootstrapTokenFile}
}

type GRPCClient struct {
	address  string
	security ClientSecurity

	// mu guards conn and client, which Reconnect swaps under the watch goroutines, and agentID,
	// which reregister() rewrites from two goroutines while the streams read it.
	mu     sync.RWMutex
	conn   *grpc.ClientConn
	client pb.AgentRegistryClient

	agentID          string
	onPeers          func([]checker.Target)
	onFleetEchoes    func([]netip.AddrPort)
	onNeedReregister func()
	onTask           func(context.Context, *pb.TaskRequest)
	onExternal       func(*pb.ExternalCheckAssignment)
}

func NewGRPCClient(address string, sec ClientSecurity) (*GRPCClient, error) { //nolint:gocritic // hugeParam: value semantics intentional, the client keeps a snapshot
	conn, err := dialController(address, sec)
	if err != nil {
		return nil, err
	}

	return &GRPCClient{
		address:  address,
		security: sec,
		conn:     conn,
		client:   pb.NewAgentRegistryClient(conn),
	}, nil
}

// maxPeerRecvBytes bounds a single controller message; see the dial option below for why.
const maxPeerRecvBytes = 16 * 1024 * 1024

func dialController(address string, sec ClientSecurity) (*grpc.ClientConn, error) { //nolint:gocritic // hugeParam: value semantics intentional
	opts := []grpc.DialOption{
		/* The narrow peer projection and the fleet's echo endpoints keep a FULL_SYNC to ~120 wire
		   bytes per agent, so gRPC's 4MB default only breaks at tens of thousands of agents; this guard is for the day labels return
		   to the projection or an external fleet grows past that, when the default would wedge every
		   resubscribe permanently. */
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxPeerRecvBytes)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Second,
			Timeout:             5 * time.Second,
			PermitWithoutStream: true,
		}),
	}

	if sec.TLS.InUse() {
		tlsCfg, err := buildClientTLS(sec.TLS)
		if err != nil {
			return nil, err
		}
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	if sec.TokenFile != "" {
		token, err := readBootstrapToken(sec.TokenFile)
		if err != nil {
			return nil, err
		}
		opts = append(opts, grpc.WithPerRPCCredentials(bearerToken{token: token}))
	}

	conn, err := grpc.NewClient(address, opts...)
	if err != nil {
		return nil, fmt.Errorf("connecting to controller: %w", err)
	}
	return conn, nil
}

// buildClientTLS turns the config block into transport TLS towards the controller's external
// gateway. An empty caFile means the system pool: a gateway behind a publicly-trusted cert needs no
// extra file on the host.
func buildClientTLS(t config.AgentTLSConfig) (*tls.Config, error) {
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: t.ServerName,
	}
	if t.CAFile != "" {
		pem, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading agent.tls.caFile: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("agent.tls.caFile %s holds no usable certificates", t.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	if t.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("loading agent client certificate: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	return tlsCfg, nil
}

// readBootstrapToken loads and trims the shared token; an empty file fails HERE with the file name
// in the error, instead of as an opaque Unauthenticated from the gateway.
func readBootstrapToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading bootstrap token: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("bootstrap token file %s is empty", path)
	}
	return token, nil
}

// bearerToken attaches the bootstrap token to every RPC. RequireTransportSecurity is true on
// purpose: gRPC then refuses to send the token over a plaintext connection, making "token leaks
// because someone removed the tls block" a startup error rather than an incident.
type bearerToken struct {
	token string
}

func (b bearerToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

func (b bearerToken) RequireTransportSecurity() bool {
	return true
}

// Reconnect drops the transport and dials the controller again. The controller Service is a
// ClusterIP, so only a fresh connection is load-balanced anew: retrying on the existing one keeps
// the agent pinned to the replica that just refused it for not being the leader.
func (c *GRPCClient) Reconnect() error {
	conn, err := dialController(c.address, c.security)
	if err != nil {
		return err
	}

	c.mu.Lock()
	old := c.conn
	c.conn = conn
	c.client = pb.NewAgentRegistryClient(conn)
	c.mu.Unlock()

	if old != nil {
		_ = old.Close()
	}
	return nil
}

// shouldRedial reports whether err means this connection cannot serve us. A standby answering "not
// the leader" and a dead transport both surface as Unavailable, and a fresh connection through the
// Service fixes both. Every other error leaves the connection alone: redialling would strand any
// in-flight diagnostic task, whose result must go back to the replica that dispatched it.
func shouldRedial(err error) bool {
	if err == nil {
		return false
	}
	st, ok := grpcstatus.FromError(err)
	return ok && st.Code() == codes.Unavailable
}

// standbyRefusals are the controller's answers from a replica that does not hold the lease
// (notLeaderMsg and errLeadershipLost in internal/controller).
var standbyRefusals = []string{"not the leader", "leadership lost"}

// isStandbyRefusal reports whether err is a replica saying it is not the leader. Unlike a dead
// transport it proves the Service answers, and the leader is one redial away, so the caller retries
// at once instead of backing off.
func isStandbyRefusal(err error) bool {
	// Unwrapped by hand: FromError on a wrapped status reports the whole wrapped text as its message.
	var se interface{ GRPCStatus() *grpcstatus.Status }
	if !errors.As(err, &se) {
		return false
	}
	st := se.GRPCStatus()
	return st.Code() == codes.Unavailable && slices.Contains(standbyRefusals, st.Message())
}

// redialIfRefused reconnects when err says this connection cannot serve us; see shouldRedial.
func (c *GRPCClient) redialIfRefused(err error) {
	if !shouldRedial(err) {
		return
	}
	if rerr := c.Reconnect(); rerr != nil {
		slog.Warn("redialling the controller failed", "error", rerr)
	}
}

// isConfigRejection reports whether the controller refused the registration payload itself
// (InvalidArgument): retrying the same payload can never succeed, so the caller must fail
// loudly instead of looping behind a "controller not ready" message.
func isConfigRejection(err error) bool {
	st, ok := grpcstatus.FromError(err)
	return ok && st.Code() == codes.InvalidArgument
}

// stub returns the current registry stub; Reconnect may swap it between calls.
func (c *GRPCClient) stub() pb.AgentRegistryClient {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.client
}

// id returns the agent ID the controller last assigned, read under the same lock reregister writes it.
func (c *GRPCClient) id() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.agentID
}

func (c *GRPCClient) OnPeersUpdate(fn func([]checker.Target)) {
	c.onPeers = fn
}

// OnFleetEchoes registers the handler that receives every registered agent's echo endpoint with each
// Register response and peer update, before the peers themselves; empty from a controller older
// than 2.5.0.
func (c *GRPCClient) OnFleetEchoes(fn func([]netip.AddrPort)) {
	c.onFleetEchoes = fn
}

func (c *GRPCClient) OnNeedReregister(fn func()) {
	c.onNeedReregister = fn
}

// OnTask registers the handler invoked for each on-demand diagnostic task
// received on the WatchTasks stream. The handler is called with the stream
// context so executions abort when the stream (and thus the agent) shuts down.
func (c *GRPCClient) OnTask(fn func(context.Context, *pb.TaskRequest)) {
	c.onTask = fn
}

// OnExternalAssignment registers the handler invoked for each CONTINUOUS external-check assignment
// received on the WatchExternalChecks stream; every message is the agent's COMPLETE assignment.
func (c *GRPCClient) OnExternalAssignment(fn func(*pb.ExternalCheckAssignment)) {
	c.onExternal = fn
}

// Register registers the agent and returns the peer list plus the zone the
// controller resolved for this agent (empty if the controller has no zone).
// own is this agent's listener ports, dialled on peers that reported none.
func (c *GRPCClient) Register(ctx context.Context, info model.AgentInfo, own checker.PeerPorts) ([]checker.Target, string, error) { //nolint:gocritic // hugeParam: AgentInfo is passed by value intentionally
	resp, err := c.stub().Register(ctx, &pb.RegisterRequest{
		Agent: &pb.AgentMeta{
			Id:       info.ID,
			NodeName: info.NodeName,
			PodName:  info.PodName,
			PodIp:    info.PodIP,
			Zone:     info.Zone,
			Labels:   info.Labels,
			// Capabilities gate the controller's dispatch of features an older
			// agent would silently ignore; see model.AgentInfo.Capabilities.
			Capabilities: info.Capabilities,
			// Where peers reach THIS agent; a pre-2.4.0 controller drops them and every peer keeps
			// dialling its own ports, the contract that fleet already runs on.
			HttpPort:    uint32(info.HTTPPort),    //nolint:gosec // the config loader bounds every port to 1..65535
			UdpPort:     uint32(info.UDPPort),     //nolint:gosec // as above
			MetricsPort: uint32(info.MetricsPort), //nolint:gosec // as above
		},
	})
	if err != nil {
		return nil, "", fmt.Errorf("registering agent: %w", err)
	}

	c.mu.Lock()
	c.agentID = resp.GetAgentId()
	c.mu.Unlock()
	if c.onFleetEchoes != nil {
		c.onFleetEchoes(protoToEchoes(resp.GetFleetEchoes(), own))
	}
	return protoToTargets(resp.GetPeers(), own), resp.GetAgent().GetZone(), nil
}

// Deregister tells the controller to remove this agent immediately, so peers
// stop probing its pod IP the moment it starts shutting down instead of waiting
// for TTL eviction. Best-effort: callers should not block shutdown on the error.
func (c *GRPCClient) Deregister(ctx context.Context) error {
	_, err := c.stub().Deregister(ctx, &pb.DeregisterRequest{AgentId: c.id()})
	if err != nil {
		return fmt.Errorf("deregistering agent: %w", err)
	}
	return nil
}

func (c *GRPCClient) StartHeartbeat(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			_, err := c.stub().Heartbeat(ctx, &pb.HeartbeatRequest{
				AgentId:   c.id(),
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

// WatchPeers streams peer-list updates to the OnPeersUpdate handler; own is this agent's listener
// ports, dialled on peers that reported none.
func (c *GRPCClient) WatchPeers(ctx context.Context, own checker.PeerPorts) error {
	stream, err := c.stub().WatchPeers(ctx, &pb.WatchPeersRequest{
		AgentId: c.id(),
	})
	if err != nil {
		return fmt.Errorf("watching peers: %w", err)
	}

	for {
		update, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("receiving peer update: %w", err)
		}

		targets := protoToTargets(update.GetPeers(), own)
		slog.Info("peer update received", "type", update.GetType(), "count", len(targets))

		if c.onFleetEchoes != nil {
			c.onFleetEchoes(protoToEchoes(update.GetFleetEchoes(), own))
		}
		if c.onPeers != nil {
			c.onPeers(targets)
		}
	}
}

// WatchTasks subscribes to the controller's on-demand task stream and invokes
// the OnTask handler for each task received. It mirrors WatchPeers: it returns
// on the first stream error so the caller's reconnect loop can re-subscribe.
func (c *GRPCClient) WatchTasks(ctx context.Context) error {
	stream, err := c.stub().WatchTasks(ctx, &pb.WatchTasksRequest{
		AgentId: c.id(),
	})
	if err != nil {
		return fmt.Errorf("watching tasks: %w", err)
	}

	for {
		task, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("receiving task: %w", err)
		}

		slog.Info("on-demand task received",
			"taskId", task.GetTaskId(),
			"checkType", task.GetCheckType(),
			"target", task.GetTarget().GetNodeName(),
			"plane", task.GetPlane(),
		)

		if c.onTask != nil {
			c.onTask(ctx, task)
		}
	}
}

// WatchExternalChecks subscribes to the controller's continuous external-check assignment stream
// and invokes the OnExternalAssignment handler for each assignment; the controller sends the
// agent's CURRENT assignment immediately on subscribe (an empty one when it has none).
func (c *GRPCClient) WatchExternalChecks(ctx context.Context) error {
	stream, err := c.stub().WatchExternalChecks(ctx, &pb.WatchExternalChecksRequest{
		AgentId: c.id(),
	})
	if err != nil {
		return fmt.Errorf("watching external checks: %w", err)
	}

	for {
		assignment, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("receiving external check assignment: %w", err)
		}

		slog.Info("external check assignment received", "specs", len(assignment.GetSpecs()))

		if c.onExternal != nil {
			c.onExternal(assignment)
		}
	}
}

// ReportTaskResult sends a completed task result back to the controller.
func (c *GRPCClient) ReportTaskResult(ctx context.Context, res *pb.TaskResult) error {
	if _, err := c.stub().ReportTaskResult(ctx, res); err != nil {
		return fmt.Errorf("reporting task result: %w", err)
	}
	return nil
}

func (c *GRPCClient) Close() error {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()

	if conn != nil {
		return conn.Close()
	}
	return nil
}

// protoToTargets maps a peer list onto checker targets. Each peer is dialled on the ports it
// reported; a zero (an agent older than 2.4.0, or a controller that dropped the fields) means this
// agent's own port, the fleet-wide contract such a peer still assumes. Each port falls back on its
// own, so a half-reported peer is not all-or-nothing.
func protoToTargets(peers []*pb.AgentMeta, own checker.PeerPorts) []checker.Target {
	targets := make([]checker.Target, 0, len(peers))
	for _, p := range peers {
		targets = append(targets, checker.Target{
			AgentID:  p.GetId(),
			NodeName: p.GetNodeName(),
			PodIP:    p.GetPodIp(),
			Zone:     p.GetZone(),
			Port:     portOr(p.GetHttpPort(), own.HTTP),
			UDPPort:  portOr(p.GetUdpPort(), own.UDP),
		})
	}
	return targets
}

// protoToEchoes maps the fleet's echo endpoints onto addresses, with protoToTargets' port fallback;
// an address that does not parse is skipped.
func protoToEchoes(echoes []*pb.EchoEndpoint, own checker.PeerPorts) []netip.AddrPort {
	out := make([]netip.AddrPort, 0, len(echoes))
	for _, e := range echoes {
		if ap, ok := echoEndpoint(e.GetAddress(), portOr(e.GetPort(), own.UDP)); ok {
			out = append(out, ap)
		}
	}
	return out
}

// echoEndpoint is addr:port as an echo address, false when addr does not parse or port is out of
// range.
func echoEndpoint(addr string, port int) (netip.AddrPort, bool) {
	ip, err := netip.ParseAddr(addr)
	if err != nil || port < 1 || port > 65535 {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(ip, uint16(port)), true
}

// portOr returns the port a peer reported, or fallback when it reported none.
func portOr(reported uint32, fallback int) int {
	if reported == 0 {
		return fallback
	}
	return int(reported)
}
