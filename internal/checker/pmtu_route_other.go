//go:build !linux

package checker

import "net"

func routeMTU(net.IP, net.IP) (int, error) { return 0, errRouteMTUUnsupported }
