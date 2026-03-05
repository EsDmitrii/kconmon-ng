package checker

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

type ICMPChecker struct {
	timeout time.Duration
}

func NewICMPChecker(timeout time.Duration) *ICMPChecker {
	return &ICMPChecker{timeout: timeout}
}

func (c *ICMPChecker) Name() model.CheckType {
	return model.CheckICMP
}

func (c *ICMPChecker) Check(ctx context.Context, target Target) model.CheckResult {
	result := model.CheckResult{
		Type:      model.CheckICMP,
		Timestamp: time.Now(),
	}

	ip := net.ParseIP(target.PodIP)
	if ip == nil {
		result.Error = fmt.Sprintf("invalid IP: %s", target.PodIP)
		return result
	}

	isIPv6 := ip.To4() == nil

	var (
		network  string
		icmpType icmp.Type
		proto    int
	)

	if isIPv6 {
		network = "udp6"
		icmpType = ipv6.ICMPTypeEchoRequest
		proto = 58 // ICMPv6
	} else {
		network = "udp4"
		icmpType = ipv4.ICMPTypeEcho
		proto = 1 // ICMPv4
	}

	conn, err := icmp.ListenPacket(network, "")
	if err != nil {
		result.Error = fmt.Sprintf("ICMP listen: %v", err)
		return result
	}
	defer func() { _ = conn.Close() }()

	id := os.Getpid() & 0xffff
	seq := int(time.Now().UnixNano() & 0xffff)

	msg := icmp.Message{
		Type: icmpType,
		Code: 0,
		Body: &icmp.Echo{
			ID:   id,
			Seq:  seq,
			Data: []byte("kconmon-ng"),
		},
	}

	msgBytes, err := msg.Marshal(nil)
	if err != nil {
		result.Error = fmt.Sprintf("ICMP marshal: %v", err)
		return result
	}

	dst := &net.UDPAddr{IP: ip}

	start := time.Now()
	if _, writeErr := conn.WriteTo(msgBytes, dst); writeErr != nil {
		result.Error = fmt.Sprintf("ICMP write: %v", writeErr)
		result.Duration = time.Since(start)
		return result
	}

	if readErr := conn.SetReadDeadline(time.Now().Add(c.timeout)); readErr != nil {
		result.Error = fmt.Sprintf("ICMP set deadline: %v", readErr)
		result.Duration = time.Since(start)
		return result
	}
	buf := make([]byte, 1500)
	n, _, err := conn.ReadFrom(buf)
	rtt := time.Since(start)

	if err != nil {
		result.Error = fmt.Sprintf("ICMP read: %v", err)
		result.Duration = rtt
		result.Details = &model.ICMPDetails{RTT: rtt, LossRatio: 1.0}
		return result
	}

	reply, err := icmp.ParseMessage(proto, buf[:n])
	if err != nil {
		result.Error = fmt.Sprintf("ICMP parse: %v", err)
		result.Duration = rtt
		return result
	}

	var replyType icmp.Type
	if isIPv6 {
		replyType = ipv6.ICMPTypeEchoReply
	} else {
		replyType = ipv4.ICMPTypeEchoReply
	}

	if reply.Type != replyType {
		result.Error = fmt.Sprintf("unexpected ICMP type: %v", reply.Type)
		result.Duration = rtt
		return result
	}

	result.Success = true
	result.Duration = rtt
	result.Details = &model.ICMPDetails{
		RTT:       rtt,
		LossRatio: 0.0,
	}

	return result
}
