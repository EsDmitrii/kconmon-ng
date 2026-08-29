/*
Package-side authentication for the EXTERNAL gateway listener only.

The in-cluster gRPC port authenticates nothing by design — a NetworkPolicy admits only this
release's agent pods — and nothing in this file touches it. The gateway serves the same services to
agents whose only credential is what they carry: a shared bootstrap token proves fleet membership,
and an optional client certificate proves WHICH agent is calling. The token alone cannot tell
agents apart, so token-only mode leaves the subscription-steal attack described in grpc_server.go
open to any token holder; that is the documented v1 trade-off, and the client CA is the fix.
*/
package controller

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// minBootstrapTokenLength refuses tokens too short to be secrets. The token is the ONLY credential
// in token-only mode, so a short one turns the gateway into an online brute-force target.
const minBootstrapTokenLength = 16

// gatewayAuthn holds what the interceptors check on every gateway RPC. The token is read once at
// startup, symmetric with the TLS material: rotating either means restarting the controller.
type gatewayAuthn struct {
	token []byte
	// pinIdentity is set when a client CA is configured: every verified certificate's CN/URI SAN
	// is then matched against the agent identity each request claims.
	pinIdentity bool
}

func newGatewayAuthn(tokenFile string, pinIdentity bool) (*gatewayAuthn, error) {
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("reading bootstrapTokenFile: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if len(token) < minBootstrapTokenLength {
		return nil, fmt.Errorf("bootstrap token in %s is %d characters; refusing tokens shorter than %d "+
			"— it is the gateway's only shared secret", tokenFile, len(token), minBootstrapTokenLength)
	}
	return &gatewayAuthn{token: []byte(token), pinIdentity: pinIdentity}, nil
}

/*
NewExternalGatewayServer builds the TLS gateway listener's grpc.Server: same keepalive contract as
the in-cluster listener (external agents run the same client), plus transport TLS and the authn
interceptors. The caller registers the SAME GRPCServer service instance on it — the gateway shares
the registry, watchers and managers with the in-cluster listener, so an external agent is an
ordinary fleet member the moment it is through the door.

Exported so the agent's end-to-end tests can stand up a real gateway without reaching into
controller internals.
*/
func NewExternalGatewayServer(gw config.ExternalGatewayConfig) (*grpc.Server, error) { //nolint:gocritic // hugeParam: value semantics intentional, config blocks are snapshots
	cert, err := tls.LoadX509KeyPair(gw.TLS.CertFile, gw.TLS.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("loading gateway certificate: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if gw.TLS.ClientCAFile != "" {
		pem, readErr := os.ReadFile(gw.TLS.ClientCAFile)
		if readErr != nil {
			return nil, fmt.Errorf("reading gateway client CA: %w", readErr)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("gateway client CA %s holds no usable certificates", gw.TLS.ClientCAFile)
		}
		tlsCfg.ClientCAs = pool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	}

	authn, err := newGatewayAuthn(gw.BootstrapTokenFile, gw.TLS.ClientCAFile != "")
	if err != nil {
		return nil, fmt.Errorf("external gateway authn: %w", err)
	}

	return grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    10 * time.Second,
			Timeout: 5 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.ChainUnaryInterceptor(authn.unary),
		grpc.ChainStreamInterceptor(authn.stream),
	), nil
}

// agentIDCarrier is any request that claims to speak FOR an agent. Heartbeat, Deregister,
// TaskResult and every Watch* request satisfy it, so a future method carrying an agent_id is
// pinned without anyone remembering to add it here.
type agentIDCarrier interface {
	GetAgentId() string
}

func (a *gatewayAuthn) unary(
	ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler,
) (any, error) {
	if err := a.checkToken(ctx); err != nil {
		return nil, err
	}
	if err := a.checkIdentity(ctx, req); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func (a *gatewayAuthn) stream(
	srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler,
) error {
	if err := a.checkToken(ss.Context()); err != nil {
		return err
	}
	// The request message of a server-streaming RPC is not visible at interception time; it
	// arrives through RecvMsg, so the identity check wraps that.
	return handler(srv, &authnStream{ServerStream: ss, authn: a})
}

// authnStream pins the identity claimed inside each received message to the caller's certificate.
// Without this, any token holder could open WatchPeers under another agent's id and take over its
// peer-list subscription — the exact attack the in-cluster listener leaves to the NetworkPolicy.
type authnStream struct {
	grpc.ServerStream
	authn *gatewayAuthn
}

func (s *authnStream) RecvMsg(m any) error {
	if err := s.ServerStream.RecvMsg(m); err != nil {
		return err
	}
	return s.authn.checkIdentity(s.Context(), m)
}

// checkToken requires "authorization: Bearer <token>" metadata and compares in constant time.
// Unauthenticated on any failure: the caller has not proven fleet membership at all.
func (a *gatewayAuthn) checkToken(ctx context.Context) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing request metadata")
	}
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return status.Error(codes.Unauthenticated, "missing bootstrap token")
	}
	got, ok := strings.CutPrefix(vals[0], "Bearer ")
	if !ok {
		return status.Error(codes.Unauthenticated, "authorization metadata is not a bearer token")
	}
	// ConstantTimeCompare returns 0 on length mismatch without a data-dependent branch; the
	// token's LENGTH is not a secret, its bytes are.
	if subtle.ConstantTimeCompare([]byte(got), a.token) != 1 {
		return status.Error(codes.Unauthenticated, "invalid bootstrap token")
	}
	return nil
}

/*
checkIdentity pins what the message CLAIMS to the certificate the transport VERIFIED.

Register asserts a node name, so the cert must carry exactly that name. Everything after Register
speaks as an agent_id, which the fleet builds as "<nodeName>-<podName>" (internal/agent/identity.go)
— the pod half is not in the cert, so the id must merely EXTEND the certified node name with a "-"
separated suffix. A message that carries no identity (e.g. an event subscription) passes on the
token alone. In token-only mode this whole check is off: that is the documented v1 trade-off.
*/
func (a *gatewayAuthn) checkIdentity(ctx context.Context, msg any) error {
	if !a.pinIdentity {
		return nil
	}
	switch m := msg.(type) {
	case *pb.RegisterRequest:
		return a.pinNodeName(ctx, m.GetAgent().GetNodeName())
	case agentIDCarrier:
		return a.pinAgentID(ctx, m.GetAgentId())
	default:
		return nil
	}
}

func (a *gatewayAuthn) pinNodeName(ctx context.Context, nodeName string) error {
	ids, err := certIdentities(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if id == nodeName && nodeName != "" {
			return nil
		}
	}
	return status.Errorf(codes.PermissionDenied,
		"client certificate identity %v does not match the registering node name %q", ids, nodeName)
}

func (a *gatewayAuthn) pinAgentID(ctx context.Context, agentID string) error {
	ids, err := certIdentities(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		// The separator is part of the match: a cert for "node-1" must not speak for "node-10".
		// A node name that is itself a prefix of another ("node-1" vs "node-1-0") stays ambiguous;
		// that is inherent to the "-" join and documented in external-agents docs.
		if id != "" && strings.HasPrefix(agentID, id+"-") {
			return nil
		}
	}
	return status.Errorf(codes.PermissionDenied,
		"client certificate identity %v may not act as agent %q", ids, agentID)
}

// certIdentities returns the names the verified client certificate may speak as: its CN and every
// URI SAN, verbatim. The transport already verified the chain (RequireAndVerifyClientCert), so a
// missing certificate here is a broken invariant, not a policy decision.
func certIdentities(ctx context.Context) ([]string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no peer information on the connection")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
		return nil, status.Error(codes.Unauthenticated, "identity pinning requires a client certificate")
	}
	leaf := tlsInfo.State.PeerCertificates[0]
	ids := make([]string, 0, 1+len(leaf.URIs))
	if leaf.Subject.CommonName != "" {
		ids = append(ids, leaf.Subject.CommonName)
	}
	for _, u := range leaf.URIs {
		ids = append(ids, u.String())
	}
	return ids, nil
}
