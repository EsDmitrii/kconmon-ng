package checker

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"strconv"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

type UDPChecker struct {
	timeout time.Duration
	packets int
	port    int
}

func NewUDPChecker(timeout time.Duration, packets, port int) *UDPChecker {
	return &UDPChecker{
		timeout: timeout,
		packets: packets,
		port:    port,
	}
}

func (c *UDPChecker) Name() model.CheckType {
	return model.CheckUDP
}

func (c *UDPChecker) Check(ctx context.Context, target Target) model.CheckResult {
	result := model.CheckResult{
		Type:      model.CheckUDP,
		Timestamp: time.Now(),
	}

	addr := net.JoinHostPort(target.PodIP, strconv.Itoa(c.port))
	raddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		result.Error = fmt.Sprintf("resolve UDP addr: %v", err)
		return result
	}

	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		result.Error = fmt.Sprintf("UDP dial: %v", err)
		return result
	}
	defer func() { _ = conn.Close() }()

	rtts := make([]time.Duration, 0, c.packets)
	sent := 0
	received := 0

	for i := 0; i < c.packets; i++ {
		select {
		case <-ctx.Done():
			result.Error = "context cancelled"
			return result
		default:
		}

		payload := make([]byte, 4)
		binary.BigEndian.PutUint32(payload, uint32(i))

		if err := conn.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
			continue
		}
		sendTime := time.Now()
		if _, err := conn.Write(payload); err != nil {
			continue
		}
		sent++

		if err := conn.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
			continue
		}
		buf := make([]byte, 1024)
		n, err := conn.Read(buf)
		rtt := time.Since(sendTime)

		if err != nil {
			continue
		}

		if n >= 4 {
			respSeq := binary.BigEndian.Uint32(buf[:4])
			if respSeq == uint32(i) {
				received++
				rtts = append(rtts, rtt)
			}
		}
	}

	details := &model.UDPDetails{
		PacketsSent: sent,
		PacketsRecv: received,
	}

	if sent > 0 {
		details.LossRatio = 1.0 - float64(received)/float64(sent)
	}

	if len(rtts) > 0 {
		details.MeanRTT = meanDuration(rtts)
		details.Variance = varianceDuration(rtts, details.MeanRTT)
		details.Jitter = jitterDuration(rtts)
	}

	result.Success = received > 0 && details.LossRatio < 1.0
	result.Duration = details.MeanRTT
	result.Details = details

	if !result.Success {
		result.Error = fmt.Sprintf("UDP loss: %.0f%% (%d/%d)", details.LossRatio*100, sent-received, sent)
	}

	return result
}

func meanDuration(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	var sum int64
	for _, d := range ds {
		sum += int64(d)
	}
	return time.Duration(sum / int64(len(ds)))
}

func varianceDuration(ds []time.Duration, mean time.Duration) time.Duration {
	if len(ds) < 2 {
		return 0
	}
	var sum float64
	for _, d := range ds {
		diff := float64(d - mean)
		sum += diff * diff
	}
	return time.Duration(math.Sqrt(sum / float64(len(ds)-1)))
}

func jitterDuration(ds []time.Duration) time.Duration {
	if len(ds) < 2 {
		return 0
	}
	var sum int64
	for i := 1; i < len(ds); i++ {
		diff := int64(ds[i]) - int64(ds[i-1])
		if diff < 0 {
			diff = -diff
		}
		sum += diff
	}
	return time.Duration(sum / int64(len(ds)-1))
}

// ParseUDPPacket extracts the sequence number from a UDP probe packet.
// Returns the sequence number and true if valid, or 0 and false otherwise.
func ParseUDPPacket(data []byte) (uint32, bool) {
	if len(data) < 4 {
		return 0, false
	}
	return binary.BigEndian.Uint32(data[:4]), true
}
