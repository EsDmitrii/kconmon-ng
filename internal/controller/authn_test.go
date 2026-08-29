package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// gatewayTestServerName is the DNS SAN issued to the test gateway certificate; clients verify
// against it via tls.Config.ServerName, the same way a fleet behind a load balancer would.
const gatewayTestServerName = "gateway.kconmon.test"

const gatewayTestToken = "correct-horse-battery-staple-9000"

/*
testPKI is a self-signed CA with a gateway server certificate, generated fresh per test and written
under t.TempDir() because the production code paths read FILES — that is part of the contract under
test.
*/
type testPKI struct {
	t      *testing.T
	dir    string
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey

	caFile, serverCertFile, serverKeyFile, tokenFile string
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	dir := t.TempDir()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kconmon-ng test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("creating CA certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parsing CA certificate: %v", err)
	}

	p := &testPKI{t: t, dir: dir, caCert: caCert, caKey: caKey}
	p.caFile = p.writePEM("ca.pem", "CERTIFICATE", caDER)
	p.serverCertFile, p.serverKeyFile = p.issue("server", &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: gatewayTestServerName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{gatewayTestServerName},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	})
	p.tokenFile = filepath.Join(dir, "token")
	if err := os.WriteFile(p.tokenFile, []byte(gatewayTestToken+"\n"), 0o600); err != nil {
		t.Fatalf("writing token file: %v", err)
	}
	return p
}

// issueClient issues a client certificate whose CN is the agent's node name — the identity the
// gateway pins requests against.
func (p *testPKI) issueClient(nodeName string) (certFile, keyFile string) {
	return p.issue("client-"+nodeName, &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: nodeName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
}

func (p *testPKI) issue(name string, tmpl *x509.Certificate) (certFile, keyFile string) {
	p.t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		p.t.Fatalf("generating %s key: %v", name, err)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, &key.PublicKey, p.caKey)
	if err != nil {
		p.t.Fatalf("issuing %s certificate: %v", name, err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		p.t.Fatalf("marshalling %s key: %v", name, err)
	}
	return p.writePEM(name+".pem", "CERTIFICATE", der),
		p.writePEM(name+"-key.pem", "EC PRIVATE KEY", keyDER)
}

func (p *testPKI) writePEM(name, blockType string, der []byte) string {
	p.t.Helper()
	path := filepath.Join(p.dir, name)
	buf := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		p.t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

// gatewayConfig returns the config an operator would write for this PKI; mTLS toggles the client CA.
func (p *testPKI) gatewayConfig(mTLS bool) config.ExternalGatewayConfig {
	gw := config.ExternalGatewayConfig{
		Enabled:            true,
		TLS:                config.GatewayTLSConfig{CertFile: p.serverCertFile, KeyFile: p.serverKeyFile},
		BootstrapTokenFile: p.tokenFile,
	}
	if mTLS {
		gw.TLS.ClientCAFile = p.caFile
	}
	return gw
}

// startGateway serves the SAME GRPCServer service a production Run would register, on a loopback
// TLS listener, and returns its address.
func startGateway(t *testing.T, gw config.ExternalGatewayConfig) (addr string, reg *Registry) {
	t.Helper()
	reg = NewRegistry(30 * time.Second)
	m := metrics.NewPrometheusMetrics("test_gateway", prometheus.NewRegistry())
	svc := NewGRPCServer(reg, m, false, nil, false)

	srv, err := NewExternalGatewayServer(gw)
	if err != nil {
		t.Fatalf("NewExternalGatewayServer: %v", err)
	}
	svc.RegisterService(srv)

	lc := net.ListenConfig{}
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), reg
}

// dialGateway dials with the test CA in the root pool; certFile/keyFile empty means no client cert.
func dialGateway(t *testing.T, p *testPKI, addr, certFile, keyFile string) pb.AgentRegistryClient {
	t.Helper()
	caPEM, err := os.ReadFile(p.caFile)
	if err != nil {
		t.Fatalf("reading test CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("test CA did not parse")
	}
	tlsCfg := &tls.Config{RootCAs: pool, ServerName: gatewayTestServerName, MinVersion: tls.VersionTLS12}
	if certFile != "" {
		cert, pairErr := tls.LoadX509KeyPair(certFile, keyFile)
		if pairErr != nil {
			t.Fatalf("loading client pair: %v", pairErr)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		t.Fatalf("dialling gateway: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return pb.NewAgentRegistryClient(conn)
}

func withToken(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

func registerReq(nodeName string) *pb.RegisterRequest {
	return &pb.RegisterRequest{Agent: &pb.AgentMeta{
		Id:       nodeName + "-" + nodeName,
		NodeName: nodeName,
		PodName:  nodeName,
		PodIp:    "192.0.2.10",
	}}
}

func wantCode(t *testing.T, err error, want codes.Code, op string) {
	t.Helper()
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("%s: error %v is not a gRPC status", op, err)
	}
	if st.Code() != want {
		t.Fatalf("%s: code = %v (%v), want %v", op, st.Code(), err, want)
	}
}

// The bearer token is the gateway's floor: without it (or with the wrong one) nothing answers,
// with it a registration lands in the same registry the in-cluster listener serves.
func TestGatewayBootstrapToken(t *testing.T) {
	p := newTestPKI(t)
	addr, reg := startGateway(t, p.gatewayConfig(false))
	client := dialGateway(t, p, addr, "", "")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	t.Run("missing token", func(t *testing.T) {
		_, err := client.Register(ctx, registerReq("node-a"))
		wantCode(t, err, codes.Unauthenticated, "Register without token")
	})

	t.Run("wrong token", func(t *testing.T) {
		_, err := client.Register(withToken(ctx, "wrong-token-with-enough-length"), registerReq("node-a"))
		wantCode(t, err, codes.Unauthenticated, "Register with wrong token")
	})

	t.Run("malformed authorization", func(t *testing.T) {
		badCtx := metadata.AppendToOutgoingContext(ctx, "authorization", gatewayTestToken)
		_, err := client.Register(badCtx, registerReq("node-a"))
		wantCode(t, err, codes.Unauthenticated, "Register with non-bearer authorization")
	})

	t.Run("correct token registers", func(t *testing.T) {
		resp, err := client.Register(withToken(ctx, gatewayTestToken), registerReq("node-a"))
		if err != nil {
			t.Fatalf("Register with the correct token: %v", err)
		}
		if resp.GetAgentId() == "" {
			t.Error("registration returned an empty agent id")
		}
		if reg.Count() != 1 {
			t.Errorf("registry holds %d agents, want 1: the gateway must share the in-cluster registry", reg.Count())
		}
	})
}

// checkToken is the constant-time comparison itself, table-driven over the metadata shapes a
// client can present.
func TestGatewayCheckToken(t *testing.T) {
	a := &gatewayAuthn{token: []byte(gatewayTestToken)}
	mdCtx := func(vals ...string) context.Context {
		return metadata.NewIncomingContext(context.Background(), metadata.MD{"authorization": vals})
	}

	tests := []struct {
		name string
		ctx  context.Context
		want codes.Code
	}{
		{"no metadata at all", context.Background(), codes.Unauthenticated},
		{"no authorization", metadata.NewIncomingContext(context.Background(), metadata.MD{}), codes.Unauthenticated},
		{"not a bearer", mdCtx(gatewayTestToken), codes.Unauthenticated},
		{"wrong token", mdCtx("Bearer nope-nope-nope-nope"), codes.Unauthenticated},
		{"wrong length", mdCtx("Bearer x"), codes.Unauthenticated},
		{"correct", mdCtx("Bearer " + gatewayTestToken), codes.OK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := a.checkToken(tt.ctx)
			if tt.want == codes.OK {
				if err != nil {
					t.Fatalf("want accept, got %v", err)
				}
				return
			}
			wantCode(t, err, tt.want, "checkToken")
		})
	}
}

// A token proves fleet membership; only the client certificate proves WHICH agent is talking. With
// a client CA configured, a cert for node-1 must not register node-2, and must not subscribe,
// heartbeat or deregister as any agent id outside node-1's own "<node>-<pod>" space — otherwise any
// token holder could take over another agent's peer-list subscription.
func TestGatewayIdentityPinning(t *testing.T) {
	p := newTestPKI(t)
	addr, _ := startGateway(t, p.gatewayConfig(true))
	certFile, keyFile := p.issueClient("node-1")
	client := dialGateway(t, p, addr, certFile, keyFile)

	baseCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ctx := withToken(baseCtx, gatewayTestToken)

	t.Run("matching CN registers", func(t *testing.T) {
		if _, err := client.Register(ctx, registerReq("node-1")); err != nil {
			t.Fatalf("Register as the certified node: %v", err)
		}
	})

	t.Run("mismatched CN is refused", func(t *testing.T) {
		_, err := client.Register(ctx, registerReq("node-2"))
		wantCode(t, err, codes.PermissionDenied, "Register as another node")
	})

	t.Run("heartbeat for own agent id passes authn", func(t *testing.T) {
		// The agent registered above under id "node-1-node-1"; NotFound would still prove the
		// pin passed, but a real heartbeat proves the whole path.
		_, err := client.Heartbeat(ctx, &pb.HeartbeatRequest{AgentId: "node-1-node-1"})
		if err != nil {
			t.Fatalf("Heartbeat as the certified agent: %v", err)
		}
	})

	t.Run("heartbeat for foreign agent id is refused", func(t *testing.T) {
		_, err := client.Heartbeat(ctx, &pb.HeartbeatRequest{AgentId: "node-2-node-2"})
		wantCode(t, err, codes.PermissionDenied, "Heartbeat as another agent")
	})

	t.Run("prefix needs the separator", func(t *testing.T) {
		// "node-1" must not speak for "node-10": the dash join is part of the match.
		_, err := client.Heartbeat(ctx, &pb.HeartbeatRequest{AgentId: "node-10-pod"})
		wantCode(t, err, codes.PermissionDenied, "Heartbeat with a lookalike prefix")
	})

	t.Run("deregister for foreign agent id is refused", func(t *testing.T) {
		_, err := client.Deregister(ctx, &pb.DeregisterRequest{AgentId: "node-2-node-2"})
		wantCode(t, err, codes.PermissionDenied, "Deregister of another agent")
	})

	t.Run("watch stream under own id receives a sync", func(t *testing.T) {
		stream, err := client.WatchPeers(ctx, &pb.WatchPeersRequest{AgentId: "node-1-node-1"})
		if err != nil {
			t.Fatalf("WatchPeers: %v", err)
		}
		update, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv on own subscription: %v", err)
		}
		if update.GetType() != pb.PeerUpdate_FULL_SYNC {
			t.Errorf("first update = %v, want FULL_SYNC", update.GetType())
		}
	})

	t.Run("watch stream under foreign id is refused", func(t *testing.T) {
		stream, err := client.WatchPeers(ctx, &pb.WatchPeersRequest{AgentId: "node-2-node-2"})
		if err != nil {
			t.Fatalf("WatchPeers open: %v", err)
		}
		_, err = stream.Recv()
		if errors.Is(err, io.EOF) {
			t.Fatal("stream ended cleanly; want PermissionDenied")
		}
		wantCode(t, err, codes.PermissionDenied, "WatchPeers as another agent")
	})

	t.Run("no client certificate cannot connect at all", func(t *testing.T) {
		bare := dialGateway(t, p, addr, "", "")
		_, err := bare.Register(ctx, registerReq("node-1"))
		if err == nil {
			t.Fatal("Register without a client certificate must fail when a client CA is configured")
		}
		// The TLS handshake itself fails (RequireAndVerifyClientCert), surfacing as Unavailable —
		// the caller never reaches the interceptors.
		wantCode(t, err, codes.Unavailable, "Register without client cert")
	})
}

// A short shared secret turns the gateway into an online brute-force target, so the authn
// constructor refuses to start with one.
func TestGatewayRefusesShortToken(t *testing.T) {
	p := newTestPKI(t)
	if err := os.WriteFile(p.tokenFile, []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewExternalGatewayServer(p.gatewayConfig(false)); err == nil {
		t.Fatal("a token below the minimum length must fail gateway startup")
	}
}

// The in-cluster plaintext listener must stay byte-identical with the gateway enabled: no TLS, no
// token, same registry. This is the "in-cluster path untouched" pin for the whole milestone.
func TestRunKeepsInClusterListenerPlaintextWithGatewayEnabled(t *testing.T) {
	p := newTestPKI(t)

	grpcPort := freePort(t)
	gwPort := freePort(t)

	cfg := config.DefaultConfig()
	cfg.MetricsPrefix = "test_gateway_run"
	cfg.HTTPPort = freePort(t)
	cfg.MetricsPort = freePort(t)
	cfg.GRPCPort = grpcPort
	cfg.Controller.LeaderElection = false
	cfg.Controller.ExternalGateway = p.gatewayConfig(true)
	cfg.Controller.ExternalGateway.Port = gwPort

	c := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- c.Run(ctx) }()

	plainConn, err := grpc.NewClient(
		fmt.Sprintf("127.0.0.1:%d", grpcPort),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		cancel()
		t.Fatalf("dialling the in-cluster listener: %v", err)
	}
	t.Cleanup(func() { _ = plainConn.Close() })
	plain := pb.NewAgentRegistryClient(plainConn)

	callCtx, callCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer callCancel()

	// Retry while Run's goroutines bind their listeners.
	var lastErr error
	registered := false
	for range 100 {
		if _, lastErr = plain.Register(callCtx, registerReq("in-cluster-node")); lastErr == nil {
			registered = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !registered {
		cancel()
		t.Fatalf("plaintext Register without any credentials must keep working: %v", lastErr)
	}

	// And the gateway on its own port refuses exactly that kind of caller.
	gwPlainConn, err := grpc.NewClient(
		fmt.Sprintf("127.0.0.1:%d", gwPort),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		cancel()
		t.Fatalf("dialling the gateway: %v", err)
	}
	t.Cleanup(func() { _ = gwPlainConn.Close() })
	if _, err := pb.NewAgentRegistryClient(gwPlainConn).Register(callCtx, registerReq("x")); err == nil {
		cancel()
		t.Fatal("a plaintext, tokenless Register must not pass the TLS gateway")
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("Run returned an error on shutdown: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after ctx cancel with the gateway enabled")
	}
}
