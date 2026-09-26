package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
)

// startGatewayWithLimits is startGateway with the connection limits a test wants to observe in
// seconds rather than minutes.
func startGatewayWithLimits(t *testing.T, gw config.ExternalGatewayConfig, limits gatewayLimits) string { //nolint:gocritic // hugeParam: test helper mirrors NewExternalGatewayServer
	t.Helper()
	reg := NewRegistry(30 * time.Second)
	m := metrics.NewPrometheusMetrics("test_gateway_limits", prometheus.NewRegistry())
	svc := NewGRPCServer(reg, m, false, nil, false)

	srv, err := newExternalGatewayServer(gw, limits)
	if err != nil {
		t.Fatalf("newExternalGatewayServer: %v", err)
	}
	svc.RegisterGatewayService(srv)

	lc := net.ListenConfig{}
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// http2Hello is the HTTP/2 client preface followed by an empty SETTINGS frame: everything a client
// sends before its first RPC, and all an idle squatter ever sends.
var http2Hello = append([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"), 0, 0, 0, 4, 0, 0, 0, 0, 0)

// dialSquatter completes TLS and the HTTP/2 preface and then never sends an RPC or a token.
func dialSquatter(t *testing.T, p *testPKI, addr string) (*tls.Conn, error) {
	t.Helper()
	caPEM, err := os.ReadFile(p.caFile)
	if err != nil {
		t.Fatalf("reading test CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("test CA did not parse")
	}
	d := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 5 * time.Second},
		Config: &tls.Config{
			RootCAs: pool, ServerName: gatewayTestServerName,
			NextProtos: []string{"h2"}, MinVersion: tls.VersionTLS12,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	conn, _ := c.(*tls.Conn)
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Write(http2Hello); err != nil {
		return nil, err
	}
	// The server sends its SETTINGS frame once its side of the handshake has returned, so reading
	// it proves the guard saw the handshake finish; a server that refuses the connection closes it
	// instead, and the refusal shows here rather than on some later read.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 512)); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Time{})
	return conn, nil
}

// closedWithin reports whether the server closed conn within d, discarding whatever it sends.
func closedWithin(conn net.Conn, d time.Duration) bool {
	_ = conn.SetReadDeadline(time.Now().Add(d))
	buf := make([]byte, 4096)
	for {
		if _, err := conn.Read(buf); err != nil {
			ne, ok := errors.AsType[net.Error](err)
			return !ok || !ne.Timeout()
		}
	}
}

func testGatewayLimits() gatewayLimits {
	return gatewayLimits{
		handshakeTimeout: 5 * time.Second,
		authDeadline:     30 * time.Second,
		maxPending:       16,
		maxIdle:          time.Hour,
	}
}

// A client that completes TLS and the HTTP/2 preface but never proves it holds the token is
// dropped at the auth deadline. Before, it was held for as long as it answered pings, and a few
// thousand of them exhausted the controller's memory.
func TestGatewayDropsAConnectionThatNeverAuthenticates(t *testing.T) {
	p := newTestPKI(t)
	limits := testGatewayLimits()
	// Long enough that a client under -race still gets its first RPC in before the deadline.
	limits.authDeadline = 1500 * time.Millisecond
	addr := startGatewayWithLimits(t, p.gatewayConfig(false), limits)

	conn, err := dialSquatter(t, p, addr)
	if err != nil {
		t.Fatalf("the first connection must be admitted: %v", err)
	}
	if !closedWithin(conn, 4*limits.authDeadline) {
		t.Fatal("an unauthenticated idle connection is still open well past the auth deadline")
	}

	// A failing RPC is activity, not authentication: it does not buy the connection more time.
	cc := dialGatewayConn(t, p, addr, "", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = pb.NewAgentRegistryClient(cc).Register(withToken(ctx, "wrong-token-with-enough-length"), registerReq("node-a"))
	wantCode(t, err, codes.Unauthenticated, "Register with a wrong token")
	waitCtx, waitCancel := context.WithTimeout(ctx, 4*limits.authDeadline)
	defer waitCancel()
	if !cc.WaitForStateChange(waitCtx, connectivity.Ready) {
		t.Fatal("a connection whose only RPC failed the token check is still open past the auth deadline")
	}
}

// The cap on connections that have not authenticated yet holds, but past it the gateway closes the
// oldest pending connection rather than refusing the new one: refusing let a few idle sockets lock
// out every agent that had to reconnect.
func TestGatewayCapsConnectionsThatHaveNotAuthenticated(t *testing.T) {
	p := newTestPKI(t)
	limits := testGatewayLimits()
	limits.maxPending = 3
	addr := startGatewayWithLimits(t, p.gatewayConfig(false), limits)

	held := make([]*tls.Conn, 0, limits.maxPending)
	for i := range limits.maxPending {
		conn, err := dialSquatter(t, p, addr)
		if err != nil {
			t.Fatalf("connection %d of %d must be admitted: %v", i+1, limits.maxPending, err)
		}
		held = append(held, conn)
	}
	if _, err := dialSquatter(t, p, addr); err != nil {
		t.Fatalf("connection %d past a cap of %d must be admitted: %v", limits.maxPending+1, limits.maxPending, err)
	}
	if !closedWithin(held[0], 5*time.Second) {
		t.Fatal("the oldest unauthenticated connection is still open past the cap")
	}
	for _, c := range held[1:] {
		if closedWithin(c, 100*time.Millisecond) {
			t.Fatal("a connection other than the oldest was closed at the cap")
		}
	}
}

// An agent that authenticated is a fleet member: it outlives the auth deadline and stops counting
// against the cap, so squatters cannot push it out.
func TestGatewayKeepsAnAuthenticatedConnection(t *testing.T) {
	p := newTestPKI(t)
	limits := testGatewayLimits()
	limits.authDeadline = 1500 * time.Millisecond
	limits.maxPending = 1
	addr := startGatewayWithLimits(t, p.gatewayConfig(false), limits)

	cc := dialGatewayConn(t, p, addr, "", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pb.NewAgentRegistryClient(cc).Register(withToken(ctx, gatewayTestToken), registerReq("node-a")); err != nil {
		t.Fatalf("Register with the correct token: %v", err)
	}

	waitCtx, waitCancel := context.WithTimeout(ctx, 2*limits.authDeadline)
	defer waitCancel()
	if cc.WaitForStateChange(waitCtx, connectivity.Ready) {
		t.Fatalf("an authenticated connection left READY (now %v) at the auth deadline", cc.GetState())
	}
	if _, err := dialSquatter(t, p, addr); err != nil {
		t.Fatalf("the authenticated connection still holds the only unauthenticated slot: %v", err)
	}
}

// keepalive MaxConnectionIdle: a connection with no RPC in flight for that long is closed; agents
// always hold their Watch streams, so only a caller that stopped using the connection is affected.
func TestGatewayClosesAnIdleConnection(t *testing.T) {
	p := newTestPKI(t)
	limits := testGatewayLimits()
	limits.maxIdle = 500 * time.Millisecond
	addr := startGatewayWithLimits(t, p.gatewayConfig(false), limits)

	cc := dialGatewayConn(t, p, addr, "", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pb.NewAgentRegistryClient(cc).Register(withToken(ctx, gatewayTestToken), registerReq("node-a")); err != nil {
		t.Fatalf("Register with the correct token: %v", err)
	}
	waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
	defer waitCancel()
	if !cc.WaitForStateChange(waitCtx, connectivity.Ready) {
		t.Fatal("an idle connection is still open well past MaxConnectionIdle")
	}
}

// A TCP connection that never starts TLS is dropped at the handshake timeout, which is much
// shorter than grpc's 120s default.
func TestGatewayBoundsTheHandshake(t *testing.T) {
	p := newTestPKI(t)
	limits := testGatewayLimits()
	limits.handshakeTimeout = 500 * time.Millisecond
	addr := startGatewayWithLimits(t, p.gatewayConfig(false), limits)

	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if !closedWithin(conn, 5*time.Second) {
		t.Fatal("a connection that never sent a ClientHello is still open past the handshake timeout")
	}
}

func TestDefaultGatewayLimitsAreBounded(t *testing.T) {
	l := defaultGatewayLimits
	if l.handshakeTimeout <= 0 || l.authDeadline <= 0 || l.maxPending <= 0 || l.maxIdle <= 0 {
		t.Fatalf("every gateway limit must be set, got %+v", l)
	}
	// An agent authenticates with its first RPC, milliseconds after the handshake.
	if l.authDeadline > time.Minute {
		t.Errorf("authDeadline %v lets a squatter hold a slot for over a minute", l.authDeadline)
	}
}

// startGatewayOnLoopbacks serves one gateway on 127.0.0.1 and on [::1], so a test has two
// distinct client sources. It skips when the host has no IPv6 loopback.
func startGatewayOnLoopbacks(t *testing.T, gw config.ExternalGatewayConfig, limits gatewayLimits) (v4, v6 string) { //nolint:gocritic // hugeParam: test helper mirrors NewExternalGatewayServer
	t.Helper()
	reg := NewRegistry(30 * time.Second)
	m := metrics.NewPrometheusMetrics("test_gateway_loopbacks", prometheus.NewRegistry())
	svc := NewGRPCServer(reg, m, false, nil, false)
	srv, err := newExternalGatewayServer(gw, limits)
	if err != nil {
		t.Fatalf("newExternalGatewayServer: %v", err)
	}
	svc.RegisterGatewayService(srv)

	lc := net.ListenConfig{}
	lis6, err := lc.Listen(context.Background(), "tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	lis4, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(lis4) }()
	go func() { _ = srv.Serve(lis6) }()
	t.Cleanup(srv.Stop)
	return lis4.Addr().String(), lis6.Addr().String()
}

/*
Idle TCP sockets need no TLS and no token. Before, the first maxPending of them from ONE address
held every unauthenticated slot until the handshake timeout, and the gateway refused every other
connection, so no external agent could reconnect after a controller rollout while the flood went
on. Now a new connection pushes out an older pending one of the source holding the most instead of
being refused, so agents get in from another address and from the same one.
*/
func TestGatewayAdmitsAgentsPastAFloodOfIdleSocketsFromOneAddress(t *testing.T) {
	p := newTestPKI(t)
	limits := testGatewayLimits()
	limits.handshakeTimeout = time.Minute // the squatters must outlive the test
	limits.maxPending = 8
	v4, v6 := startGatewayOnLoopbacks(t, p.gatewayConfig(false), limits)

	squatters := make([]net.Conn, 0, 2*limits.maxPending)
	d := net.Dialer{Timeout: 5 * time.Second}
	for range 2 * limits.maxPending {
		conn, err := d.DialContext(context.Background(), "tcp", v4)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		squatters = append(squatters, conn)
	}
	// Every squatter past the cap is closed by the server; once they are, the rest hold their slots.
	wantClosed := len(squatters) - limits.maxPending
	closed := make([]bool, len(squatters))
	nClosed := 0
	for deadline := time.Now().Add(5 * time.Second); nClosed < wantClosed && time.Now().Before(deadline); {
		for i, c := range squatters {
			if !closed[i] && closedWithin(c, 20*time.Millisecond) {
				closed[i] = true
				nClosed++
			}
		}
	}
	if nClosed != wantClosed {
		t.Errorf("%d of %d idle sockets from one address were closed, want %d (cap %d)",
			nClosed, len(squatters), wantClosed, limits.maxPending)
	}

	for _, addr := range []string{v6, v4} {
		cc := dialGatewayConn(t, p, addr, "", "")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := pb.NewAgentRegistryClient(cc).Register(withToken(ctx, gatewayTestToken), registerReq("node-a"))
		cancel()
		if err != nil {
			t.Errorf("Register via %s while one address floods the gateway with idle sockets: %v", addr, err)
		}
	}
}

/*
Behind externalTrafficPolicy Cluster an agent shares its source address with every other client
that came through the same node. Bare TCP sockets from that address, far more of them than the
gateway holds, push out one another and leave alone the agent that finished TLS and is about to
send its first RPC: displacing it takes a TLS handshake per connection.
*/
func TestGatewayKeepsAnAgentPastTLSThroughBareSocketsFromItsAddress(t *testing.T) {
	p := newTestPKI(t)
	limits := testGatewayLimits()
	limits.handshakeTimeout = time.Minute // the bare sockets must outlive the test
	limits.maxPending = 4
	addr := startGatewayWithLimits(t, p.gatewayConfig(false), limits)

	agent, err := dialSquatter(t, p, addr)
	if err != nil {
		t.Fatalf("the agent's connection must be admitted: %v", err)
	}
	bare := make([]net.Conn, 0, 3*limits.maxPending)
	d := net.Dialer{Timeout: 5 * time.Second}
	for range 3 * limits.maxPending {
		conn, err := d.DialContext(context.Background(), "tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		bare = append(bare, conn)
	}
	wantClosed := len(bare) + 1 - limits.maxPending
	closed := make([]bool, len(bare))
	nClosed := 0
	for deadline := time.Now().Add(5 * time.Second); nClosed < wantClosed && time.Now().Before(deadline); {
		for i, c := range bare {
			if !closed[i] && closedWithin(c, 20*time.Millisecond) {
				closed[i] = true
				nClosed++
			}
		}
	}
	if nClosed != wantClosed {
		t.Errorf("%d of %d bare sockets were closed, want %d (cap %d)", nClosed, len(bare), wantClosed, limits.maxPending)
	}
	if closedWithin(agent, 100*time.Millisecond) {
		t.Fatal("bare TCP sockets from the agent's own address pushed out its connection after TLS")
	}
}

// pipeConn is a net.Conn with a chosen remote address.
type pipeConn struct {
	net.Conn
	remote net.Addr
}

func (c *pipeConn) RemoteAddr() net.Addr { return c.remote }

func newPipeConn(t *testing.T, remote string) *pipeConn {
	t.Helper()
	ap, err := netip.ParseAddrPort(remote)
	if err != nil {
		t.Fatalf("parse %q: %v", remote, err)
	}
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return &pipeConn{Conn: a, remote: net.TCPAddrFromAddrPort(ap)}
}

func isClosedPipe(c *pipeConn) bool {
	_ = c.SetWriteDeadline(time.Now().Add(time.Millisecond))
	_, err := c.Write([]byte{0})
	return errors.Is(err, io.ErrClosedPipe)
}

/*
The guard's eviction order. Nothing is closed while the gateway is below maxPending, however many of
the pending slots one source holds: behind externalTrafficPolicy Cluster every client arrives from a
node IP, and a per-source cap of 32 let about 400 bare TCP connects a second through one node push
out every agent handshake arriving through it. At the cap the source holding the most loses a slot,
the new connection counted, so a flooding source pushes out its own first; within that source a
connection still in its TLS handshake goes before one past it, so bare sockets displace each other
and not an agent that is already sending its first RPC. One IPv6 /64 counts as one source.
*/
func TestGatewayConnGuardEvictsTheOldestSlotOfTheHeaviestSource(t *testing.T) {
	g := newGatewayConnGuard(gatewayLimits{authDeadline: time.Minute, maxPending: 5})
	slots := map[*pipeConn]*gatewaySlot{}
	admit := func(remote string) *pipeConn {
		t.Helper()
		c := newPipeConn(t, remote)
		s := g.admit(c)
		t.Cleanup(s.closed)
		slots[c] = s
		return c
	}
	open := func(step string, want map[*pipeConn]bool) {
		t.Helper()
		for c, wantOpen := range want {
			if isClosedPipe(c) == wantOpen {
				t.Fatalf("%s: connection from %s open=%v, want %v", step, c.remote, !wantOpen, wantOpen)
			}
		}
	}

	a1 := admit("192.0.2.1:1001")
	a2 := admit("192.0.2.1:1002")
	a3 := admit("192.0.2.1:1003")
	a4 := admit("192.0.2.1:1004")
	a5 := admit("192.0.2.1:1005")
	open("one source below the global cap", map[*pipeConn]bool{a1: true, a2: true, a3: true, a4: true, a5: true})

	slots[a2].handshakeDone()
	a6 := admit("192.0.2.1:1006") // full: the oldest still in its handshake goes, not a2
	a7 := admit("192.0.2.1:1007")
	open("past TLS", map[*pipeConn]bool{a1: false, a3: false, a2: true, a4: true, a5: true, a6: true, a7: true})

	c1 := admit("198.51.100.7:3001") // full, 192.0.2.1 holds the most
	b1 := admit("[2001:db8::1]:2001")
	b2 := admit("[2001:db8::2]:2002") // same /64, same source
	open("full gateway", map[*pipeConn]bool{a4: false, a5: false, a6: false, a2: true, a7: true, c1: true, b1: true, b2: true})

	b3 := admit("[2001:db8::3]:2003") // full again, and this /64 would now hold the most
	open("flooding source", map[*pipeConn]bool{b1: false, a2: true, a7: true, c1: true, b2: true, b3: true})

	d1 := admit("203.0.113.9:4001") // full, 192.0.2.1 and the /64 tie at 2: 192.0.2.1 holds the oldest slot
	open("tie", map[*pipeConn]bool{a7: false, a2: true, c1: true, b2: true, b3: true, d1: true})

	a8 := admit("192.0.2.1:1008") // full, ties the /64 at 2: its own source, with no handshake in flight
	open("nothing in its handshake", map[*pipeConn]bool{a2: false, c1: true, b2: true, b3: true, d1: true, a8: true})

	if got := g.pendingCount(); got != 5 {
		t.Fatalf("pending = %d, want 5", got)
	}
}

func (g *gatewayConnGuard) pendingCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.order.Len()
}
