package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

func TestProbeServerUDP(t *testing.T) {
	srv := NewProbeServer(0)

	lc := net.ListenConfig{}
	listener, err := lc.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.listener = listener
	srv.running.Store(true)
	go srv.serve(srv.listener)
	defer func() { _ = srv.Close() }()

	addr := listener.LocalAddr()

	dialer := net.Dialer{}
	conn, err := dialer.DialContext(context.Background(), "udp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	for seq := range uint32(3) {
		payload := make([]byte, 4)
		binary.BigEndian.PutUint32(payload, seq)

		if _, err := conn.Write(payload); err != nil {
			t.Fatal(err)
		}

		if err := conn.SetReadDeadline(time.Now().Add(1 * time.Second)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 1024)
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("read error for seq %d: %v", seq, err)
		}

		if n < 4 {
			t.Fatalf("response too short: %d bytes", n)
		}

		respSeq := binary.BigEndian.Uint32(buf[:4])
		if respSeq != seq {
			t.Errorf("expected seq %d, got %d", seq, respSeq)
		}
	}
}

func TestProbeServerShortPacket(t *testing.T) {
	srv := NewProbeServer(0)

	lc2 := net.ListenConfig{}
	listener2, err := lc2.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.listener = listener2
	srv.running.Store(true)
	go srv.serve(srv.listener)
	defer func() { _ = srv.Close() }()

	addr := listener2.LocalAddr()

	dialer2 := net.Dialer{}
	conn, err := dialer2.DialContext(context.Background(), "udp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	// Send a packet shorter than 4 bytes -- should be silently ignored
	if _, writeErr := conn.Write([]byte{0x01, 0x02}); writeErr != nil {
		t.Fatal(writeErr)
	}

	if deadlineErr := conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); deadlineErr != nil {
		t.Fatal(deadlineErr)
	}
	buf := make([]byte, 1024)
	_, err = conn.Read(buf)
	if err == nil {
		t.Error("expected timeout for short packet, got response")
	}
}

// A pmtu probe sends up to the interface MTU, jumbo included, and needs its seq back. The server
// reads the whole datagram rather than relying on a truncated read keeping the first four bytes,
// which holds on Linux and Darwin but is an error on other stacks.
func TestProbeServerEchoesJumboDatagram(t *testing.T) {
	srv := NewProbeServer(0)
	lc := net.ListenConfig{}
	listener, err := lc.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.listener = listener
	srv.running.Store(true)
	go srv.serve(srv.listener)
	defer func() { _ = srv.Close() }()

	dialer := net.Dialer{}
	conn, err := dialer.DialContext(context.Background(), "udp", listener.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	payload := make([]byte, 8972) // 9000 bytes on the wire
	binary.BigEndian.PutUint32(payload, 42)
	if _, err = conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	resp := make([]byte, 16)
	n, err := conn.Read(resp)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 || binary.BigEndian.Uint32(resp[:4]) != 42 {
		t.Fatalf("reply = %v (%d bytes), want the 4-byte seq 42", resp[:n], n)
	}
}

// A truncated read keeps the seq on Linux and Darwin, so only this catches a revert of the buffer.
func TestProbeReadBufferHoldsTheLargestIPv4Datagram(t *testing.T) {
	if probeReadBuffer < 65535 {
		t.Fatalf("probeReadBuffer = %d, want at least 65535 so a full-size pmtu probe is read whole", probeReadBuffer)
	}
}

// countingConn counts datagrams the echo server reads and the replies it writes.
type countingConn struct {
	net.PacketConn
	reads, writes atomic.Int64
}

func (c *countingConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(p)
	if err == nil {
		c.reads.Add(1)
	}
	return n, addr, err
}

// WriteTo counts before sending: the test reads the counter as soon as the reply arrives.
func (c *countingConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.writes.Add(1)
	n, err := c.PacketConn.WriteTo(p, addr)
	if err != nil {
		c.writes.Add(-1)
	}
	return n, err
}

/*
One datagram forged from one agent's echo socket to another's must not bounce between them forever.
The receiver knows the sender's echo endpoint from its planned peers or, from a 2.5.0 controller, from
the whole fleet; echo ports differ per agent since 2.4.0, so the two servers use different ports.
*/
func TestProbeServerBreaksAnEchoLoopBetweenAgents(t *testing.T) {
	for _, tc := range []struct {
		name  string
		learn func(srv *ProbeServer, self, other netip.AddrPort)
	}{
		{"planned peer", func(srv *ProbeServer, _, other netip.AddrPort) {
			srv.SetPeerEchoAddrs([]netip.AddrPort{other})
		}},
		{"fleet stranger", func(srv *ProbeServer, self, other netip.AddrPort) {
			srv.SetFleetEchoAddrs([]netip.AddrPort{self, other})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lc := net.ListenConfig{}
			px, err := lc.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			pe, err := lc.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			connX, connE := &countingConn{PacketConn: px}, &countingConn{PacketConn: pe}
			for _, s := range []struct {
				conn        *countingConn
				self, other net.Addr
			}{{connX, px.LocalAddr(), pe.LocalAddr()}, {connE, pe.LocalAddr(), px.LocalAddr()}} {
				srv := NewProbeServer(s.self.(*net.UDPAddr).Port)
				tc.learn(srv, s.self.(*net.UDPAddr).AddrPort(), s.other.(*net.UDPAddr).AddrPort())
				srv.listener = s.conn
				srv.running.Store(true)
				go srv.serve(srv.listener)
				t.Cleanup(func() { _ = srv.Close() })
			}

			if _, err = px.WriteTo([]byte{0, 0, 0, 7}, pe.LocalAddr()); err != nil {
				t.Fatal(err)
			}
			// A probe from an ordinary source behind it: E serves datagrams in order, so the probe's echo
			// means the forged datagram was handled first.
			probe, err := (&net.Dialer{}).DialContext(context.Background(), "udp", pe.LocalAddr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = probe.Close() }()
			if _, err = probe.Write([]byte{0, 0, 0, 8}); err != nil {
				t.Fatal(err)
			}
			_ = probe.SetReadDeadline(time.Now().Add(time.Second))
			if _, err = probe.Read(make([]byte, 16)); err != nil {
				t.Fatalf("E did not echo an ordinary probe: %v", err)
			}
			if r, w := connE.reads.Load(), connE.writes.Load(); r != 2 || w != 1 {
				t.Fatalf("E read %d datagrams and wrote %d replies, want 2 and 1: it answered X's echo endpoint", r, w)
			}
		})
	}
}

/*
An echo is an address AND a port: another host's probe may come from the same port number. Where
the platform gives no destination address, our own echo is told by the fleet's list, which holds
this agent too; the port alone no longer refuses, since a NAT hands out any unprivileged port.
*/
func TestProbeServerRefusesEchoEndpointsNotPorts(t *testing.T) {
	srv := NewProbeServer(9090)
	srv.SetPeerEchoAddrs([]netip.AddrPort{netip.MustParseAddrPort("10.0.0.2:9191"), {}})
	self := netip.MustParseAddr("10.0.0.1")
	for _, tc := range []struct {
		src  string
		dst  netip.Addr
		want bool
	}{
		{src: "10.0.0.2:45000", dst: self, want: true},  // a peer probe: DialUDP with no local address
		{src: "10.0.0.1:9090", dst: self, want: false},  // our own echo port, forged or looped back
		{src: "127.0.0.1:9090", dst: self, want: false}, // our own echo over loopback
		{src: "127.0.0.1:9090", want: false},            // the same, where the platform gives no destination
		{src: "10.0.0.2:9090", dst: self, want: true},   // another host's port that is our echo port
		{src: "10.0.0.2:9090", want: true},              // the same, where the platform gives no destination
		{src: "10.0.0.2:9191", dst: self, want: false},  // a peer's echo
		{src: "10.0.0.3:9191", dst: self, want: true},   // another host's port that is a peer's echo port
		{src: "10.0.0.2:7", dst: self, want: false},     // a well-known service such as echo or chargen
		{src: "10.0.0.2:1023", dst: self, want: false},  // privileged range
		{src: "10.0.0.2:1024", dst: self, want: true},   // first unprivileged port
		{src: "10.0.0.2:0", dst: self, want: false},     // no real sender uses port 0
	} {
		if got := srv.answers(netip.MustParseAddrPort(tc.src), tc.dst, 9090); got != tc.want {
			t.Errorf("answers(%s to %v) = %v, want %v", tc.src, tc.dst, got, tc.want)
		}
	}
}

/*
The fleet's peers feed the refusal as (address, echo port). A probe from another host whose kernel
handed out a peer's echo port as its ephemeral port is a probe: an agent with an echo port inside
the ephemeral range (32768-60999 on Linux) cost its fleet one random 100%-loss sample per ~28k
probe sockets.
*/
func TestProbeServerAnswersAProbeFromAPortAPeerEchoesOn(t *testing.T) {
	a, _ := newDepartureAgent(t)
	srv := NewProbeServer(0)
	listener, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.listener = listener
	srv.running.Store(true)
	go srv.serve(srv.listener)
	t.Cleanup(func() { _ = srv.Close() })
	a.probeServer = srv

	conn, err := (&net.Dialer{}).DialContext(context.Background(), "udp", listener.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	probePort := conn.LocalAddr().(*net.UDPAddr).Port
	a.applyPeers([]checker.Target{{AgentID: "id-b", NodeName: "node-b", PodIP: "10.0.0.9", UDPPort: probePort}})

	if _, err = conn.Write([]byte{0, 0, 0, 7}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err = conn.Read(make([]byte, 16)); err != nil {
		t.Fatalf("a probe from 127.0.0.1:%d got no echo because peer 10.0.0.9 echoes on port %d: %v",
			probePort, probePort, err)
	}
}

/*
The peer-port guard only knows this agent's own peers. Two agents that are not in each other's peer
lists (a sparse plan, a neighbouring fleet on a routable network) still answer each other's echo
ports, so one forged datagram would bounce between them at loopback speed; the per-source limit is
what ends it.
*/
func TestProbeServerRateLimitEndsAnEchoLoopBetweenStrangers(t *testing.T) {
	lc := net.ListenConfig{}
	pa, err := lc.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pb, err := lc.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	connA, connB := &countingConn{PacketConn: pa}, &countingConn{PacketConn: pb}
	for _, c := range []*countingConn{connA, connB} {
		srv := NewProbeServer(c.LocalAddr().(*net.UDPAddr).Port)
		srv.listener = c
		srv.running.Store(true)
		go srv.serve(srv.listener)
		t.Cleanup(func() { _ = srv.Close() })
	}

	seq := make([]byte, 4)
	binary.BigEndian.PutUint32(seq, 7)
	if _, err = connB.WriteTo(seq, pa.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)

	// A burst plus a second of refill: the loop may win back a token or two while it runs.
	limit := int64(echoBurst + echoRate)
	if a, b := connA.reads.Load(), connB.reads.Load(); a > limit || b > limit {
		t.Fatalf("one forged datagram between two echo servers that do not know each other made A read %d "+
			"and B read %d, want at most %d each", a, b, limit)
	}
}

// The limit must never cost a real probe a reply: the largest udp probe (100 packets on one socket)
// several times over from the same source, and a pmtu-sized burst on top.
func TestProbeServerRateLimitNeverDropsProbes(t *testing.T) {
	srv := NewProbeServer(0)
	lc := net.ListenConfig{}
	listener, err := lc.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.listener = listener
	srv.running.Store(true)
	go srv.serve(srv.listener)
	t.Cleanup(func() { _ = srv.Close() })

	port := listener.LocalAddr().(*net.UDPAddr).Port
	udp := checker.NewUDPChecker(time.Second, 100, port)
	for round := range 5 {
		res := udp.Check(context.Background(), checker.Target{PodIP: "127.0.0.1", UDPPort: port})
		d, _ := res.Details.(*model.UDPDetails)
		if !res.Success || d == nil || d.PacketsRecv != 100 {
			t.Fatalf("round %d: a 100-packet udp probe lost replies to the echo limit: %+v %s", round, d, res.Error)
		}
	}

	// A source port the kernel hands out again right away gets a second probe's worth at once.
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(context.Background(), "udp", listener.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	buf := make([]byte, 16)
	for i := range uint32(2*100 + 22) {
		payload := make([]byte, 4)
		binary.BigEndian.PutUint32(payload, i)
		if _, err := conn.Write(payload); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := conn.Read(buf); err != nil {
			t.Fatalf("datagram %d from one source got no echo: %v", i, err)
		}
	}
}

func TestEchoLimiterRefillsAndStaysBounded(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := newEchoLimiter(func() time.Time { return now })
	src := netip.MustParseAddrPort("10.0.0.2:45000")

	for i := range echoBurst {
		if !l.allow(src) {
			t.Fatalf("datagram %d of a burst of %d was refused", i, echoBurst)
		}
	}
	if l.allow(src) {
		t.Fatal("a source past its burst was still answered")
	}
	if !l.allow(netip.MustParseAddrPort("10.0.0.2:45001")) {
		t.Fatal("another source port of the same host shares the drained bucket")
	}
	now = now.Add(time.Second)
	for i := range echoRate {
		if !l.allow(src) {
			t.Fatalf("datagram %d after a second of refill was refused, want %d/s", i, echoRate)
		}
	}
	if l.allow(src) {
		t.Fatal("refill exceeded the rate")
	}

	// A flood of distinct sources never grows the map past its bound.
	for i := range 3 * echoMaxSources {
		ap := netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, 1, byte(i >> 8), byte(i)}), uint16(1024+i%60000))
		l.allow(ap)
		if n := len(l.buckets); n > echoMaxSources {
			t.Fatalf("limiter tracks %d sources, bound is %d", n, echoMaxSources)
		}
	}
}

/*
The probes use connected sockets, so a reply from any address other than the one probed is dropped
by the prober's kernel. An agent reached at an address that is not its route's preferred source
(advertiseAddress on a multi-homed host) read as 100% UDP loss. Linux answers the whole of 127/8 on
lo, which gives a second local address without root.
*/
func TestProbeServerRepliesFromTheProbedAddress(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs 127.0.0.2 as a local address, which only Linux has without configuration")
	}
	srv := NewProbeServer(0)
	if err := srv.ListenUDP(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	port := srv.listener.LocalAddr().(*net.UDPAddr).Port

	udp := checker.NewUDPChecker(300*time.Millisecond, 5, port)
	for _, ip := range []string{"127.0.0.1", "127.0.0.2"} {
		res := udp.Check(context.Background(), checker.Target{PodIP: ip, UDPPort: port})
		if !res.Success {
			t.Errorf("udp probe of the echo at %s: %s", ip, res.Error)
		}
	}
}

// ListenUDP opens a socket per family; both answer, each from the address it was probed at.
func TestProbeServerListensOnBothFamilies(t *testing.T) {
	srv := NewProbeServer(0)
	if err := srv.ListenUDP(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	port := srv.listener.LocalAddr().(*net.UDPAddr).Port
	targets := []string{"127.0.0.1"}
	if srv.listener6 != nil {
		targets = append(targets, "::1")
	}

	udp := checker.NewUDPChecker(300*time.Millisecond, 5, port)
	for _, ip := range targets {
		res := udp.Check(context.Background(), checker.Target{PodIP: ip, UDPPort: port})
		if !res.Success {
			t.Errorf("udp probe of the echo at %s: %s", ip, res.Error)
		}
	}
}

/*
A pod's probe to an agent off the cluster leaves through the node's MASQUERADE, which with
--random-fully draws every new flow's source port from 1024-65535; a prober whose kernel range is
widened to 1024-65535 hands out the same. One such probe in ~64k arrives from our own echo port, and
refusing that port from anywhere read as a 100% loss sample.
*/
func TestProbeServerAnswersASNATedProbeFromItsEchoPort(t *testing.T) {
	srv := NewProbeServer(9090)
	self := netip.MustParseAddr("203.0.113.5")
	srv.SetFleetEchoAddrs([]netip.AddrPort{
		netip.AddrPortFrom(self, 9090),
		netip.MustParseAddrPort("10.244.1.5:9090"),
		netip.MustParseAddrPort("10.0.0.7:9091"),
	})
	for _, src := range []string{
		"192.168.10.21:9090", // the node address the pod's probe was masqueraded to
		"10.0.0.7:9090",      // an agent that echoes on 9091, probing from a widened range
	} {
		if !srv.answers(netip.MustParseAddrPort(src), self, 9090) {
			t.Errorf("an echo on 9090 refused a probe from %s, which is no agent's echo", src)
		}
	}
}

/*
Under a sparse plan most of the fleet is in no agent's peer list. An in-cluster agent on 9090 and
an external one on 9091, outside each other's plans, answered each other's echo, so one forged
datagram bounced between them until the limiter drained. The controller sends every agent's echo.
*/
func TestProbeServerRefusesEveryFleetEchoOutsideItsPlan(t *testing.T) {
	x, e := netip.MustParseAddrPort("10.0.0.7:9090"), netip.MustParseAddrPort("10.0.0.9:9091")
	fleet := []netip.AddrPort{x, e, netip.MustParseAddrPort("10.0.0.11:9090")}
	srvX, srvE := NewProbeServer(int(x.Port())), NewProbeServer(int(e.Port()))
	for _, srv := range []*ProbeServer{srvX, srvE} {
		srv.SetPeerEchoAddrs([]netip.AddrPort{netip.MustParseAddrPort("10.0.0.11:9090")})
		srv.SetFleetEchoAddrs(fleet)
	}
	if srvE.answers(x, e.Addr(), int(e.Port())) {
		t.Errorf("E answered X's echo %s", x)
	}
	if srvX.answers(e, x.Addr(), int(x.Port())) {
		t.Errorf("X answered E's echo %s", e)
	}
	if !srvE.answers(netip.AddrPortFrom(x.Addr(), 45000), e.Addr(), int(e.Port())) {
		t.Error("E refused X's probe from an ordinary port")
	}

	// A controller older than 2.5.0 sends no fleet: the planned peers are what is left.
	srvE.SetFleetEchoAddrs(nil)
	if !srvE.answers(x, e.Addr(), int(e.Port())) {
		t.Error("with no fleet list E refused X, which is not its peer")
	}
	if srvE.answers(netip.MustParseAddrPort("10.0.0.11:9090"), e.Addr(), int(e.Port())) {
		t.Error("with no fleet list E answered its planned peer's echo")
	}
}

/*
No probe is sent to a broadcast or group address, while one datagram to one reaches every agent on
the segment: the reply from a broadcast source fails, and every datagram logged an ERROR line.
*/
func TestProbeServerIgnoresBroadcastAndGroupDestinations(t *testing.T) {
	srv := NewProbeServer(9090)
	srv.broadcasts.addrs = func() ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.IPv4(192, 168, 215, 6), Mask: net.CIDRMask(24, 32)},
			&net.IPNet{IP: net.IPv4(10, 1, 1, 1), Mask: net.CIDRMask(32, 32)},
			&net.IPNet{IP: net.IPv4(10, 2, 2, 0), Mask: net.CIDRMask(31, 32)},
			&net.IPNet{IP: net.ParseIP("fd00::6"), Mask: net.CIDRMask(64, 128)},
		}, nil
	}
	src := netip.MustParseAddrPort("192.168.215.7:45000")
	for _, tc := range []struct {
		dst  string
		want bool
	}{
		{dst: "192.168.215.6", want: true},
		{dst: "10.1.1.1", want: true},
		{dst: "10.2.2.1", want: true}, // a /31 has no broadcast address
		{dst: "fd00::6", want: true},
		{dst: "192.168.215.255", want: false},
		{dst: "255.255.255.255", want: false},
		{dst: "224.0.0.1", want: false},
		{dst: "ff02::1", want: false},
	} {
		if got := srv.answers(src, netip.MustParseAddr(tc.dst), 9090); got != tc.want {
			t.Errorf("answers(%s to %s) = %v, want %v", src, tc.dst, got, tc.want)
		}
	}
}

// failingWriteConn stands in for a socket whose every reply fails.
type failingWriteConn struct{ net.PacketConn }

func (failingWriteConn) WriteTo([]byte, net.Addr) (int, error) {
	return 0, errors.New("sendmsg: network is unreachable")
}

// A failing reply is logged once per interval with a count, not once per datagram.
func TestProbeServerRateLimitsTheWriteErrorLog(t *testing.T) {
	logs := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	listener, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	counting := &countingConn{PacketConn: failingWriteConn{listener}}
	srv := NewProbeServer(0)
	srv.listener = counting
	srv.running.Store(true)
	go srv.serve(srv.listener)
	t.Cleanup(func() { _ = srv.Close() })

	const datagrams = 10
	for i := range datagrams {
		conn, err := (&net.Dialer{}).DialContext(context.Background(), "udp", listener.LocalAddr().String())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = conn.Write([]byte{0, 0, 0, byte(i)}); err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	}
	waitFor(t, 2*time.Second, "the echo to read every datagram", func() bool { return counting.reads.Load() == datagrams })
	time.Sleep(50 * time.Millisecond)

	if n := strings.Count(logs.String(), `msg="UDP write error"`); n != 1 {
		t.Errorf("%d 'UDP write error' lines for %d failed replies, want 1", n, datagrams)
	}
}

// failingReadConn fails its first failures reads, then reads from the socket it wraps.
type failingReadConn struct {
	net.PacketConn
	failures int64
	reads    atomic.Int64
}

func (c *failingReadConn) ReadFrom(p []byte) (int, net.Addr, error) {
	if c.reads.Add(1) <= c.failures {
		return 0, nil, errors.New("recvmsg: no buffer space available")
	}
	return c.PacketConn.ReadFrom(p)
}

// A socket that keeps failing reads logs once per interval with a count, like a failing reply.
func TestProbeServerRateLimitsTheReadErrorLog(t *testing.T) {
	logs := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	listener, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	const failures = 10
	failing := &failingReadConn{PacketConn: listener, failures: failures}
	srv := NewProbeServer(0)
	srv.listener = failing
	srv.running.Store(true)
	go srv.serve(srv.listener)
	t.Cleanup(func() { _ = srv.Close() })

	waitFor(t, 2*time.Second, "the echo to get past every failing read", func() bool { return failing.reads.Load() > failures })

	if n := strings.Count(logs.String(), `msg="UDP read error"`); n != 1 {
		t.Errorf("%d 'UDP read error' lines for %d failed reads, want 1", n, failures)
	}
}

/*
A reply from the probed address can fail where the kernel's own source would go out (the address
left the host between the read and the write, or the kernel refuses it as a source). The reply then
falls back to the kernel's source instead of being dropped.
*/
func TestEchoSocketFallsBackToTheKernelSource(t *testing.T) {
	lc := net.ListenConfig{}
	echo, err := lc.ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = echo.Close() }()
	prober, err := lc.ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = prober.Close() }()

	sock := newEchoSocket(echo)
	if _, ok := sock.(ipv4EchoSocket); !ok {
		t.Skipf("no destination control messages on %s", runtime.GOOS)
	}
	if err = sock.writeTo([]byte{0, 0, 0, 7}, prober.LocalAddr(), netip.MustParseAddr("192.0.2.1")); err != nil {
		t.Fatalf("a reply whose source 192.0.2.1 is not local failed instead of taking the kernel's: %v", err)
	}
	_ = prober.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err = prober.ReadFrom(make([]byte, 16)); err != nil {
		t.Fatalf("no reply arrived: %v", err)
	}
}

// A stale set keeps answering while one background read replaces it; both serve goroutines ask.
func TestLocalBroadcastsRefreshesAStaleSetInTheBackground(t *testing.T) {
	var reads atomic.Int64
	prefix := atomic.Pointer[net.IPNet]{}
	prefix.Store(&net.IPNet{IP: net.IPv4(192, 168, 1, 6), Mask: net.CIDRMask(24, 32)})
	b := &localBroadcasts{addrs: func() ([]net.Addr, error) {
		reads.Add(1)
		return []net.Addr{prefix.Load()}, nil
	}}
	old, moved := netip.MustParseAddr("192.168.1.255"), netip.MustParseAddr("192.168.2.255")
	if !b.contains(old) || b.contains(moved) {
		t.Fatal("the first read did not give 192.168.1.255 alone")
	}

	prefix.Store(&net.IPNet{IP: net.IPv4(192, 168, 2, 6), Mask: net.CIDRMask(24, 32)})
	b.set.Store(&broadcastSet{at: time.Now().Add(-broadcastRefresh), addrs: b.set.Load().addrs})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				b.contains(old)
			}
		})
	}
	wg.Wait()
	waitFor(t, 2*time.Second, "the background refresh", func() bool { return b.contains(moved) })
	if b.contains(old) {
		t.Error("the refreshed set still holds the old broadcast address")
	}
	if n := reads.Load(); n != 2 {
		t.Errorf("the interfaces were read %d times, want 2: the first read and one refresh of the stale set", n)
	}
}
