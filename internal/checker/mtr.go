package checker

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
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

// TryAcquire checks whether MTR can run for the given source-destination pair
// and, if so, atomically records the current time to enforce the cooldown.
// Returns true if the trace should proceed, false if still within cooldown.
func (c *MTRChecker) TryAcquire(source, destination string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := source + "->" + destination
	if last, ok := c.lastRun[key]; ok && time.Since(last) < c.cooldown {
		return false
	}
	c.lastRun[key] = time.Now()
	return true
}

func (c *MTRChecker) Check(ctx context.Context, target Target) model.CheckResult {
	result := model.CheckResult{
		Type:      model.CheckMTR,
		Timestamp: time.Now(),
	}

	ip := net.ParseIP(target.PodIP)
	if ip == nil {
		result.Error = fmt.Sprintf("invalid IP: %s", target.PodIP)
		return result
	}

	hops, err := c.traceroute(ctx, ip)
	if err != nil {
		result.Error = fmt.Sprintf("MTR traceroute: %v", err)
		return result
	}

	details := &model.MTRDetails{
		Target: target.PodIP,
		Hops:   hops,
	}

	result.Success = true
	result.Details = details

	slog.Info("MTR trace completed",
		"target", target.PodIP,
		"hops", len(hops),
		"targetNode", target.NodeName,
	)

	return result
}

func (c *MTRChecker) traceroute(ctx context.Context, dst net.IP) ([]model.MTRHop, error) {
	isIPv6 := dst.To4() == nil

	var (
		network   string
		icmpType  icmp.Type
		replyType icmp.Type
		proto     int
	)
	if isIPv6 {
		network = "udp6"
		icmpType = ipv6.ICMPTypeEchoRequest
		replyType = ipv6.ICMPTypeEchoReply
		proto = 58
	} else {
		network = "udp4"
		icmpType = ipv4.ICMPTypeEcho
		replyType = ipv4.ICMPTypeEchoReply
		proto = 1
	}

	conn, err := icmp.ListenPacket(network, "")
	if err != nil {
		return nil, fmt.Errorf("ICMP listen: %w", err)
	}
	defer func() { _ = conn.Close() }()

	id := os.Getpid() & 0xffff
	hops := make([]model.MTRHop, 0, c.maxHops)

	for ttl := 1; ttl <= c.maxHops; ttl++ {
		select {
		case <-ctx.Done():
			return hops, ctx.Err()
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
			Body: &icmp.Echo{
				ID:   id,
				Seq:  ttl,
				Data: []byte("kconmon-ng-mtr"),
			},
		}

		msgBytes, err := msg.Marshal(nil)
		if err != nil {
			continue
		}

		start := time.Now()
		if _, writeErr := conn.WriteTo(msgBytes, &net.UDPAddr{IP: dst}); writeErr != nil {
			continue
		}

		if deadlineErr := conn.SetReadDeadline(time.Now().Add(c.timeout)); deadlineErr != nil {
			continue
		}
		buf := make([]byte, 1500)
		n, peer, err := conn.ReadFrom(buf)
		rtt := time.Since(start)

		hop := model.MTRHop{
			Number: ttl,
			RTT:    rtt,
		}

		if err != nil {
			hop.IP = "*"
			hop.LossRatio = 1.0
		} else {
			reply, parseErr := icmp.ParseMessage(proto, buf[:n])
			if parseErr != nil {
				hop.IP = "*"
				hop.LossRatio = 1.0
			} else {
				hop.IP = peer.String()
				hop.LossRatio = 0.0

				names, _ := net.DefaultResolver.LookupAddr(ctx, peer.String())
				if len(names) > 0 {
					hop.Hostname = names[0]
				}

				if reply.Type == replyType {
					hops = append(hops, hop)
					break
				}
			}
		}

		hops = append(hops, hop)
	}

	return hops, nil
}
