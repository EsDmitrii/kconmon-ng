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

var errPMTUUnsupported = fmt.Errorf("pmtu probe is not supported on %s", runtime.GOOS)

// interfaceMTUFor is a seam: tests pin the MTU the probe believes its interface has.
var interfaceMTUFor = interfaceMTU

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
	probeMTU, err := c.probeSize(local.IP, target.NodeName)
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
		result.Error = "pmtu: the peer's echo did not answer a 64-byte datagram; reachability is the udp plane's verdict"
	}
	return result
}

// probeSize resolves the IP-level size this probe tests: the configured size, else the MTU of the
// interface that owns the socket's local address, clamped to what one IPv4 datagram can be.
func (c *PMTUChecker) probeSize(local net.IP, peer string) (int, error) {
	device, err := interfaceMTUFor(local)
	if err != nil && c.size == 0 {
		return 0, fmt.Errorf("pmtu: cannot read the MTU of the interface owning %s: %w", local, err)
	}
	size := c.size
	switch {
	case size == 0:
		size = device
	case err == nil && size > device:
		// Above the device MTU every send fails locally and would read as a broken network; the
		// operator asked a question this host cannot put on the wire.
		if _, warned := c.clampWarned.LoadOrStore(peer, struct{}{}); !warned {
			slog.Warn("checkers.pmtu.size is above the interface MTU; probing at the interface MTU",
				"configured", size, "interfaceMtu", device, "peer", peer)
		}
		size = device
	}
	return min(max(size, pmtuMinSize), pmtuMaxIPSize), nil
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
