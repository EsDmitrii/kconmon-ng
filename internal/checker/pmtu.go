package checker

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

const (
	// pmtuMinSize is the datagram every IPv4 host must accept (RFC 791): a smaller probe says nothing
	// about MTU.
	pmtuMinSize = 576
	// pmtuMaxIPSize is the IPv4 total-length ceiling. Linux loopback advertises 65536, one more than
	// any IPv4 datagram can be.
	pmtuMaxIPSize = 65535
	udpHeaderLen  = 8
)

var (
	errPMTUUnsupported     = fmt.Errorf("pmtu probe is not supported on %s", runtime.GOOS)
	errRouteMTUUnsupported = fmt.Errorf("route MTU lookup is not supported on %s", runtime.GOOS)
)

// routeMTUFor and interfaceMTUFor are seams: tests pin the MTU the probe believes the route to the
// peer and the interface owning its address have.
var (
	routeMTUFor     = routeMTU
	interfaceMTUFor = interfaceMTU
)

// PMTUChecker sends DF-marked UDP datagrams to a peer's echo port and bisects the size on loss. The
// echo contract is the udp checker's: the first four bytes carry a sequence number and the peer
// answers with exactly those four bytes, so a 2.4.x peer needs nothing new. The reply is small on
// purpose: this probe measures the forward path, and the peer's own probe measures the way back.
type PMTUChecker struct {
	timeout  time.Duration
	interval time.Duration
	size     int
	port     int
	// clampWarned holds the peers already warned about a size above the device MTU: one warning
	// per peer for the life of the agent, not one per interval.
	clampWarned sync.Map
	// routeWarned is set once the route lookup has failed and been warned about.
	routeWarned atomic.Bool
}

func NewPMTUChecker(timeout, interval time.Duration, size, port int) *PMTUChecker {
	return &PMTUChecker{timeout: timeout, interval: interval, size: size, port: port}
}

func (c *PMTUChecker) Name() model.CheckType {
	return model.CheckPMTU
}

func (c *PMTUChecker) Check(ctx context.Context, target Target) model.CheckResult { //nolint:gocritic // hugeParam: Target is a VALUE by design -- a checker must not be able to mutate the caller's copy, and one 80-byte copy per probe is nothing next to the probe itself
	start := time.Now()
	result := model.CheckResult{Type: model.CheckPMTU, Timestamp: start}

	if target.External {
		result.Error = "pmtu is probed between kconmon agents only: an external host runs no echo responder"
		return result
	}
	if !pmtuSupported {
		result.Error = errPMTUUnsupported.Error()
		return result
	}

	// The peer's reported echo port, else our own: the udp checker's rule, for the same reason.
	port := target.UDPPort
	if port == 0 {
		port = c.port
	}
	raddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(target.PodIP, strconv.Itoa(port)))
	if err != nil {
		result.Error = fmt.Sprintf("resolve UDP addr: %v", err)
		return result
	}
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		result.Error = fmt.Sprintf("UDP dial: %v", err)
		return result
	}
	defer func() { _ = conn.Close() }()

	v6 := raddr.IP.To4() == nil
	if err = setDontFragment(conn, v6); err != nil {
		result.Error = fmt.Sprintf("setting DF on the probe socket: %v", err)
		return result
	}
	local, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		result.Error = "probe socket has no UDP local address"
		return result
	}
	probeMTU, err := c.probeSize(raddr.IP, local.IP, target.NodeName)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	// Half the interval: a black-holed fleet must not push the next round late.
	deadline := start.Add(c.interval / 2)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	search := pmtuSearch{
		ctx: ctx, path: newUDPPMTUPath(conn, v6, probeMTU), probeMTU: probeMTU,
		timeout: c.timeout, deadline: deadline, now: time.Now,
	}
	d := search.run()

	result.Details = &d
	result.Duration = time.Since(start)
	result.Success = d.Verdict == model.PMTUVerdictOK || d.Verdict == model.PMTUVerdictReduced
	switch d.Verdict {
	case model.PMTUVerdictBlackhole:
		result.Error = fmt.Sprintf("path MTU black hole: %d-byte datagrams are lost with no ICMP frag-needed, %d bytes cross",
			probeMTU, d.PathMTU)
		if d.Truncated {
			result.Error += " (the search stopped at its time budget: the path carries at least that much)"
		}
	case model.PMTUVerdictUnreachable:
		result.Error = "pmtu: " + search.reason
	}
	return result
}

// ForgetPeer drops what the checker keeps about a peer that left the mesh, so node churn does not
// grow it for the life of the agent. A peer that comes back is warned about once more.
func (c *PMTUChecker) ForgetPeer(nodeName string) {
	c.clampWarned.Delete(nodeName)
}

// probeSize resolves the IP-level size this probe tests: the configured size, else the MTU of the
// route to the peer (the interface owning the socket's address where the route cannot be read),
// clamped to what one IPv4 datagram can be.
func (c *PMTUChecker) probeSize(remote, local net.IP, peer string) (int, error) {
	device, err := c.localMTU(remote, local)
	if err != nil && c.size == 0 {
		return 0, fmt.Errorf("pmtu: cannot read the MTU of the interface owning %s: %w", local, err)
	}
	size := c.size
	switch {
	case size == 0:
		size = device
	case err == nil && size > device:
		// Above the route MTU the probe asks about datagrams this host never sends to the peer, and
		// above the device MTU every send fails locally and would read as a broken network.
		if _, warned := c.clampWarned.LoadOrStore(peer, struct{}{}); !warned {
			slog.Warn("checkers.pmtu.size is above the MTU of the route to the peer; probing at the route MTU",
				"configured", size, "routeMtu", device, "peer", peer)
		}
		size = device
	}
	return min(max(size, pmtuMinSize), pmtuMaxIPSize), nil
}

// localMTU is the largest datagram this host sends to remote: the route's MTU on Linux, else the MTU
// of the interface that owns local. PROBE mode sizes against the device and ignores a route MTU, so
// a CNI that keeps the pod's link at 1500 and sets the tunnel MTU on its routes would be probed at a
// size its pods never send.
func (c *PMTUChecker) localMTU(remote, local net.IP) (int, error) {
	mtu, err := routeMTUFor(remote, local)
	if err == nil {
		return mtu, nil
	}
	if !errors.Is(err, errRouteMTUUnsupported) {
		if c.routeWarned.CompareAndSwap(false, true) {
			slog.Warn("pmtu: cannot read the route to the peer; sizing the probe from the interface MTU",
				"peer", remote, "error", err)
		} else {
			slog.Debug("pmtu: cannot read the route to the peer; sizing the probe from the interface MTU",
				"peer", remote, "error", err)
		}
	}
	return interfaceMTUFor(local)
}

func interfaceMTU(local net.IP) (int, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return 0, err
	}
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip != nil && ip.Equal(local) {
				return ifc.MTU, nil
			}
		}
	}
	return 0, fmt.Errorf("no interface owns %s", local)
}

func ipHeaderLen(v6 bool) int {
	if v6 {
		return 40
	}
	return 20
}

// udpPMTUPath is the real pmtuPath: one connected, DF-marked UDP socket per probe.
type udpPMTUPath struct {
	conn    *net.UDPConn
	v6      bool
	seq     uint32
	payload []byte
	buf     []byte
}

func newUDPPMTUPath(conn *net.UDPConn, v6 bool, probeMTU int) *udpPMTUPath {
	return &udpPMTUPath{conn: conn, v6: v6, payload: make([]byte, probeMTU), buf: make([]byte, 64)}
}

func (p *udpPMTUPath) Send(size int, timeout time.Duration) error {
	n := max(size-ipHeaderLen(p.v6)-udpHeaderLen, 4)
	p.seq++
	binary.BigEndian.PutUint32(p.payload[:4], p.seq)
	if err := p.conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	if _, err := p.conn.Write(p.payload[:n]); err != nil {
		return p.classify(err)
	}
	for {
		m, err := p.conn.Read(p.buf)
		if err != nil {
			return p.classify(err)
		}
		// An earlier datagram's late echo is drained, not counted: only this seq ends the wait.
		if m >= 4 && binary.BigEndian.Uint32(p.buf[:4]) == p.seq {
			return nil
		}
	}
}

// classify turns a socket error into the search's vocabulary. On a connected UDP socket the kernel
// relays an ICMP frag-needed (or the local route's refusal) as EMSGSIZE on the next call; a timeout,
// a refused port and any other transient error are a lost datagram.
func (p *udpPMTUPath) classify(err error) error {
	if errors.Is(err, syscall.EMSGSIZE) {
		return &pmtuTooBigError{mtu: reportedPathMTU(p.conn, p.v6)}
	}
	return errPMTULost
}
