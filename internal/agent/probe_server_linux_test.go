package agent

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A datagram to a broadcast address reaches every agent on the segment; none may answer it, and none
// may log an ERROR line for it.
func TestProbeServerIgnoresDatagramsToABroadcastAddress(t *testing.T) {
	var bcast net.IP
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() {
			if ones, _ := n.Mask.Size(); ones <= 30 {
				ip, mask := n.IP.To4(), n.Mask[len(n.Mask)-4:]
				bcast = net.IPv4(ip[0]|^mask[0], ip[1]|^mask[1], ip[2]|^mask[2], ip[3]|^mask[3])
				break
			}
		}
	}
	if bcast == nil {
		t.Skip("no IPv4 interface with a broadcast address")
	}
	logs := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	srv := NewProbeServer(0)
	if err := srv.ListenUDP(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	port := srv.listener.LocalAddr().(*net.UDPAddr).Port

	for _, dst := range []net.IP{bcast, net.IPv4bcast} {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{})
		if err != nil {
			t.Fatal(err)
		}
		rc, err := c.SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		_ = rc.Control(func(fd uintptr) { _ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1) })
		for range 5 {
			if _, err = c.WriteTo([]byte{0, 0, 0, 7}, &net.UDPAddr{IP: dst, Port: port}); err != nil {
				t.Logf("send to %v: %v", dst, err)
			}
		}
		_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		if n, from, err := c.ReadFrom(make([]byte, 16)); err == nil {
			t.Errorf("a datagram to %v got a %d-byte echo from %v", dst, n, from)
		}
		_ = c.Close()
	}
	if n := strings.Count(logs.String(), `msg="UDP write error"`); n != 0 {
		t.Errorf("%d 'UDP write error' lines for datagrams to broadcast addresses, want 0:\n%s", n, logs.String())
	}
}
