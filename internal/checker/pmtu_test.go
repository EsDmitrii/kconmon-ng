package checker

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

func skipWithoutPMTU(t *testing.T) {
	t.Helper()
	if !pmtuSupported {
		t.Skip("pmtu probe is not supported on this OS")
	}
}

func pinInterfaceMTU(t *testing.T, mtu int, err error) {
	t.Helper()
	prev := interfaceMTUFor
	interfaceMTUFor = func(net.IP) (int, error) { return mtu, err }
	t.Cleanup(func() { interfaceMTUFor = prev })
}

func pinRouteMTU(t *testing.T, mtu int, err error) {
	t.Helper()
	prev := routeMTUFor
	routeMTUFor = func(net.IP, net.IP) (int, error) { return mtu, err }
	t.Cleanup(func() { routeMTUFor = prev })
}

func pmtuDetails(t *testing.T, res model.CheckResult) *model.PMTUDetails {
	t.Helper()
	d, ok := res.Details.(*model.PMTUDetails)
	if !ok {
		t.Fatalf("Details = %T (%+v), want *model.PMTUDetails; error %q", res.Details, res.Details, res.Error)
	}
	return d
}

// startUDPEchoServer reads into a 1024-byte buffer and answers with the first four bytes: exactly
// what a 2.4.x agent's echo server does. A pmtu probe from a 2.5.0 agent must still read its peer
// as healthy, at 1500 and at a jumbo size.
func TestPMTUCheckerOldPeerEchoesLargeDatagram(t *testing.T) {
	skipWithoutPMTU(t)
	port, cleanup := startUDPEchoServer(t)
	defer cleanup()

	for _, size := range []int{1500, 9000} {
		c := NewPMTUChecker(200*time.Millisecond, time.Minute, size, port)
		res := c.Check(context.Background(), Target{NodeName: "peer", PodIP: "127.0.0.1"})
		d := pmtuDetails(t, res)
		if !res.Success || d.Verdict != model.PMTUVerdictOK || d.PathMTU != size || d.ProbeMTU != size {
			t.Fatalf("size %d: success=%v details=%+v error=%q, want ok at %d", size, res.Success, d, res.Error, size)
		}
		if res.Type != model.CheckPMTU {
			t.Errorf("Type = %q, want %q", res.Type, model.CheckPMTU)
		}
	}
}

// Cilium keeps the pod's eth0 at 1500 and puts the tunnel MTU on the pod's routes: the probe asks
// about the route to the peer, the size the pod's traffic uses, not about the interface.
func TestPMTUCheckerProbeSizeFollowsTheRoute(t *testing.T) {
	skipWithoutPMTU(t)
	pinRouteMTU(t, 1450, nil)
	pinInterfaceMTU(t, 1500, nil)
	port, cleanup := startUDPEchoServer(t)
	defer cleanup()

	c := NewPMTUChecker(200*time.Millisecond, time.Minute, 0, port)
	res := c.Check(context.Background(), Target{NodeName: "peer", PodIP: "127.0.0.1"})
	if d := pmtuDetails(t, res); d.ProbeMTU != 1450 || d.Verdict != model.PMTUVerdictOK {
		t.Fatalf("details = %+v, want an ok probe at the route's 1450", d)
	}
	if c.routeWarned.Load() {
		t.Error("a readable route logged the fallback warning")
	}
}

// Without the route (netlink refused, or an OS with no FIB query) the probe sizes itself as before,
// from the interface that owns its address. An unexpected failure is warned about once per agent.
func TestPMTUCheckerFallsBackToTheInterfaceMTU(t *testing.T) {
	skipWithoutPMTU(t)
	pinInterfaceMTU(t, 1450, nil)
	port, cleanup := startUDPEchoServer(t)
	defer cleanup()

	for _, tc := range []struct {
		routeErr error
		warned   bool
	}{{errRouteMTUUnsupported, false}, {errors.New("netlink: operation not permitted"), true}} {
		pinRouteMTU(t, 0, tc.routeErr)
		c := NewPMTUChecker(200*time.Millisecond, time.Minute, 0, port)
		for range 2 {
			res := c.Check(context.Background(), Target{NodeName: "peer", PodIP: "127.0.0.1"})
			if d := pmtuDetails(t, res); d.ProbeMTU != 1450 || d.Verdict != model.PMTUVerdictOK {
				t.Fatalf("route error %v: details = %+v, want an ok probe at the interface's 1450", tc.routeErr, d)
			}
		}
		if c.routeWarned.Load() != tc.warned {
			t.Errorf("route error %v: warned = %v, want %v", tc.routeErr, c.routeWarned.Load(), tc.warned)
		}
	}
}

// size: 9000 on a 1500 network must not read as a fleet-wide black hole: clamp, warn once per peer.
func TestPMTUCheckerClampsConfiguredSizeToTheDevice(t *testing.T) {
	skipWithoutPMTU(t)
	pinRouteMTU(t, 1500, nil)
	port, cleanup := startUDPEchoServer(t)
	defer cleanup()

	c := NewPMTUChecker(200*time.Millisecond, time.Minute, 9000, port)
	for range 2 {
		res := c.Check(context.Background(), Target{NodeName: "peer", PodIP: "127.0.0.1"})
		if d := pmtuDetails(t, res); d.ProbeMTU != 1500 || d.Verdict != model.PMTUVerdictOK {
			t.Fatalf("details = %+v, want an ok probe clamped to 1500", d)
		}
	}
	warned := 0
	c.clampWarned.Range(func(any, any) bool { warned++; return true })
	if warned != 1 {
		t.Errorf("clamp warnings recorded for %d peers, want exactly 1", warned)
	}

	// A departed peer leaves nothing behind: node churn must not grow the record for the agent's life.
	c.ForgetPeer("peer")
	warned = 0
	c.clampWarned.Range(func(any, any) bool { warned++; return true })
	if warned != 0 {
		t.Errorf("clamp warnings recorded for %d peers after ForgetPeer, want 0", warned)
	}
}

func TestPMTUCheckerUnknownInterfaceMTU(t *testing.T) {
	skipWithoutPMTU(t)
	pinRouteMTU(t, 0, errors.New("netlink: operation not permitted"))
	pinInterfaceMTU(t, 0, errors.New("no interface owns the address"))
	port, cleanup := startUDPEchoServer(t)
	defer cleanup()

	res := NewPMTUChecker(200*time.Millisecond, time.Minute, 0, port).
		Check(context.Background(), Target{NodeName: "peer", PodIP: "127.0.0.1"})
	if res.Success || res.Details != nil || !strings.Contains(res.Error, "cannot read the MTU") {
		t.Fatalf("result = %+v, want a failure naming the unreadable MTU and no details", res)
	}

	// An explicit size needs no interface lookup.
	res = NewPMTUChecker(200*time.Millisecond, time.Minute, 1400, port).
		Check(context.Background(), Target{NodeName: "peer", PodIP: "127.0.0.1"})
	if d := pmtuDetails(t, res); d.ProbeMTU != 1400 || d.Verdict != model.PMTUVerdictOK {
		t.Fatalf("details = %+v, want an ok probe at the configured 1400", d)
	}
}

// startUDP6EchoServer is the echo contract on ::1, recording the payload length of every datagram.
func startUDP6EchoServer(t *testing.T) (port int, payloads func() []int) {
	t.Helper()
	lc := net.ListenConfig{}
	conn, err := lc.ListenPacket(context.Background(), "udp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback here: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	var mu sync.Mutex
	var seen []int
	go func() {
		buf := make([]byte, 65536)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			mu.Lock()
			seen = append(seen, n)
			mu.Unlock()
			if n >= 4 {
				_, _ = conn.WriteTo(buf[:4], from)
			}
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr).Port, func() []int {
		mu.Lock()
		defer mu.Unlock()
		return append([]int(nil), seen...)
	}
}

// Over IPv6 the header is 40 bytes, not 20: a 1500-byte probe carries 1452 bytes of payload. With the
// IPv4 arithmetic every v6 probe would be 20 bytes larger than the size it reports.
func TestPMTUCheckerIPv6PayloadMath(t *testing.T) {
	skipWithoutPMTU(t)
	port, payloads := startUDP6EchoServer(t)

	res := NewPMTUChecker(200*time.Millisecond, time.Minute, 1500, port).
		Check(context.Background(), Target{NodeName: "peer", PodIP: "::1"})
	d := pmtuDetails(t, res)
	if !res.Success || d.Verdict != model.PMTUVerdictOK || d.PathMTU != 1500 {
		t.Fatalf("success=%v details=%+v error=%q, want ok at 1500 over ::1", res.Success, d, res.Error)
	}
	got := payloads()
	if want := []int{pmtuBaseSize - 48, 1500 - 48}; !slices.Equal(got, want) {
		t.Fatalf("payload lengths = %v, want %v (IPv6 40 + UDP 8 bytes of headers)", got, want)
	}
}

// A peer that is gone is the udp plane's verdict, not a black hole.
func TestPMTUCheckerDeadPeerIsUnreachable(t *testing.T) {
	skipWithoutPMTU(t)
	res := NewPMTUChecker(100*time.Millisecond, time.Minute, 1500, deadUDPPort(t)).
		Check(context.Background(), Target{NodeName: "peer", PodIP: "127.0.0.1"})
	d := pmtuDetails(t, res)
	if res.Success || d.Verdict != model.PMTUVerdictUnreachable || !strings.Contains(res.Error, "udp plane") {
		t.Fatalf("success=%v details=%+v error=%q, want unreachable handed to the udp plane", res.Success, d, res.Error)
	}
}

// A run cancelled by its caller is not a peer that stopped answering: the error says what happened.
func TestPMTUCheckerCancelledRunSaysSo(t *testing.T) {
	skipWithoutPMTU(t)
	port, cleanup := startUDPEchoServer(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := NewPMTUChecker(100*time.Millisecond, time.Minute, 1500, port).
		Check(ctx, Target{NodeName: "peer", PodIP: "127.0.0.1"})
	d := pmtuDetails(t, res)
	if res.Success || d.Verdict != model.PMTUVerdictUnreachable || !strings.Contains(res.Error, "context canceled") ||
		strings.Contains(res.Error, "did not answer") {
		t.Fatalf("success=%v details=%+v error=%q, want unreachable naming the cancellation", res.Success, d, res.Error)
	}
}

func TestPMTUCheckerRefusesAnExternalTarget(t *testing.T) {
	res := NewPMTUChecker(100*time.Millisecond, time.Minute, 1500, 9).
		Check(context.Background(), Target{PodIP: "192.0.2.1", Port: 53, External: true})
	if res.Success || res.Details != nil || !strings.Contains(res.Error, "between kconmon agents only") {
		t.Fatalf("result = %+v, want a refusal for an external host", res)
	}
}

// A datagram larger than any UDP payload is refused by the local stack with EMSGSIZE: the path
// adapter must turn that into a refusal with a reason, not a loss.
func TestUDPPMTUPathClassifiesEMSGSIZE(t *testing.T) {
	skipWithoutPMTU(t)
	port, cleanup := startUDPEchoServer(t)
	defer cleanup()

	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := setDontFragment(conn, false); err != nil {
		t.Fatal(err)
	}
	const oversize = 70000
	p := newUDPPMTUPath(conn, false, oversize)
	var tooBig *pmtuTooBigError
	if err := p.Send(oversize, 100*time.Millisecond); !errors.As(err, &tooBig) {
		t.Fatalf("Send(%d) = %v, want *pmtuTooBigError", oversize, err)
	}
	if err := p.Send(1500, 200*time.Millisecond); err != nil {
		t.Fatalf("Send(1500) after the refusal = %v, want the echo", err)
	}
}
