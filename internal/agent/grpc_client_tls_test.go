package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/controller"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

const clientTestToken = "agent-side-bootstrap-token-42"

/*
clientTestPKI mirrors the controller package's test PKI in miniature: a fresh CA, a gateway server
cert and one client cert per node name, all written to files because file paths ARE the client's
config surface. Duplicated rather than shared: test helpers cannot be imported across packages,
and a production package only for test scaffolding would be worse.
*/
type clientTestPKI struct {
	t      *testing.T
	dir    string
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey

	caFile, serverCertFile, serverKeyFile, tokenFile string
}

func newClientTestPKI(t *testing.T) *clientTestPKI {
	t.Helper()
	dir := t.TempDir()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kconmon-ng agent test CA"},
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

	p := &clientTestPKI{t: t, dir: dir, caCert: caCert, caKey: caKey}
	p.caFile = p.writePEM("ca.pem", "CERTIFICATE", caDER)
	p.serverCertFile, p.serverKeyFile = p.issue("server", &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "gw.agent.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"gw.agent.test"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	})
	p.tokenFile = filepath.Join(dir, "token")
	if err := os.WriteFile(p.tokenFile, []byte(clientTestToken+"\n"), 0o600); err != nil {
		t.Fatalf("writing token file: %v", err)
	}
	return p
}

func (p *clientTestPKI) issueClient(nodeName string) (certFile, keyFile string) {
	return p.issue("client-"+nodeName, &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: nodeName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
}

func (p *clientTestPKI) issue(name string, tmpl *x509.Certificate) (certFile, keyFile string) {
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

func (p *clientTestPKI) writePEM(name, blockType string, der []byte) string {
	p.t.Helper()
	path := filepath.Join(p.dir, name)
	buf := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		p.t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

// startTestGateway stands up the REAL controller gateway (TLS + authn interceptors + shared
// service) on loopback and returns its address — the agent client below is exercised end-to-end
// against exactly what production Run() serves.
func startTestGateway(t *testing.T, p *clientTestPKI, mTLS bool) (addr string, reg *controller.Registry) {
	t.Helper()
	reg = controller.NewRegistry(30 * time.Second)
	m := metrics.NewPrometheusMetrics("test_agent_tls", prometheus.NewRegistry())
	svc := controller.NewGRPCServer(reg, m, false, nil, false)

	gw := config.ExternalGatewayConfig{
		Enabled:            true,
		TLS:                config.GatewayTLSConfig{CertFile: p.serverCertFile, KeyFile: p.serverKeyFile},
		BootstrapTokenFile: p.tokenFile,
	}
	if mTLS {
		gw.TLS.ClientCAFile = p.caFile
	}
	srv, err := controller.NewExternalGatewayServer(gw)
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

// The full external-agent client path: TLS to the gateway, client certificate, bearer token from a
// file — through the unmodified GRPCClient API the in-cluster agent uses, Reconnect included.
func TestGRPCClientDialsTLSGatewayEndToEnd(t *testing.T) {
	p := newClientTestPKI(t)
	addr, reg := startTestGateway(t, p, true)
	certFile, keyFile := p.issueClient("edge-host-1")

	sec := ClientSecurity{
		TLS: config.AgentTLSConfig{
			CAFile:     p.caFile,
			CertFile:   certFile,
			KeyFile:    keyFile,
			ServerName: "gw.agent.test",
		},
		TokenFile: p.tokenFile,
	}
	client, err := NewGRPCClient(addr, sec)
	if err != nil {
		t.Fatalf("NewGRPCClient over TLS: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// The identity matches the certificate CN, as resolveIdentity would produce on that host.
	info := model.AgentInfo{
		ID: "edge-host-1-edge-host-1", NodeName: "edge-host-1", PodName: "edge-host-1", PodIP: "192.0.2.7",
	}
	if _, _, regErr := client.Register(ctx, info, 8080); regErr != nil {
		t.Fatalf("Register through the TLS gateway: %v", regErr)
	}
	if reg.Count() != 1 {
		t.Fatalf("registry holds %d agents after a gateway registration, want 1", reg.Count())
	}

	// A node name OUTSIDE the certificate must be refused: the client carries the pinning
	// material, the gateway enforces it.
	foreign := model.AgentInfo{
		ID: "other-node-x", NodeName: "other-node", PodName: "x", PodIP: "192.0.2.8",
	}
	_, _, err = client.Register(ctx, foreign, 8080)
	if st, ok := grpcstatus.FromError(err); !ok || st.Code() != codes.PermissionDenied {
		t.Fatalf("Register outside the certificate = %v, want PermissionDenied", err)
	}

	// Reconnect must take the same TLS+token path: it is what the reregister loop calls after a
	// controller failover, and a plaintext fallback here would strand every external agent.
	if err := client.Reconnect(); err != nil {
		t.Fatalf("Reconnect over TLS: %v", err)
	}
	if _, _, err := client.Register(ctx, info, 8080); err != nil {
		t.Fatalf("Register after Reconnect: %v", err)
	}
}

// The wrong token must be refused before any identity logic runs, and the failure mode must be the
// gRPC code the reregister loop treats as fatal-ish logging, not a silent retry storm.
func TestGRPCClientTLSGatewayRejectsWrongToken(t *testing.T) {
	p := newClientTestPKI(t)
	addr, _ := startTestGateway(t, p, false)

	badTokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(badTokenFile, []byte("not-the-right-token-at-all\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sec := ClientSecurity{
		TLS:       config.AgentTLSConfig{CAFile: p.caFile, ServerName: "gw.agent.test"},
		TokenFile: badTokenFile,
	}
	client, err := NewGRPCClient(addr, sec)
	if err != nil {
		t.Fatalf("NewGRPCClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	info := model.AgentInfo{ID: "h-h", NodeName: "h", PodName: "h", PodIP: "192.0.2.9"}
	_, _, err = client.Register(ctx, info, 8080)
	if st, ok := grpcstatus.FromError(err); !ok || st.Code() != codes.Unauthenticated {
		t.Fatalf("Register with the wrong token = %v, want Unauthenticated", err)
	}
}

// An empty or missing token file is a startup error naming the file, not an opaque refusal from
// the far end an operator has to tcpdump.
func TestGRPCClientRefusesEmptyTokenFile(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(empty, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sec := ClientSecurity{
		TLS:       config.AgentTLSConfig{ServerName: "gw.agent.test"},
		TokenFile: empty,
	}
	if _, err := NewGRPCClient("127.0.0.1:1", sec); err == nil {
		t.Fatal("an empty token file must fail the dial")
	}

	sec.TokenFile = filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := NewGRPCClient("127.0.0.1:1", sec); err == nil {
		t.Fatal("a missing token file must fail the dial")
	}
}

// RequireTransportSecurity is the property that keeps the token off plaintext connections: gRPC
// refuses to attach these credentials without TLS. Pinned so nobody "fixes" a dial error by
// flipping it.
func TestBearerTokenRequiresTransportSecurity(t *testing.T) {
	if !(bearerToken{token: "x"}).RequireTransportSecurity() {
		t.Fatal("bearerToken must require transport security")
	}
}
