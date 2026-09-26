package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync/atomic"
	"syscall"
	"time"
)

type ProbeServer struct {
	udpPort int
	// listener is the IPv4 socket, listener6 the IPv6 one (nil on a host without IPv6): a socket per
	// family, because a dual-stack socket cannot set the source of an IPv4 reply.
	listener  net.PacketConn
	listener6 net.PacketConn
	running   atomic.Bool
	// fleetEcho holds every registered agent's echo endpoint as the controller sends it; peerEcho, the
	// planned peers' alone, is all an older controller leaves to go on.
	fleetEcho  atomic.Pointer[map[netip.AddrPort]struct{}]
	peerEcho   atomic.Pointer[map[netip.AddrPort]struct{}]
	broadcasts localBroadcasts
}

func NewProbeServer(udpPort int) *ProbeServer {
	return &ProbeServer{udpPort: udpPort}
}

func (s *ProbeServer) ListenUDP(ctx context.Context) error {
	lc := net.ListenConfig{}
	conn, err := lc.ListenPacket(ctx, "udp4", fmt.Sprintf(":%d", s.udpPort))
	if err != nil {
		return err
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port //nolint:forcetypeassert // a udp4 listener
	conn6, err := lc.ListenPacket(ctx, "udp6", fmt.Sprintf(":%d", port))
	switch {
	case errors.Is(err, syscall.EADDRINUSE):
		_ = conn.Close()
		return err
	case err != nil:
		slog.Info("UDP echo serves IPv4 only: no IPv6 socket", "error", err)
		conn6 = nil
	}
	s.listener, s.listener6 = conn, conn6
	s.running.Store(true)

	go s.serve(conn)
	if conn6 != nil {
		go s.serve(conn6)
	}
	return nil
}

// probeReadBuffer holds the largest UDP datagram IPv4 can carry, so a full-size pmtu probe is read
// whole. The reply stays four bytes: only the sequence number travels back, the forward path is what
// a probe measures.
const probeReadBuffer = 65535

// echoLogInterval spaces the echo's repeated warnings: a source over the limit, a read or a reply
// that fails.
const echoLogInterval = time.Minute

func (s *ProbeServer) serve(conn net.PacketConn) {
	buf := make([]byte, probeReadBuffer)
	ownPort := s.udpPort
	if la, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		ownPort = la.Port
	}
	sock := newEchoSocket(conn)
	limiter := newEchoLimiter(time.Now)
	var limitLog, readLog, writeLog logThrottle
	for s.running.Load() {
		n, addr, dst, err := sock.readFrom(buf)
		if err != nil {
			if s.running.Load() {
				if failed, ok := readLog.tick(time.Now()); ok {
					slog.Error("UDP read error", "error", err, "failed", failed)
				}
			}
			continue
		}

		ua, ok := addr.(*net.UDPAddr)
		if n < 4 || !ok {
			continue
		}
		src := netip.AddrPortFrom(ua.AddrPort().Addr().Unmap(), ua.AddrPort().Port())
		if !s.answers(src, dst, ownPort) {
			continue
		}
		if !limiter.allow(src) {
			if dropped, ok := limitLog.tick(time.Now()); ok {
				slog.Warn("UDP echo rate limit reached, replies dropped: an echo loop with another UDP "+
					"responder or a flood, not a probe", "source", addr, "dropped", dropped)
			}
			continue
		}

		seq := binary.BigEndian.Uint32(buf[:4])
		resp := make([]byte, 4)
		binary.BigEndian.PutUint32(resp, seq)

		if err := sock.writeTo(resp, addr, dst); err != nil {
			if failed, ok := writeLog.tick(time.Now()); ok {
				slog.Error("UDP write error", "error", err, "addr", addr, "from", dst, "failed", failed)
			}
		}
	}
}

// logThrottle lets the first of a run of events log, then one line per echoLogInterval that counts
// the events since the last line.
type logThrottle struct {
	at time.Time
	n  int
}

func (l *logThrottle) tick(now time.Time) (int, bool) {
	l.n++
	if now.Sub(l.at) < echoLogInterval {
		return 0, false
	}
	n := l.n
	l.at, l.n = now, 0
	return n, true
}

// SetPeerEchoAddrs records the echo address and port of every current peer.
func (s *ProbeServer) SetPeerEchoAddrs(addrs []netip.AddrPort) {
	set := echoSet(addrs)
	s.peerEcho.Store(&set)
}

// SetFleetEchoAddrs records the echo address and port of every registered agent; none (a controller
// older than 2.5.0) leaves the peers' to go on.
func (s *ProbeServer) SetFleetEchoAddrs(addrs []netip.AddrPort) {
	set := echoSet(addrs)
	s.fleetEcho.Store(&set)
}

func echoSet(addrs []netip.AddrPort) map[netip.AddrPort]struct{} {
	set := make(map[netip.AddrPort]struct{}, len(addrs))
	for _, a := range addrs {
		if a.IsValid() && a.Port() > 0 {
			set[netip.AddrPortFrom(a.Addr().Unmap(), a.Port())] = struct{}{}
		}
	}
	return set
}

/*
answers reports whether a datagram from src, sent to dst, gets an echo. No probe is sent to a
broadcast or group address, while one such datagram reaches every agent on the segment. A datagram
from another responder starts an echo loop, so these are refused: a privileged source port (a
well-known service such as echo or chargen), any registered agent's echo endpoint, and our own echo
port from the address the datagram reached us at or from loopback. A port alone proves nothing,
ours included: a NAT on the path (MASQUERADE --random-fully) draws a probe's source port from the
whole unprivileged range, and so may a prober's own kernel. The echo limiter ends a loop these miss,
one forged from an address that is not an agent's.
*/
func (s *ProbeServer) answers(src netip.AddrPort, dst netip.Addr, ownPort int) bool {
	if src.Port() < 1024 || dst.IsMulticast() || dst == limitedBroadcast || s.broadcasts.contains(dst) {
		return false
	}
	if int(src.Port()) == ownPort && (src.Addr() == dst || src.Addr().IsLoopback()) {
		return false
	}
	echoes := s.fleetEcho.Load()
	if echoes == nil || len(*echoes) == 0 {
		echoes = s.peerEcho.Load()
	}
	if echoes == nil {
		return true
	}
	_, isEcho := (*echoes)[src]
	return !isEcho
}

func (s *ProbeServer) Close() error {
	s.running.Store(false)
	var errs []error
	for _, l := range []net.PacketConn{s.listener, s.listener6} {
		if l != nil {
			errs = append(errs, l.Close())
		}
	}
	return errors.Join(errs...)
}
