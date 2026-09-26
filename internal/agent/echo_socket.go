package agent

import (
	"encoding/binary"
	"log/slog"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

/*
echoSocket reads a datagram together with the address it was sent to, and replies from that address.

The probes dial connected sockets, so their kernel drops a reply from any address other than the one
probed. A socket bound to the wildcard lets the kernel pick the reply's source from the route back,
the host's primary address, so an agent probed at a secondary one (advertiseAddress on a multi-homed
host, a VIP) would read as 100% UDP loss and pmtu unreachable while TCP and ICMP stay green.
*/
type echoSocket interface {
	// readFrom returns dst invalid when the platform gives no destination address.
	readFrom(b []byte) (n int, src net.Addr, dst netip.Addr, err error)
	// writeTo lets the kernel pick the source when from is invalid.
	writeTo(b []byte, to net.Addr, from netip.Addr) error
}

// newEchoSocket asks for the destination address of every datagram on conn, and falls back to the
// kernel's choice of reply source where the platform has no such control message (Windows).
func newEchoSocket(conn net.PacketConn) echoSocket {
	uc, ok := conn.(*net.UDPConn)
	if !ok {
		return plainEchoSocket{conn}
	}
	la, _ := uc.LocalAddr().(*net.UDPAddr)
	var err error
	if la != nil && la.IP.To4() != nil {
		pc := ipv4.NewPacketConn(uc)
		if err = pc.SetControlMessage(ipv4.FlagDst|ipv4.FlagInterface, true); err == nil {
			return ipv4EchoSocket{pc}
		}
	} else {
		pc := ipv6.NewPacketConn(uc)
		if err = pc.SetControlMessage(ipv6.FlagDst|ipv6.FlagInterface, true); err == nil {
			return ipv6EchoSocket{pc}
		}
	}
	slog.Debug("UDP echo cannot read destination addresses; replies take the kernel's source address",
		"listen", conn.LocalAddr(), "error", err)
	return plainEchoSocket{conn}
}

type plainEchoSocket struct{ net.PacketConn }

func (p plainEchoSocket) readFrom(b []byte) (n int, src net.Addr, dst netip.Addr, err error) {
	n, src, err = p.ReadFrom(b)
	return n, src, dst, err
}

func (p plainEchoSocket) writeTo(b []byte, to net.Addr, _ netip.Addr) error {
	_, err := p.WriteTo(b, to)
	return err
}

type ipv4EchoSocket struct{ *ipv4.PacketConn }

func (p ipv4EchoSocket) readFrom(b []byte) (n int, src net.Addr, dst netip.Addr, err error) {
	n, cm, src, err := p.ReadFrom(b)
	if cm != nil {
		dst, _ = netip.AddrFromSlice(cm.Dst)
	}
	return n, src, dst.Unmap(), err
}

// writeTo sets only the source: pinning the interface the request came in on would break a host
// whose route back leaves through another one. A source the kernel refuses (the address left the
// host after the read) falls back to the kernel's choice.
func (p ipv4EchoSocket) writeTo(b []byte, to net.Addr, from netip.Addr) error {
	var cm *ipv4.ControlMessage
	if replySource(from) {
		cm = &ipv4.ControlMessage{Src: from.AsSlice()}
	}
	_, err := p.WriteTo(b, cm, to)
	if err != nil && cm != nil {
		_, err = p.WriteTo(b, nil, to)
	}
	return err
}

type ipv6EchoSocket struct{ *ipv6.PacketConn }

func (p ipv6EchoSocket) readFrom(b []byte) (n int, src net.Addr, dst netip.Addr, err error) {
	n, cm, src, err := p.ReadFrom(b)
	if cm != nil {
		dst, _ = netip.AddrFromSlice(cm.Dst)
	}
	return n, src, dst.Unmap(), err
}

// writeTo: an IPv4 datagram on a dual-stack socket keeps the kernel's source, x/net/ipv6 cannot set
// an IPv4 one; ListenUDP opens a socket per family so that never happens there.
func (p ipv6EchoSocket) writeTo(b []byte, to net.Addr, from netip.Addr) error {
	var cm *ipv6.ControlMessage
	if replySource(from) && from.Is6() {
		cm = &ipv6.ControlMessage{Src: from.AsSlice()}
	}
	_, err := p.WriteTo(b, cm, to)
	if err != nil && cm != nil {
		_, err = p.WriteTo(b, nil, to)
	}
	return err
}

var limitedBroadcast = netip.AddrFrom4([4]byte{255, 255, 255, 255})

// replySource reports whether a datagram's destination can be the source of its reply: an
// unspecified one keeps the kernel's choice. The kernel refuses a group or broadcast source, and
// ProbeServer.answers never lets such a datagram reach a reply.
func replySource(dst netip.Addr) bool {
	return dst.IsValid() && !dst.IsUnspecified() && !dst.IsMulticast() && dst != limitedBroadcast
}

// broadcastRefresh is how long a new interface's broadcast address may go unrecognised.
const broadcastRefresh = 30 * time.Second

/*
localBroadcasts knows the host's IPv4 subnet broadcast addresses. A stale set is re-read in the
background: reading the interfaces of a node with hundreds of pods takes milliseconds, which the
serve loop would add to the RTT of the probes waiting behind it. Both serve goroutines ask it.
*/
type localBroadcasts struct {
	addrs      func() ([]net.Addr, error) // nil means net.InterfaceAddrs
	set        atomic.Pointer[broadcastSet]
	refreshing atomic.Bool
}

type broadcastSet struct {
	at    time.Time
	addrs map[netip.Addr]struct{}
}

func (b *localBroadcasts) contains(a netip.Addr) bool {
	// The host part of a broadcast address is all ones and at least two bits long (a /31 has none).
	if !a.Is4() || a.As4()[3]&3 != 3 {
		return false
	}
	set := b.set.Load()
	if set == nil {
		set = b.refresh()
	} else if time.Since(set.at) >= broadcastRefresh && b.refreshing.CompareAndSwap(false, true) {
		go func() {
			defer b.refreshing.Store(false)
			// A caller that loaded the set before the last refresh stored its successor wins the swap late.
			if time.Since(b.set.Load().at) >= broadcastRefresh {
				b.refresh()
			}
		}()
	}
	_, ok := set.addrs[a]
	return ok
}

func (b *localBroadcasts) refresh() *broadcastSet {
	list := b.addrs
	if list == nil {
		list = net.InterfaceAddrs
	}
	addrs, err := list()
	set := &broadcastSet{at: time.Now(), addrs: make(map[netip.Addr]struct{}, len(addrs))}
	if err != nil {
		slog.Debug("UDP echo cannot list the interface addresses to tell broadcasts", "error", err)
	}
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok || n.IP.To4() == nil {
			continue
		}
		ones, bits := n.Mask.Size()
		if bits == 8*net.IPv6len {
			ones -= 8 * (net.IPv6len - net.IPv4len)
		}
		if bits == 0 || ones < 0 || ones > 30 {
			continue
		}
		var bcast [4]byte
		binary.BigEndian.PutUint32(bcast[:], binary.BigEndian.Uint32(n.IP.To4())|^uint32(0)>>ones)
		set.addrs[netip.AddrFrom4(bcast)] = struct{}{}
	}
	b.set.Store(set)
	return set
}
