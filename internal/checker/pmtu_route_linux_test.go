//go:build linux

package checker

import (
	"encoding/binary"
	"errors"
	"net"
	"slices"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func nlU32(v uint32) []byte { return binary.NativeEndian.AppendUint32(nil, v) }

// nlAttr is one rtattr padded to four bytes, the way the kernel lays them out.
func nlAttr(typ uint16, payload ...[]byte) []byte {
	body := slices.Concat(payload...)
	b := binary.NativeEndian.AppendUint16(nil, uint16(unix.SizeofRtAttr+len(body)))
	b = binary.NativeEndian.AppendUint16(b, typ)
	b = append(b, body...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

func nlMsg(typ uint16, seq uint32, body []byte) []byte {
	b := binary.NativeEndian.AppendUint32(nil, uint32(unix.SizeofNlMsghdr+len(body)))
	b = binary.NativeEndian.AppendUint16(b, typ)
	b = binary.NativeEndian.AppendUint16(b, 0)
	b = binary.NativeEndian.AppendUint32(b, seq)
	b = binary.NativeEndian.AppendUint32(b, 0)
	return append(b, body...)
}

// newRoute is an RTM_NEWROUTE reply: the rtmsg header, then attributes.
func newRoute(seq uint32, family uint8, attrs ...[]byte) []byte {
	dstLen := uint8(32)
	if family == unix.AF_INET6 {
		dstLen = 128
	}
	hdr := make([]byte, 0, unix.SizeofRtMsg)
	hdr = append(hdr, family, dstLen, 0, 0, unix.RT_TABLE_MAIN, unix.RTPROT_BOOT, unix.RT_SCOPE_UNIVERSE, unix.RTN_UNICAST)
	hdr = append(hdr, nlU32(0)...) // rtm_flags
	return nlMsg(unix.RTM_NEWROUTE, seq, slices.Concat(append([][]byte{hdr}, attrs...)...))
}

func nlErr(seq uint32, errno unix.Errno) []byte {
	body := binary.NativeEndian.AppendUint32(nil, uint32(-int32(errno)))
	return nlMsg(unix.NLMSG_ERROR, seq, append(body, make([]byte, unix.SizeofNlMsghdr)...))
}

func rtNexthop(ifindex uint32, attrs ...[]byte) []byte {
	body := slices.Concat(attrs...)
	b := binary.NativeEndian.AppendUint16(nil, uint16(unix.SizeofRtNexthop+len(body)))
	b = append(b, 0, 0)
	b = binary.NativeEndian.AppendUint32(b, ifindex)
	return append(b, body...)
}

// Replies shaped like the kernel's answer to RTM_GETROUTE with RTM_F_FIB_MATCH: Cilium's pod route
// carries an mtu metric next to its egress device, a plain route carries only the device.
func TestParseRouteReply(t *testing.T) {
	const seq = 7
	gw4 := nlAttr(unix.RTA_GATEWAY, net.IPv4(10, 0, 0, 1).To4())
	gw6 := nlAttr(unix.RTA_GATEWAY, net.ParseIP("fd00::1"))
	cases := []struct {
		name  string
		reply []byte
		want  fibRoute
		err   string
	}{
		{"v4 route mtu", newRoute(seq, unix.AF_INET, nlAttr(unix.RTA_TABLE, nlU32(unix.RT_TABLE_MAIN)),
			nlAttr(unix.RTA_METRICS, nlAttr(unix.RTAX_MTU, nlU32(1450))), gw4, nlAttr(unix.RTA_OIF, nlU32(12))),
			fibRoute{mtu: 1450, oifs: []int{12}}, ""},
		{"v4 no metrics", newRoute(seq, unix.AF_INET, gw4, nlAttr(unix.RTA_OIF, nlU32(2))),
			fibRoute{oifs: []int{2}}, ""},
		{"v4 metrics without mtu", newRoute(seq, unix.AF_INET,
			nlAttr(unix.RTA_METRICS, nlAttr(unix.RTAX_ADVMSS, nlU32(1460))), nlAttr(unix.RTA_OIF, nlU32(2))),
			fibRoute{oifs: []int{2}}, ""},
		{"v6 route mtu", newRoute(seq, unix.AF_INET6, nlAttr(unix.RTA_PRIORITY, nlU32(1024)),
			nlAttr(unix.RTA_METRICS, nlAttr(unix.RTAX_HOPLIMIT, nlU32(64)), nlAttr(unix.RTAX_MTU, nlU32(1355))),
			gw6, nlAttr(unix.RTA_OIF, nlU32(4))),
			fibRoute{mtu: 1355, oifs: []int{4}}, ""},
		{"v6 no metrics", newRoute(seq, unix.AF_INET6, gw6, nlAttr(unix.RTA_OIF, nlU32(4))),
			fibRoute{oifs: []int{4}}, ""},
		{"multipath", newRoute(seq, unix.AF_INET, nlAttr(unix.RTA_METRICS, nlAttr(unix.RTAX_MTU, nlU32(1400))),
			nlAttr(unix.RTA_MULTIPATH, rtNexthop(2, gw4), rtNexthop(5, gw4))),
			fibRoute{mtu: 1400, oifs: []int{2, 5}}, ""},
		{"stale message skipped", slices.Concat(newRoute(seq-1, unix.AF_INET, nlAttr(unix.RTA_OIF, nlU32(9))),
			newRoute(seq, unix.AF_INET, nlAttr(unix.RTA_OIF, nlU32(3)))),
			fibRoute{oifs: []int{3}}, ""},
		{"no route", nlErr(seq, unix.ENETUNREACH), fibRoute{}, "network is unreachable"},
		{"permission", nlErr(seq, unix.EPERM), fibRoute{}, "operation not permitted"},
		{"only a stale reply", newRoute(seq+1, unix.AF_INET, nlAttr(unix.RTA_OIF, nlU32(3))), fibRoute{}, "no route"},
		{"truncated", newRoute(seq, unix.AF_INET, nlAttr(unix.RTA_OIF, nlU32(3)))[:20], fibRoute{}, "truncated"},
		{"attribute overruns", newRoute(seq, unix.AF_INET, []byte{0xff, 0, unix.RTA_OIF, 0, 3, 0, 0, 0}),
			fibRoute{}, "malformed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRouteReply(tc.reply, seq)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("parseRouteReply = %+v, %v; want an error naming %q", got, err, tc.err)
				}
				return
			}
			if err != nil || got.mtu != tc.want.mtu || !slices.Equal(got.oifs, tc.want.oifs) {
				t.Fatalf("parseRouteReply = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}

	if _, err := parseRouteReply(nlErr(seq, unix.ENETUNREACH), seq); !errors.Is(err, unix.ENETUNREACH) {
		t.Errorf("error reply = %v, want it to wrap ENETUNREACH", err)
	}
}

// The probe fits the smallest of the route's mtu metric and its egress devices.
func TestFIBRouteProbeMTU(t *testing.T) {
	devs := map[int]int{2: 1500, 3: 1450, 4: 65536}
	linkMTU := func(i int) (int, error) {
		if mtu, ok := devs[i]; ok {
			return mtu, nil
		}
		return 0, errors.New("no such device")
	}
	cases := []struct {
		name  string
		route fibRoute
		want  int
		err   bool
	}{
		{"route mtu below the device", fibRoute{mtu: 1450, oifs: []int{2}}, 1450, false},
		{"no route mtu", fibRoute{oifs: []int{2}}, 1500, false},
		{"route mtu above the device", fibRoute{mtu: 9000, oifs: []int{2}}, 1500, false},
		{"multipath takes the smallest device", fibRoute{oifs: []int{2, 3}}, 1450, false},
		{"loopback", fibRoute{oifs: []int{4}}, 65536, false},
		{"no egress device", fibRoute{mtu: 1400}, 0, true},
		{"unknown device", fibRoute{oifs: []int{9}}, 0, true},
	}
	for _, tc := range cases {
		got, err := tc.route.probeMTU(linkMTU)
		if (err != nil) != tc.err || got != tc.want {
			t.Errorf("%s: probeMTU = %d, %v; want %d (error %v)", tc.name, got, err, tc.want, tc.err)
		}
	}
}

func TestRouteRequest(t *testing.T) {
	cases := []struct {
		remote, local net.IP
		family        uint8
		addrLen       int
	}{
		{net.ParseIP("10.244.1.7"), net.ParseIP("10.244.0.3"), unix.AF_INET, 4},
		{net.ParseIP("fd00:10:244:1::7"), net.ParseIP("fd00:10:244::3"), unix.AF_INET6, 16},
		{net.ParseIP("10.244.1.7"), nil, unix.AF_INET, 4},
	}
	for _, tc := range cases {
		req := routeRequest(tc.remote, tc.local, 42)
		msgs, err := syscall.ParseNetlinkMessage(req)
		if err != nil || len(msgs) != 1 {
			t.Fatalf("%v: request does not parse as one netlink message: %v", tc.remote, err)
		}
		m := msgs[0]
		if m.Header.Type != unix.RTM_GETROUTE || m.Header.Flags != unix.NLM_F_REQUEST || m.Header.Seq != 42 {
			t.Fatalf("%v: header = %+v, want RTM_GETROUTE, NLM_F_REQUEST, seq 42", tc.remote, m.Header)
		}
		rtm := m.Data[:unix.SizeofRtMsg]
		if rtm[0] != tc.family || int(rtm[1]) != 8*tc.addrLen ||
			binary.NativeEndian.Uint32(rtm[8:12]) != unix.RTM_F_FIB_MATCH {
			t.Fatalf("%v: rtmsg = %v, want family %d, dst_len %d and RTM_F_FIB_MATCH", tc.remote, rtm, tc.family, 8*tc.addrLen)
		}
		// The stdlib parses attributes of route replies only; a request has the same layout.
		m.Header.Type = unix.RTM_NEWROUTE
		attrs, err := syscall.ParseNetlinkRouteAttr(&m)
		if err != nil {
			t.Fatal(err)
		}
		got := map[uint16][]byte{}
		for _, a := range attrs {
			got[a.Attr.Type] = a.Value
		}
		if !net.IP(got[unix.RTA_DST]).Equal(tc.remote) || len(got[unix.RTA_DST]) != tc.addrLen {
			t.Errorf("%v: RTA_DST = %v", tc.remote, got[unix.RTA_DST])
		}
		if src, ok := got[unix.RTA_SRC]; (tc.local == nil) == ok || (ok && !net.IP(src).Equal(tc.local)) {
			t.Errorf("%v: RTA_SRC = %v, want %v", tc.remote, src, tc.local)
		}
	}
}

// Against this kernel: the route to loopback leaves through lo, so the answer is lo's MTU.
func TestRouteMTUFromTheKernel(t *testing.T) {
	lo, err := net.InterfaceByName("lo")
	if err != nil {
		t.Skipf("no lo here: %v", err)
	}
	for _, addr := range []string{"127.0.0.1", "::1"} {
		ip := net.ParseIP(addr)
		got, err := routeMTU(ip, ip)
		if addr == "::1" && err != nil {
			t.Logf("no IPv6 loopback route here: %v", err)
			continue
		}
		if err != nil || got != lo.MTU {
			t.Errorf("routeMTU(%s) = %d, %v; want lo's %d", addr, got, err, lo.MTU)
		}
	}
}
