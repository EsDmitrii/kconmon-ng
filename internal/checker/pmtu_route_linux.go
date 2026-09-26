//go:build linux

package checker

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

var routeSeq atomic.Uint32

// routeMTU asks the kernel's FIB for the route from local to remote and returns the size the probe
// should test: the route's mtu metric when it has one, never above the egress device's MTU. With
// RTM_F_FIB_MATCH the kernel answers from the configured route, not from its cache, so an MTU learned
// from an earlier frag-needed does not shrink the probe and hide a reduced path.
func routeMTU(remote, local net.IP) (int, error) {
	seq := routeSeq.Add(1)
	reply, err := routeRoundTrip(routeRequest(remote, local, seq))
	if err != nil {
		return 0, err
	}
	route, err := parseRouteReply(reply, seq)
	if err != nil {
		return 0, err
	}
	return route.probeMTU(linkMTU)
}

func linkMTU(ifindex int) (int, error) {
	ifc, err := net.InterfaceByIndex(ifindex)
	if err != nil {
		return 0, err
	}
	return ifc.MTU, nil
}

func routeRoundTrip(req []byte) ([]byte, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("netlink socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	tv := unix.Timeval{Sec: 1}
	if err = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return nil, fmt.Errorf("netlink socket: %w", err)
	}
	kernel := &unix.SockaddrNetlink{Family: unix.AF_NETLINK}
	if err = unix.Sendto(fd, req, 0, kernel); err != nil {
		return nil, fmt.Errorf("netlink RTM_GETROUTE: %w", err)
	}
	buf := make([]byte, 8192)
	n, _, err := unix.Recvfrom(fd, buf, 0)
	if err != nil {
		return nil, fmt.Errorf("netlink RTM_GETROUTE reply: %w", err)
	}
	return buf[:n], nil
}

// routeRequest is what `ip route get fibmatch <remote> from <local>` sends.
func routeRequest(remote, local net.IP, seq uint32) []byte {
	family, addrBits, dst, src := uint8(unix.AF_INET6), uint8(128), remote.To16(), local.To16()
	if local.To4() != nil {
		src = nil
	}
	if v4 := remote.To4(); v4 != nil {
		family, addrBits, dst, src = unix.AF_INET, 32, v4, local.To4()
	}
	attrs := routeAttr(unix.RTA_DST, dst)
	srcBits := uint8(0)
	if src != nil && !src.IsUnspecified() {
		attrs = append(attrs, routeAttr(unix.RTA_SRC, src)...)
		srcBits = addrBits
	}
	b := binary.NativeEndian.AppendUint32(nil, uint32(unix.SizeofNlMsghdr+unix.SizeofRtMsg+len(attrs))) //nolint:gosec // G115: two addresses, under 100 bytes
	b = binary.NativeEndian.AppendUint16(b, unix.RTM_GETROUTE)
	b = binary.NativeEndian.AppendUint16(b, unix.NLM_F_REQUEST)
	b = binary.NativeEndian.AppendUint32(b, seq)
	b = binary.NativeEndian.AppendUint32(b, 0)
	b = append(b, family, addrBits, srcBits, 0, 0, 0, 0, 0)
	b = binary.NativeEndian.AppendUint32(b, unix.RTM_F_FIB_MATCH)
	return append(b, attrs...)
}

func routeAttr(typ uint16, value []byte) []byte {
	b := binary.NativeEndian.AppendUint16(nil, uint16(unix.SizeofRtAttr+len(value))) //nolint:gosec // G115: an address, 16 bytes at most
	b = binary.NativeEndian.AppendUint16(b, typ)
	b = append(b, value...)
	for len(b)%unix.NLMSG_ALIGNTO != 0 {
		b = append(b, 0)
	}
	return b
}

// fibRoute is what a route reply says about size: the mtu metric (0 when the route has none) and
// the egress devices, several on a multipath route.
type fibRoute struct {
	mtu  int
	oifs []int
}

// probeMTU fits the smallest egress device of a multipath route, since a flow may hash to any of them.
func (r fibRoute) probeMTU(linkMTU func(int) (int, error)) (int, error) {
	if len(r.oifs) == 0 {
		return 0, errors.New("the route reply names no egress device")
	}
	size := 0
	for _, oif := range r.oifs {
		mtu, err := linkMTU(oif)
		if err != nil {
			return 0, fmt.Errorf("MTU of egress device %d: %w", oif, err)
		}
		if size == 0 || mtu < size {
			size = mtu
		}
	}
	if r.mtu > 0 && r.mtu < size {
		size = r.mtu
	}
	return size, nil
}

var errRouteReplyMalformed = errors.New("malformed netlink route reply")

func parseRouteReply(b []byte, seq uint32) (fibRoute, error) {
	for len(b) > 0 {
		if len(b) < unix.SizeofNlMsghdr {
			return fibRoute{}, fmt.Errorf("%w: truncated message header", errRouteReplyMalformed)
		}
		msgLen := int(binary.NativeEndian.Uint32(b[0:4]))
		if msgLen < unix.SizeofNlMsghdr || msgLen > len(b) {
			return fibRoute{}, fmt.Errorf("%w: truncated message", errRouteReplyMalformed)
		}
		typ, msgSeq := binary.NativeEndian.Uint16(b[4:6]), binary.NativeEndian.Uint32(b[8:12])
		body := b[unix.SizeofNlMsghdr:msgLen]
		b = b[min(nlAlign(msgLen), len(b)):]
		if msgSeq != seq {
			continue
		}
		switch typ {
		case unix.NLMSG_ERROR:
			if len(body) < 4 {
				return fibRoute{}, fmt.Errorf("%w: truncated error", errRouteReplyMalformed)
			}
			// nlmsgerr.error is a negative errno; 0 is an acknowledgement.
			code := binary.NativeEndian.Uint32(body[0:4])
			if code == 0 {
				continue
			}
			return fibRoute{}, fmt.Errorf("netlink RTM_GETROUTE: %w", unix.Errno(-code))
		case unix.RTM_NEWROUTE:
			if len(body) < unix.SizeofRtMsg {
				return fibRoute{}, fmt.Errorf("%w: truncated rtmsg", errRouteReplyMalformed)
			}
			return parseRouteAttrs(body[unix.SizeofRtMsg:])
		}
	}
	return fibRoute{}, errors.New("netlink RTM_GETROUTE: no route in the reply")
}

func parseRouteAttrs(b []byte) (fibRoute, error) {
	var r fibRoute
	err := walkAttrs(b, func(typ uint16, v []byte) error {
		switch typ {
		case unix.RTA_OIF:
			if len(v) < 4 {
				return errRouteReplyMalformed
			}
			r.oifs = append(r.oifs, int(binary.NativeEndian.Uint32(v)))
		case unix.RTA_METRICS:
			return walkAttrs(v, func(typ uint16, v []byte) error {
				if typ == unix.RTAX_MTU && len(v) >= 4 {
					r.mtu = int(binary.NativeEndian.Uint32(v))
				}
				return nil
			})
		case unix.RTA_MULTIPATH:
			for len(v) >= unix.SizeofRtNexthop {
				n := int(binary.NativeEndian.Uint16(v[0:2]))
				if n < unix.SizeofRtNexthop || n > len(v) {
					return errRouteReplyMalformed
				}
				r.oifs = append(r.oifs, int(binary.NativeEndian.Uint32(v[4:8])))
				v = v[min(nlAlign(n), len(v)):]
			}
		}
		return nil
	})
	return r, err
}

func walkAttrs(b []byte, fn func(typ uint16, value []byte) error) error {
	for len(b) >= unix.SizeofRtAttr {
		n := int(binary.NativeEndian.Uint16(b[0:2]))
		if n < unix.SizeofRtAttr || n > len(b) {
			return errRouteReplyMalformed
		}
		if err := fn(binary.NativeEndian.Uint16(b[2:4])&^(unix.NLA_F_NESTED|unix.NLA_F_NET_BYTEORDER), b[unix.SizeofRtAttr:n]); err != nil {
			return err
		}
		b = b[min(nlAlign(n), len(b)):]
	}
	return nil
}

func nlAlign(n int) int {
	return (n + unix.NLMSG_ALIGNTO - 1) &^ (unix.NLMSG_ALIGNTO - 1)
}
