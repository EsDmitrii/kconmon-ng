//go:build linux

package checker

import (
	"net"

	"golang.org/x/sys/unix"
)

const pmtuSupported = true

// setDontFragment puts the socket in PROBE mode: DF on every datagram, sized against the device MTU
// and never against the kernel's cached path MTU, so each probe asks the path again instead of
// reading back what the previous probe taught the kernel.
func setDontFragment(conn *net.UDPConn, v6 bool) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		if v6 {
			serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_MTU_DISCOVER, unix.IPV6_PMTUDISC_PROBE)
			return
		}
		serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_MTU_DISCOVER, unix.IP_PMTUDISC_PROBE)
	}); err != nil {
		return err
	}
	return serr
}

// reportedPathMTU reads the MTU the kernel learned from the ICMP frag-needed that caused EMSGSIZE.
func reportedPathMTU(conn *net.UDPConn, v6 bool) int {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0
	}
	mtu := 0
	_ = raw.Control(func(fd uintptr) {
		var v int
		var gerr error
		if v6 {
			v, gerr = unix.GetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_MTU)
		} else {
			v, gerr = unix.GetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_MTU)
		}
		if gerr == nil {
			mtu = v
		}
	})
	return mtu
}
