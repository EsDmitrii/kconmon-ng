package checker

import (
	"context"
	"errors"
	"net"
	"strings"
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

// A pod behind a VXLAN CNI has eth0 at 1450 while the node NIC says 1500: the probe asks about the
// pod's own interface.
func TestPMTUCheckerProbeSizeFollowsInterfaceMTU(t *testing.T) {
	skipWithoutPMTU(t)
	pinInterfaceMTU(t, 1450, nil)
	port, cleanup := startUDPEchoServer(t)
	defer cleanup()

	res := NewPMTUChecker(200*time.Millisecond, time.Minute, 0, port).
		Check(context.Background(), Target{NodeName: "peer", PodIP: "127.0.0.1"})
	if d := pmtuDetails(t, res); d.ProbeMTU != 1450 || d.Verdict != model.PMTUVerdictOK {
		t.Fatalf("details = %+v, want an ok probe at 1450", d)
	}
}

// size: 9000 on a 1500 network must not read as a fleet-wide black hole: clamp, warn once per peer.
func TestPMTUCheckerClampsConfiguredSizeToTheDevice(t *testing.T) {
	skipWithoutPMTU(t)
	pinInterfaceMTU(t, 1500, nil)
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
}

func TestPMTUCheckerUnknownInterfaceMTU(t *testing.T) {
	skipWithoutPMTU(t)
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
