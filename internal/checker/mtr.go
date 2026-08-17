package checker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

type MTRChecker struct {
	maxHops  int
	timeout  time.Duration
	cooldown time.Duration

	mu      sync.Mutex
	lastRun map[string]time.Time
}

func NewMTRChecker(maxHops int, timeout, cooldown time.Duration) *MTRChecker {
	return &MTRChecker{
		maxHops:  maxHops,
		timeout:  timeout,
		cooldown: cooldown,
		lastRun:  make(map[string]time.Time),
	}
}

func (c *MTRChecker) Name() model.CheckType {
	return model.CheckMTR
}

// TryAcquire checks whether MTR can run for the given source-destination pair and.
func (c *MTRChecker) TryAcquire(source, destination string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := source + "->" + destination
	if last, ok := c.lastRun[key]; ok && time.Since(last) < c.cooldown {
		return false
	}

	for k, t := range c.lastRun {
		if time.Since(t) >= c.cooldown {
			delete(c.lastRun, k)
		}
	}

	c.lastRun[key] = time.Now()
	return true
}

func (c *MTRChecker) Check(ctx context.Context, target Target) model.CheckResult { //nolint:gocritic // hugeParam: Target is a VALUE by design -- a checker must not be able to mutate the caller's copy, and one 80-byte copy per probe is nothing next to the probe itself
	// Every other checker fills Duration; MTR did not, so a SUCCESSFUL trace
	// reached the console as durationNs 0 while a failed one carried the
	// console's own wall clock (the runner's error branch measures it there).
	// The whole trace is what a traceroute takes -- per-hop RTTs are their own
	// field and are not summed into this.
	start := time.Now()
	result := model.CheckResult{
		Type:      model.CheckMTR,
		Timestamp: start,
	}

	ip := net.ParseIP(target.PodIP)
	if ip == nil {
		result.Error = fmt.Sprintf("invalid IP: %s", target.PodIP)
		result.Duration = time.Since(start)
		return result
	}

	hops, reached, err := c.traceroute(ctx, ip)
	if err != nil {
		result.Error = fmt.Sprintf("MTR traceroute: %v", err)
		result.Duration = time.Since(start)
		return result
	}

	details := &model.MTRDetails{
		Target:  target.PodIP,
		Hops:    hops,
		Reached: reached,
	}

	/* Success means the DESTINATION answered. A trace that ran out of TTLs in silence used to be
	   reported exactly like one that arrived — and MTR fires when a pair is already failing, so the
	   one measurement taken during an incident was the one that claimed everything was fine. */
	result.Success = reached
	if !reached {
		result.Error = fmt.Sprintf("MTR: %s did not answer within %d hops (%s)",
			target.PodIP, c.maxHops, lastAnsweringHop(hops))
	}
	result.Details = details
	result.Duration = time.Since(start)

	slog.Info("MTR trace completed",
		"target", target.PodIP,
		"hops", len(hops),
		"reached", reached,
		"targetNode", target.NodeName,
	)

	return result
}

// hopLookupTimeout bounds the reverse lookup of ONE hop; see its use.
const hopLookupTimeout = 500 * time.Millisecond

// mtrRawUnavailableOnce keeps the "no NET_RAW" warning to one line per process: it is a property of
// the deployment, not of the probe, and a trace fires on every failed peer check.
var mtrRawUnavailableOnce sync.Once

/*
 * replyMatches decides whether an ICMP message is the answer to THIS hop's probe.
 *
 * A raw socket sees every ICMP message the host receives, so identity has to come from the message:
 *   - TimeExceeded quotes the datagram that expired, and the echo header inside it carries the id
 *     and seq this trace wrote (seq == the TTL). That is an intermediate hop answering.
 *   - EchoReply carries the id and seq directly. That is the destination answering.
 * Anything else — another trace's traffic, a straggler from an earlier TTL, an unreachable — is not
 * this hop's reading and must not be recorded as one.
 *
 * matchID is false on the DATAGRAM socket, and that is not a relaxation: there the kernel overwrites
 * the echo id with the socket's source port and demultiplexes replies by it, so every reply arriving
 * is this trace's by construction — while comparing the id we asked for rejected all of them,
 * including the destination's own.
 */
func replyMatches(reply *icmp.Message, replyType, exceededType icmp.Type, id, seq int, matchID bool) bool {
	switch reply.Type {
	case replyType:
		echo, ok := reply.Body.(*icmp.Echo)
		if !ok || echo.Seq != seq {
			return false
		}
		return !matchID || echo.ID == id
	case exceededType:
		te, ok := reply.Body.(*icmp.TimeExceeded)
		if !ok {
			return false
		}
		return quotedEchoMatches(te.Data, id, seq, matchID)
	default:
		return false
	}
}

/*
 * quotedEchoMatches reads the echo header out of the IP datagram an ICMP error quotes back.
 *
 * The quote is the original IP header followed by the first 8 bytes of its payload — which for an
 * echo request is exactly type/code/checksum/id/seq. The IP header length is the low nibble of its
 * first byte, in 32-bit words; anything shorter than that plus 8 is a quote too small to identify.
 */
func quotedEchoMatches(quoted []byte, id, seq int, matchID bool) bool {
	if len(quoted) < 1 {
		return false
	}
	// IPv6 has no variable header here: the quote starts with a fixed 40-byte header.
	headerLen := 40
	if version := quoted[0] >> 4; version == 4 {
		headerLen = int(quoted[0]&0x0f) * 4
	}
	if headerLen < 20 || len(quoted) < headerLen+8 {
		return false
	}
	echo := quoted[headerLen:]
	gotID := int(echo[4])<<8 | int(echo[5])
	gotSeq := int(echo[6])<<8 | int(echo[7])
	if gotSeq != seq {
		return false
	}
	return !matchID || gotID == id
}

/*
 * traceroute walks the TTLs to dst and reports the hops AND whether the destination itself answered.
 *
 * The bool is the whole difference between a path and a guess. Without it a trace that heard nothing
 * — no router, not even the destination — returned maxHops silent entries and a nil error, and every
 * caller read that as a completed measurement: `success: true`, a run in status `succeeded` with
 * pairOk 1, and kconmon_ng_mtr_hops publishing 30 as the length of a two-hop pod-to-pod path. A
 * trace that did not reach its destination measured nothing, and now says so.
 */
func (c *MTRChecker) traceroute(ctx context.Context, dst net.IP) ([]model.MTRHop, bool, error) {
	isIPv6 := dst.To4() == nil

	var (
		network      string
		rawNetwork   string
		icmpType     icmp.Type
		replyType    icmp.Type
		exceededType icmp.Type
		proto        int
	)
	if isIPv6 {
		network, rawNetwork = "udp6", "ip6:ipv6-icmp"
		icmpType, replyType, exceededType = ipv6.ICMPTypeEchoRequest, ipv6.ICMPTypeEchoReply, ipv6.ICMPTypeTimeExceeded
		proto = 58
	} else {
		network, rawNetwork = "udp4", "ip4:icmp"
		icmpType, replyType, exceededType = ipv4.ICMPTypeEcho, ipv4.ICMPTypeEchoReply, ipv4.ICMPTypeTimeExceeded
		proto = 1
	}

	/* A RAW socket first, the datagram one only as a fallback.
	   The datagram-ICMP socket ("udp4") delivers echo REPLIES and nothing else: the TTL-exceeded
	   messages that intermediate routers send are not readable on it, so every hop before the
	   destination reads as silent. The DaemonSet requests NET_RAW for exactly this; where the
	   capability does not reach the process's effective set (a non-root container with
	   allowPrivilegeEscalation:false — the chart's own default) the datagram socket still hears the
	   destination, so the trace degrades to what it always was rather than failing.

	   WHICH socket it is decides two things below, and getting either wrong makes the whole trace a
	   fiction: the ADDRESS TYPE a write takes, and whether the echo ID means anything. */
	raw := true
	conn, rawErr := icmp.ListenPacket(rawNetwork, "")
	if rawErr != nil {
		raw = false
		var dgramErr error
		conn, dgramErr = icmp.ListenPacket(network, "")
		if dgramErr != nil {
			return nil, false, fmt.Errorf("ICMP listen: %w", errors.Join(rawErr, dgramErr))
		}
		mtrRawUnavailableOnce.Do(func() {
			slog.Warn("MTR: no raw ICMP socket (NET_RAW), tracing on the datagram socket: "+
				"intermediate hops cannot answer there and will be recorded as silent",
				"error", rawErr)
		})
	}
	defer func() { _ = conn.Close() }()

	/* The destination address, in the shape THIS socket accepts. A raw ICMP conn is a *net.IPConn
	   underneath and its WriteTo type-asserts *net.IPAddr — handed a *net.UDPAddr it returns EINVAL
	   for every packet, so the trace sent nothing at all and came back empty AND successful. The
	   datagram conn is a *net.UDPConn and wants the other one. */
	var target net.Addr = &net.UDPAddr{IP: dst}
	if raw {
		target = &net.IPAddr{IP: dst}
	}

	/* A per-TRACE id, not the process id.
	   On a raw socket every trace in this netns sees every other trace's replies, and traces now run
	   concurrently (one per failed peer). With a process-wide id, a TimeExceeded caused by peer B's
	   route matched peer D's trace at the same TTL, and D's path was written with B's routers in it.
	   A random id per trace is what makes "this reply is mine" mean something.

	   On the DATAGRAM socket the id is not ours to choose: the kernel overwrites it with the
	   socket's source port and demultiplexes replies by that port, so a reply arriving on this
	   socket is by construction this trace's. Checking the id there rejected every reply, including
	   the destination's own — which is why matchID is false for that path. */
	id := int(rand.Int32N(0xffff)) //nolint:gosec // trace correlation, not a secret
	matchID := raw
	hops := make([]model.MTRHop, 0, c.maxHops)
	buf := make([]byte, 1500)
	sent := 0

	for ttl := 1; ttl <= c.maxHops; ttl++ {
		select {
		case <-ctx.Done():
			return hops, false, ctx.Err()
		default:
		}

		if isIPv6 {
			if err := conn.IPv6PacketConn().SetHopLimit(ttl); err != nil {
				continue
			}
		} else {
			if err := conn.IPv4PacketConn().SetTTL(ttl); err != nil {
				continue
			}
		}

		msg := icmp.Message{
			Type: icmpType,
			Code: 0,
			Body: &icmp.Echo{ID: id, Seq: ttl, Data: []byte("kconmon-ng-mtr")},
		}
		msgBytes, err := msg.Marshal(nil)
		if err != nil {
			continue
		}

		start := time.Now()
		if _, writeErr := conn.WriteTo(msgBytes, target); writeErr != nil {
			/* Counted rather than swallowed. Every write failing is not a route of silent hops, it
			   is a trace that never happened, and returning that as a successful empty path is how a
			   green result came to mean nothing at all. */
			slog.Debug("MTR: probe write failed", "ttl", ttl, "error", writeErr)
			continue
		}
		sent++

		if deadlineErr := conn.SetReadDeadline(time.Now().Add(c.timeout)); deadlineErr != nil {
			continue
		}

		hop := model.MTRHop{Number: ttl, IP: "*", LossRatio: 1.0}
		/* Read until THIS hop's deadline. A raw socket sees every ICMP message in the netns, so a
		   reply for another trace, or for one of this trace's earlier TTLs, can arrive first; one
		   read per hop would take it as the answer or, having consumed it, miss the real one. */
		arrived := false
		for !arrived {
			n, peer, readErr := conn.ReadFrom(buf)
			if readErr != nil {
				break // this hop's deadline, or a dead socket
			}
			reply, parseErr := icmp.ParseMessage(proto, buf[:n])
			if parseErr != nil || !replyMatches(reply, replyType, exceededType, id, ttl, matchID) {
				continue // not this hop's; keep waiting inside the same deadline
			}

			ip := hopIPFromAddr(peer)
			/* RTT is set only when something ANSWERED. It used to be assigned unconditionally, so a
			   silent hop carried RTT == c.timeout — one second — and that deadline was stored, served
			   as `rttNs`, drawn in the hop table and published on the per-hop gauge: a uniform
			   1000ms latency wall that exists nowhere on the network. */
			hop.IP = ip
			hop.LossRatio = 0.0
			hop.RTT = time.Since(start)
			arrived = true

			/* The reverse lookup gets its OWN small budget. It is a network call per answering hop,
			   made while the trace holds the loop, and on the default resolver an unreachable
			   CoreDNS means the resolv.conf ladder. The address is the reading; the name is a
			   courtesy. */
			lookupCtx, cancelLookup := context.WithTimeout(ctx, hopLookupTimeout)
			names, _ := net.DefaultResolver.LookupAddr(lookupCtx, ip)
			cancelLookup()
			if len(names) > 0 {
				hop.Hostname = names[0]
			}

			if reply.Type == replyType {
				// The DESTINATION answered: the route ends here. `break` would only leave the read
				// loop, and the trace then ran on to maxHops appending this same hop again and again.
				hops = append(hops, hop)
				return hops, true, nil
			}
		}

		hops = append(hops, hop)
	}

	if sent == 0 {
		return nil, false, fmt.Errorf("MTR: no probe could be sent (raw=%t)", raw)
	}
	/* Out of TTLs with no reply from the destination: the hops collected are what was seen on the
	   way, and the path is INCOMPLETE.

	   TRAILING silent hops are dropped. Each TTL past the last router that answered contributes a
	   "*" and nothing else, and there is no evidence any of them exist: a destination that is merely
	   SLOW (which is the condition that triggers a trace in the first place) keeps the loop walking
	   to maxHops, and every one of those TTLs became a hop. kconmon_ng_mtr_hops is published from
	   len(hops), so the path length grew with the tracer's patience rather than with the network —
	   a pair whose destination went quiet reported 30 hops where the real path is 4. Silent hops
	   BETWEEN answers stay: those are real routers that decline to reply, and their position is
	   established by the answers on either side. */
	return trimTrailingSilentHops(hops), false, nil
}

/*
trimTrailingSilentHops drops the run of unanswered hops at the END of an incomplete trace.

Only the tail: a "*" with an answer after it is a real router that did not reply, and dropping it
would renumber everything behind it. A "*" with nothing after it is the tracer counting its own
TTLs.
*/
func trimTrailingSilentHops(hops []model.MTRHop) []model.MTRHop {
	end := len(hops)
	for end > 0 {
		h := hops[end-1]
		if h.IP != "" && h.IP != "*" {
			break
		}
		end--
	}
	return hops[:end]
}

// lastAnsweringHop names the furthest hop that replied, which is where the operator's attention
// belongs: "no reply past 10.244.1.1 (hop 2)" is a lead, "the trace failed" is not.
func lastAnsweringHop(hops []model.MTRHop) string {
	for i := len(hops) - 1; i >= 0; i-- {
		if hops[i].IP != "" && hops[i].IP != "*" {
			return fmt.Sprintf("last reply from %s at hop %d", hops[i].IP, hops[i].Number)
		}
	}
	return "no hop answered"
}

// hopIPFromAddr extracts the bare IP from a net.Addr returned by an ICMP socket's ReadFrom.
func hopIPFromAddr(addr net.Addr) string {
	switch a := addr.(type) {
	case *net.IPAddr:
		return a.IP.String()
	case *net.UDPAddr:
		return a.IP.String()
	}

	s := addr.String()
	if host, _, err := net.SplitHostPort(s); err == nil {
		return host
	}
	return s
}
