//go:build darwin

package checker

import (
	"net"

	"golang.org/x/sys/unix"
)

// Darwin is a development target: DF works, the learned MTU cannot be read back, so a refusal
// arrives without a number and the search bisects to it.
const pmtuSupported = true

func setDontFragment(conn *net.UDPConn, v6 bool) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		if v6 {
			serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_DONTFRAG, 1)
			return
		}
		serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_DONTFRAG, 1)
	}); err != nil {
		return err
	}
	return serr
}

func reportedPathMTU(*net.UDPConn, bool) int { return 0 }
