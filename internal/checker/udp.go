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

func (c *UDPChecker) Check(ctx context.Context, target Target) model.CheckResult { //nolint:gocritic // hugeParam: Target is a VALUE by design -- a checker must not be able to mutate the caller's copy, and one 80-byte copy per probe is nothing next to the probe itself
	result := model.CheckResult{
		Type:      model.CheckUDP,
		Timestamp: time.Now(),
	}

	/* A PEER is probed on the checker's own configured echo port -- that is where the agent listens,
	   and Target.Port carries the peer's HTTP port, not a UDP one. An EXTERNAL destination has no
	   agent behind it, so the port the operator asked for is the only port that means anything;
	   probing c.port there sent every packet to the wrong place and reported 100% loss.

	   With no port at all an external target has nothing to fall back TO. Falling back to c.port
	   pointed the probe at the agent's own echo port (config.grpcPort, 9090 by default) on someone
	   else's host: a UDP diagnostic against a public address that reported a clean 100% loss while
	   naming the operator's intended destination, and quietly sent packets at a port nobody asked
	   about. An error says the same thing honestly. */
	port := c.port
	if target.External {
		if target.Port == 0 {
			result.Error = "external UDP destination has no port: name one on the target (address host:port, or the port field)"
			return result
		}
		port = target.Port
	}
	addr := net.JoinHostPort(target.PodIP, strconv.Itoa(port))
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
	/* attempted counts the packets this probe TRIED to put on the wire, and writeErr keeps the first
	   reason one could not go.
	   Write failures used to be swallowed with a bare `continue`: on a total blackout — the peer's
	   route gone, EPERM, ECONNREFUSED on the connected socket — nothing was sent, `sent` stayed 0,
	   the loss computation was skipped, and LossRatio kept its 0.0 zero value. The gauge then
	   published "no loss" for a pair where not one datagram left the host, so a
	   packet_loss_ratio > 0.5 alert stayed silent through the outage, and the operator-facing error
	   read "UDP loss: 0% (0/0)". */
	attempted := 0
	var writeErr error

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
		attempted++
		if _, err := conn.Write(payload); err != nil {
			if writeErr == nil {
				writeErr = err
			}
			continue
		}
		sent++

		/* Read until this packet's own deadline, not once.
		   A single read per packet desynchronised the loop for good: a reply that arrived just after
		   the deadline stayed queued in the socket, so the NEXT iteration's read returned the
		   PREVIOUS packet's reply, its sequence never matched again, and a pair where every datagram
		   was delivered reported 100% loss. One ordinary latency spike past checkers.udp.timeout
		   (250ms) was enough, and the false reading then fed the loss gauge and the matrix.
		   A reply for an EARLIER sequence is a late delivery of a packet already counted lost, and it
		   is drained rather than mistaken for this one; a reply for THIS sequence ends the wait. */
		deadline := time.Now().Add(c.timeout)
		if err := conn.SetReadDeadline(deadline); err != nil {
			continue
		}
		buf := make([]byte, 1024)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				break // deadline for this packet, or a dead socket; either way it is lost
			}
			if n < 4 {
				continue // not one of ours; keep waiting within the same deadline
			}
			respSeq := binary.BigEndian.Uint32(buf[:4])
			if respSeq < uint32(i) { //nolint:gosec // i is bounded by c.packets
				continue // a late reply to an earlier packet: drained, not counted
			}
			if respSeq == uint32(i) { //nolint:gosec // as above
				received++
				rtts = append(rtts, time.Since(sendTime))
			}
			break
		}
	}

	details := &model.UDPDetails{
		PacketsSent: sent,
		PacketsRecv: received,
	}

	switch {
	case sent > 0:
		details.LossRatio = 1.0 - float64(received)/float64(sent)
	case attempted > 0:
		// Nothing left the host. That is total loss, not the absence of a reading.
		details.LossRatio = 1.0
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
		if sent == 0 && writeErr != nil {
			// The write error itself, which nothing recorded before: "UDP loss: 0% (0/0)" told the
			// operator neither what happened nor that anything had.
			result.Error = fmt.Sprintf("UDP send failed for all %d packets: %v", attempted, writeErr)
		} else {
			result.Error = fmt.Sprintf("UDP loss: %.0f%% (%d/%d)", details.LossRatio*100, sent-received, sent)
		}
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
