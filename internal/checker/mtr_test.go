package checker

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

func TestMTRCheckerTryAcquire(t *testing.T) {
	c := NewMTRChecker(30, 1*time.Second, 5*time.Second)

	// First call: should succeed and record the time.
	if !c.TryAcquire("node-1", "node-2") {
		t.Error("expected TryAcquire=true on first call")
	}

	// Immediate second call: cooldown not elapsed.
	if c.TryAcquire("node-1", "node-2") {
		t.Error("expected TryAcquire=false within cooldown")
	}

	// Different destination: independent cooldown, should succeed.
	if !c.TryAcquire("node-1", "node-3") {
		t.Error("expected TryAcquire=true for different pair")
	}
}

func TestMTRCheckerTryAcquireCooldownExpired(t *testing.T) {
	c := NewMTRChecker(30, 1*time.Second, 100*time.Millisecond)

	// Seed an old entry so the cooldown is already expired.
	c.mu.Lock()
	c.lastRun["node-1->node-2"] = time.Now().Add(-200 * time.Millisecond)
	c.mu.Unlock()

	if !c.TryAcquire("node-1", "node-2") {
		t.Error("expected TryAcquire=true after cooldown expired")
	}
}

func TestMTRCheckerTryAcquireAtomicRecord(t *testing.T) {
	c := NewMTRChecker(30, 1*time.Second, 1*time.Second)

	// TryAcquire must record the run so that a subsequent ShouldRun-like
	// check immediately blocks without a separate MarkRun step.
	if !c.TryAcquire("src", "dst") {
		t.Fatal("expected first TryAcquire=true")
	}

	c.mu.Lock()
	_, recorded := c.lastRun["src->dst"]
	c.mu.Unlock()

	if !recorded {
		t.Error("TryAcquire must record the key atomically")
	}
}

func TestMTRCheckerExpiredEntriesPurged(t *testing.T) {
	c := NewMTRChecker(30, 1*time.Second, 100*time.Millisecond)

	// Seed several expired entries directly.
	c.mu.Lock()
	for i := range 5 {
		key := fmt.Sprintf("node-%d->node-x", i)
		c.lastRun[key] = time.Now().Add(-200 * time.Millisecond)
	}
	c.mu.Unlock()

	// TryAcquire for a new pair triggers cleanup of the expired entries.
	if !c.TryAcquire("node-new", "node-x") {
		t.Fatal("expected TryAcquire=true for new pair")
	}

	c.mu.Lock()
	remaining := len(c.lastRun)
	c.mu.Unlock()

	// Only the newly recorded entry should remain.
	if remaining != 1 {
		t.Errorf("expected 1 entry after purge, got %d", remaining)
	}
}

// MTR was the ONE checker that never filled CheckResult.Duration, so every
// successful trace persisted durationNs 0 while a FAILED one showed 2-3ms --
// the console's own wall clock, filled in by the runner's error branch. A run
// where the failures are the only rows with a duration is backwards.
func TestMTRCheckerFillsDurationOnEveryPath(t *testing.T) {
	c := NewMTRChecker(30, 1*time.Second, 0)

	// The invalid-IP path returns before any packet is sent and is the one
	// failure this test can produce without a network.
	got := c.Check(t.Context(), Target{PodIP: "not-an-ip", NodeName: "node-b"})
	if got.Success {
		t.Fatalf("Check(invalid IP) succeeded, want a failure: %+v", got)
	}
	if got.Duration <= 0 {
		t.Errorf("Duration = %s, want the time actually spent (> 0)", got.Duration)
	}
}

func TestHopIPFromAddrStripsPort(t *testing.T) {
	cases := []struct {
		name string
		addr net.Addr
		want string
	}{
		{"IPAddr v4", &net.IPAddr{IP: net.ParseIP("10.244.1.11")}, "10.244.1.11"},
		{"IPAddr v6", &net.IPAddr{IP: net.ParseIP("fe80::1")}, "fe80::1"},
		{"UDPAddr with port", &net.UDPAddr{IP: net.ParseIP("10.244.1.11"), Port: 0}, "10.244.1.11"},
		{"UDPAddr v6 with port", &net.UDPAddr{IP: net.ParseIP("fe80::1"), Port: 12345}, "fe80::1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hopIPFromAddr(tc.addr)
			if got != tc.want {
				t.Errorf("hopIPFromAddr(%v) = %q, want %q", tc.addr, got, tc.want)
			}
			// A "host:port" string (as produced by UDPAddr.String()) must not
			// survive extraction; a bare IPv6 address is not itself a valid
			// "host:port" pair, so SplitHostPort must fail on the result.
			if _, _, err := net.SplitHostPort(got); err == nil {
				t.Errorf("hopIPFromAddr(%v) = %q, still looks like host:port", tc.addr, got)
			}
		})
	}
}

/* ── a reply belongs to the hop that asked for it ────────────────────────── */

/*
 * A raw ICMP socket sees every message the host receives — another process's traffic, a straggler
 * from an earlier TTL, an unreachable. Recording whatever arrives as "this hop answered" is how a
 * trace ends up describing a route nobody walked, so identity comes out of the message itself.
 */
func TestReplyMatchesOnlyThisHopsProbe(t *testing.T) {
	// matchID = true throughout: this is the RAW socket's rule, where the id is ours to choose and is
	// the only thing separating one concurrent trace from another.
	const id, seq = 0x1234, 7

	echoReply := &icmp.Message{
		Type: ipv4.ICMPTypeEchoReply,
		Body: &icmp.Echo{ID: id, Seq: seq},
	}
	if !replyMatches(echoReply, ipv4.ICMPTypeEchoReply, ipv4.ICMPTypeTimeExceeded, id, seq, true) {
		t.Error("the destination's own echo reply was not recognised")
	}
	// Same shape, another probe's identity.
	other := &icmp.Message{Type: ipv4.ICMPTypeEchoReply, Body: &icmp.Echo{ID: id ^ 0xff, Seq: seq}}
	if replyMatches(other, ipv4.ICMPTypeEchoReply, ipv4.ICMPTypeTimeExceeded, id, seq, true) {
		t.Error("another process's echo reply was accepted as this hop's")
	}
	stale := &icmp.Message{Type: ipv4.ICMPTypeEchoReply, Body: &icmp.Echo{ID: id, Seq: seq - 1}}
	if replyMatches(stale, ipv4.ICMPTypeEchoReply, ipv4.ICMPTypeTimeExceeded, id, seq, true) {
		t.Error("a straggler from an earlier TTL was accepted as this hop's")
	}

	// An intermediate router's TimeExceeded, quoting our own datagram back at us.
	quoted := make([]byte, 20+8)
	quoted[0] = 0x45 // IPv4, 5-word header
	quoted[20] = 8   // echo request
	quoted[24], quoted[25] = byte(id>>8), byte(id&0xff)
	quoted[26], quoted[27] = byte(seq>>8), byte(seq&0xff)
	exceeded := &icmp.Message{Type: ipv4.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: quoted}}
	if !replyMatches(exceeded, ipv4.ICMPTypeEchoReply, ipv4.ICMPTypeTimeExceeded, id, seq, true) {
		t.Error("an intermediate hop's TTL-exceeded was not recognised — every hop before the destination would read as silent")
	}

	// The same message quoting somebody else's datagram (the LOW byte of the id is what differs).
	quoted[25] = byte((id ^ 0xff) & 0xff)
	if replyMatches(exceeded, ipv4.ICMPTypeEchoReply, ipv4.ICMPTypeTimeExceeded, id, seq, true) {
		t.Error("a TTL-exceeded for another probe was accepted as this hop's")
	}

	// A quote too short to identify anything.
	short := &icmp.Message{Type: ipv4.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: []byte{0x45, 0, 0}}}
	if replyMatches(short, ipv4.ICMPTypeEchoReply, ipv4.ICMPTypeTimeExceeded, id, seq, true) {
		t.Error("a truncated quote was accepted")
	}
}

/*
 * On the DATAGRAM socket the id is not ours: Linux overwrites the echo id with the socket's source
 * port and demultiplexes replies by it. Comparing the id we asked for therefore rejected every
 * reply, the destination's included, and a trace came back as nothing but silent hops — which is the
 * default deployment's path, since NET_RAW does not reach the effective set of a non-root container.
 */
func TestReplyMatchesIgnoresTheIDOnTheDatagramSocket(t *testing.T) {
	const seq = 3
	kernelChosen := &icmp.Message{
		Type: ipv4.ICMPTypeEchoReply,
		Body: &icmp.Echo{ID: 0x9a3f, Seq: seq}, // a source port, not our id
	}
	if replyMatches(kernelChosen, ipv4.ICMPTypeEchoReply, ipv4.ICMPTypeTimeExceeded, 0x1234, seq, true) {
		t.Error("the raw rule accepted a reply whose id is not ours — concurrent traces would cross")
	}
	if !replyMatches(kernelChosen, ipv4.ICMPTypeEchoReply, ipv4.ICMPTypeTimeExceeded, 0x1234, seq, false) {
		t.Error("the datagram rule rejected the destination's own reply; every hop would read as silent")
	}
	// The SEQ still separates one hop from another on both paths.
	stale := &icmp.Message{Type: ipv4.ICMPTypeEchoReply, Body: &icmp.Echo{ID: 0x9a3f, Seq: seq - 1}}
	if replyMatches(stale, ipv4.ICMPTypeEchoReply, ipv4.ICMPTypeTimeExceeded, 0x1234, seq, false) {
		t.Error("a straggler from an earlier TTL was accepted on the datagram path")
	}
}

/* ── a trace against a real destination ──────────────────────────────────── */

/*
 * The end-to-end shape, on loopback, where the destination answers at TTL 1. It pins the three
 * things a rewrite of this function got wrong at once: the write must reach the socket at all (a raw
 * conn takes *net.IPAddr and returns EINVAL for a *net.UDPAddr, so every probe silently failed and
 * the trace came back empty AND successful), the destination must END the trace (a `break` inside
 * the switch left the loop running to maxHops, appending the same hop twice per TTL), and the reply
 * must be recognised on whichever socket the process could open.
 */
func TestTracerouteReachesLoopbackAndStops(t *testing.T) {
	c := NewMTRChecker(8, 2*time.Second, time.Minute)

	hops, reached, err := c.traceroute(context.Background(), net.ParseIP("127.0.0.1"))
	if err != nil {
		// No ICMP socket at all in this environment (a sandbox without ping_group_range): the trace
		// cannot be exercised here, and saying so is better than a test that passes by accident.
		t.Skipf("no usable ICMP socket in this environment: %v", err)
	}

	if len(hops) == 0 {
		t.Fatal("traceroute to loopback returned NO hops — every probe failed to send")
	}
	/* The destination ANSWERED, and the tracer has to say so: a trace that ran out of TTLs in
	   silence used to be indistinguishable from this one, and every caller called both a success. */
	if !reached {
		t.Error("traceroute to loopback reports the destination was not reached")
	}
	if len(hops) > 4 {
		t.Errorf("traceroute to loopback returned %d hops, want it to stop at the destination", len(hops))
	}
	last := hops[len(hops)-1]
	if last.IP != "127.0.0.1" {
		t.Errorf("last hop = %q, want the destination 127.0.0.1 (hops: %+v)", last.IP, hops)
	}
	if last.LossRatio != 0 || last.RTT <= 0 {
		t.Errorf("the destination hop reads as silent: %+v", last)
	}
	// No hop is repeated: that was the `break`-inside-the-switch defect.
	seen := map[int]bool{}
	for _, h := range hops {
		if seen[h.Number] {
			t.Fatalf("hop number %d appears twice: %+v", h.Number, hops)
		}
		seen[h.Number] = true
	}
}

/* ── QA round 5: a trace that reached nothing is not a measurement ────────── */

/*
 * The destination is what makes a trace a trace. Before this, traceroute returned (hops, nil) the
 * moment ONE probe had been written — no matter what came back — and Check set Success=true on that
 * alone. A trace into a black hole came back as maxHops silent entries, `success: true`, a run in
 * status `succeeded` with pairOk 1, and kconmon_ng_mtr_hops publishing 30 as the length of a
 * two-hop pod-to-pod path. MTR fires when a pair is ALREADY failing, so the one measurement taken
 * during an incident was the one that claimed everything was fine.
 */
func TestMTRUnreachedDestinationIsNotSuccess(t *testing.T) {
	// 3 TTLs at 150 ms so the whole trace is under half a second; 192.0.2.1 is TEST-NET-1 (RFC 5737)
	// and answers nothing anywhere.
	c := NewMTRChecker(3, 150*time.Millisecond, time.Minute)

	result := c.Check(context.Background(), Target{PodIP: "192.0.2.1", NodeName: "nowhere"})
	if result.Error != "" && result.Details == nil {
		t.Skipf("no usable ICMP socket in this environment: %s", result.Error)
	}

	if result.Success {
		t.Error("a trace whose destination never answered is reported as a successful measurement")
	}
	if result.Error == "" {
		t.Error("an unreached destination carries no error text, so nothing downstream can say why")
	}
	details, ok := result.Details.(*model.MTRDetails)
	if !ok {
		t.Fatalf("details are %T, want *model.MTRDetails", result.Details)
	}
	if details.Reached {
		t.Error("Reached is true for a destination that never answered")
	}
}

func TestLastAnsweringHop(t *testing.T) {
	if got := lastAnsweringHop(nil); got != "no hop answered" {
		t.Errorf("empty trace = %q", got)
	}
	if got := lastAnsweringHop([]model.MTRHop{{Number: 1, IP: "*"}, {Number: 2, IP: "*"}}); got != "no hop answered" {
		t.Errorf("all-silent trace = %q", got)
	}
	hops := []model.MTRHop{{Number: 1, IP: "10.0.0.1"}, {Number: 2, IP: "10.0.0.2"}, {Number: 3, IP: "*"}}
	if got := lastAnsweringHop(hops); got != "last reply from 10.0.0.2 at hop 2" {
		t.Errorf("last answering hop = %q", got)
	}
}
