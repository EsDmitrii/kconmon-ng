//go:build !linux && !darwin

package checker

import "net"

// Elsewhere (Windows) the probe reports itself unsupported; CI keeps GOOS=windows vet green on it.
const pmtuSupported = false

func setDontFragment(*net.UDPConn, bool) error { return errPMTUUnsupported }

func reportedPathMTU(*net.UDPConn, bool) int { return 0 }
