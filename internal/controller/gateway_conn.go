package controller

import (
	"container/list"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

/*
gatewayLimits bound what a client that has not proven fleet membership can hold on the external
gateway. The token is checked per RPC, so without them a client that completed TLS and the HTTP/2
preface and then never sent an RPC was kept for as long as it answered pings, and a few thousand
such connections were enough to OOM the controller and take the fleet's control plane with it.

An agent authenticates with its first RPC, milliseconds after the handshake, so none of these limits
touch a working fleet member. A full pending set is never a reason to refuse: the newest connection
is admitted and an older pending one is closed, so idle sockets cannot lock out an agent that
reconnects, unless someone opens more than maxPending connections within its TLS handshake.
*/
type gatewayLimits struct {
	// handshakeTimeout bounds the TLS handshake and the HTTP/2 preface (grpc.ConnectionTimeout,
	// 120s by default).
	handshakeTimeout time.Duration
	// authDeadline is how long a connection may stay open before one of its RPCs passes the token
	// check; a failing RPC does not extend it.
	authDeadline time.Duration
	// maxPending caps the connections that have not authenticated yet; past it, a new connection
	// closes one of the source holding the most (see victimLocked). Authenticated connections do not
	// count. There is deliberately no smaller cap per source: behind externalTrafficPolicy Cluster
	// every client arrives from a node IP, and such a cap let far fewer bare sockets push out every
	// agent handshake arriving through one node.
	maxPending int
	// maxIdle is keepalive MaxConnectionIdle: agents always hold their Watch streams, so a
	// connection with no RPC in flight for this long has been abandoned.
	maxIdle time.Duration
}

var defaultGatewayLimits = gatewayLimits{
	handshakeTimeout: 10 * time.Second,
	authDeadline:     30 * time.Second,
	maxPending:       128,
	maxIdle:          2 * time.Minute,
}

// gatewayEvictionLogEvery rate-limits the "cap reached" warning: under a flood it would otherwise
// be one line per connection.
const gatewayEvictionLogEvery = time.Minute

// gatewayConnGuard tracks the connections that have not authenticated yet, oldest first.
type gatewayConnGuard struct {
	limits gatewayLimits

	mu        sync.Mutex
	order     *list.List // of *gatewaySlot, in admission order
	perSource map[string]int
	evicted   int
	lastLog   time.Time
}

func newGatewayConnGuard(limits gatewayLimits) *gatewayConnGuard { //nolint:gocritic // hugeParam: value semantics intentional
	return &gatewayConnGuard{limits: limits, order: list.New(), perSource: map[string]int{}}
}

// gatewaySource is the key a source's pending slots are counted under: the IPv4 address, or the
// IPv6 /64, since one host usually holds a whole /64.
func gatewaySource(addr net.Addr) string {
	ap, err := netip.ParseAddrPort(addr.String())
	if err != nil {
		return addr.String()
	}
	ip := ap.Addr().Unmap()
	if ip.Is6() {
		return netip.PrefixFrom(ip, 64).Masked().String()
	}
	return ip.String()
}

// admit takes a pending slot for raw and arms its auth deadline. At the cap it closes an older
// pending connection to make room; it does not refuse.
func (g *gatewayConnGuard) admit(raw net.Conn) *gatewaySlot {
	src := gatewaySource(raw.RemoteAddr())
	s := &gatewaySlot{guard: g, raw: raw, src: src}

	g.mu.Lock()
	victim := g.victimLocked(src)
	if victim != nil {
		g.removeLocked(victim)
		victim.timer.Stop()
		g.evicted++
		if now := time.Now(); now.Sub(g.lastLog) >= gatewayEvictionLogEvery {
			slog.Warn("external gateway closing the oldest unauthenticated connections: too many have not authenticated yet",
				"maxPending", g.limits.maxPending, "closed", g.evicted, "source", victim.src)
			g.evicted = 0
			g.lastLog = now
		}
	}
	s.elem = g.order.PushBack(s)
	g.perSource[src]++
	s.timer = time.AfterFunc(g.limits.authDeadline, s.expire)
	g.mu.Unlock()

	if victim != nil {
		_ = victim.raw.Close()
	}
	return s
}

/*
victimLocked picks the pending slot a new connection from incoming pushes out: none below maxPending,
otherwise one of the source holding the most, the new connection counted, so a flooding source
pushes out its own first; a tie goes to incoming, else to the source with the oldest slot. Within
that source the oldest connection still in its TLS handshake goes first, and only a source with none
loses its oldest past TLS: bare TCP sockets then displace each other, and pushing out an agent that
is already sending its first RPC takes a TLS handshake per connection.
*/
func (g *gatewayConnGuard) victimLocked(incoming string) *gatewaySlot {
	if g.order.Len() < g.limits.maxPending {
		return nil
	}
	most := g.perSource[incoming] + 1
	for _, n := range g.perSource {
		most = max(most, n)
	}
	from := incoming
	if n := g.perSource[incoming]; n == 0 || n+1 < most {
		for e := g.order.Front(); e != nil; e = e.Next() {
			if v, _ := e.Value.(*gatewaySlot); g.perSource[v.src] == most {
				from = v.src
				break
			}
		}
	}
	var oldestPastTLS *gatewaySlot
	for e := g.order.Front(); e != nil; e = e.Next() {
		v, _ := e.Value.(*gatewaySlot)
		switch {
		case v.src != from:
		case !v.handshook:
			return v
		case oldestPastTLS == nil:
			oldestPastTLS = v
		}
	}
	return oldestPastTLS
}

// removeLocked gives a slot back; it reports false when it already was.
func (g *gatewayConnGuard) removeLocked(s *gatewaySlot) bool {
	if s.elem == nil {
		return false
	}
	g.order.Remove(s.elem)
	s.elem = nil
	g.perSource[s.src]--
	if g.perSource[s.src] <= 0 {
		delete(g.perSource, s.src)
	}
	return true
}

// gatewaySlot is one connection's pending slot. It is given back exactly once: when the connection
// authenticates, closes, reaches its auth deadline or is pushed out, whichever comes first.
type gatewaySlot struct {
	guard *gatewayConnGuard
	raw   net.Conn
	src   string
	// elem, timer and handshook are guarded by guard.mu; elem is nil once the slot is given back.
	elem      *list.Element
	timer     *time.Timer
	handshook bool
}

func (s *gatewaySlot) release() bool {
	s.guard.mu.Lock()
	defer s.guard.mu.Unlock()
	if !s.guard.removeLocked(s) {
		return false
	}
	s.timer.Stop()
	return true
}

// handshakeDone records that the connection finished TLS, which victimLocked weighs.
func (s *gatewaySlot) handshakeDone() {
	s.guard.mu.Lock()
	s.handshook = true
	s.guard.mu.Unlock()
}

// authenticated converts the slot: the connection is a fleet member from here on.
func (s *gatewaySlot) authenticated() { s.release() }

func (s *gatewaySlot) closed() { s.release() }

// expire closes the raw TCP connection, which also aborts a TLS handshake still in progress; grpc
// tears the transport down on the read error.
func (s *gatewaySlot) expire() {
	if s.release() {
		slog.Debug("external gateway closed a connection that did not authenticate in time",
			"remote", s.raw.RemoteAddr().String(), "authDeadline", s.guard.limits.authDeadline)
		_ = s.raw.Close()
	}
}

// gatewayCreds wraps the gateway's TLS credentials so every connection passes through the guard
// before the handshake and carries its slot to the interceptors in its AuthInfo.
type gatewayCreds struct {
	credentials.TransportCredentials
	guard *gatewayConnGuard
}

func (c *gatewayCreds) ServerHandshake(raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	slot := c.guard.admit(raw)
	conn, info, err := c.TransportCredentials.ServerHandshake(raw)
	if err != nil {
		slot.closed()
		return nil, nil, err
	}
	tlsInfo, ok := info.(credentials.TLSInfo)
	if !ok {
		slot.closed()
		_ = conn.Close()
		return nil, nil, errors.New("external gateway: transport credentials are not TLS")
	}
	slot.handshakeDone()
	return &gatewayConn{Conn: conn, slot: slot}, gatewayAuthInfo{TLSInfo: tlsInfo, slot: slot}, nil
}

func (c *gatewayCreds) Clone() credentials.TransportCredentials {
	return &gatewayCreds{TransportCredentials: c.TransportCredentials.Clone(), guard: c.guard}
}

// gatewayAuthInfo is the TLS AuthInfo plus the connection's slot; certIdentities reads the TLS half.
type gatewayAuthInfo struct {
	credentials.TLSInfo
	slot *gatewaySlot
}

// gatewayConn gives the slot back when grpc closes a connection that never authenticated.
type gatewayConn struct {
	net.Conn
	slot *gatewaySlot
}

func (c *gatewayConn) Close() error {
	c.slot.closed()
	return c.Conn.Close()
}

// markAuthenticated is called once an RPC has passed the token check.
func markAuthenticated(ctx context.Context) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return
	}
	if info, ok := p.AuthInfo.(gatewayAuthInfo); ok {
		info.slot.authenticated()
	}
}
