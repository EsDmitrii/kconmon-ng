package checker

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

func startUDPEchoServer(t *testing.T) (port int, cleanup func()) {
	t.Helper()

	lc := net.ListenConfig{}
	conn, err := lc.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	port = conn.LocalAddr().(*net.UDPAddr).Port

	done := make(chan struct{})
	go func() {
		buf := make([]byte, 1024)
		for {
			select {
			case <-done:
				return
			default:
			}
			_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				continue
			}
			if n >= 4 {
				resp := make([]byte, 4)
				copy(resp, buf[:4])
				_, _ = conn.WriteTo(resp, addr)
			}
		}
	}()

	return port, func() {
		close(done)
		_ = conn.Close()
	}
}

func TestUDPCheckerSuccess(t *testing.T) {
	port, cleanup := startUDPEchoServer(t)
	defer cleanup()

	c := NewUDPChecker(1*time.Second, 5, port)
	result := c.Check(context.Background(), Target{
		PodIP: "127.0.0.1",
	})

	if !result.Success {
		t.Errorf("expected success, got error: %s", result.Error)
	}
	if result.Type != model.CheckUDP {
		t.Errorf("expected type UDP, got %s", result.Type)
	}

	details, ok := result.Details.(*model.UDPDetails)
	if !ok {
		t.Fatal("expected UDPDetails")
	}
	if details.PacketsSent != 5 {
		t.Errorf("expected 5 packets sent, got %d", details.PacketsSent)
	}
	if details.PacketsRecv < 1 {
		t.Error("expected at least 1 packet received")
	}
	if details.MeanRTT <= 0 {
		t.Error("expected positive mean RTT")
	}
}

func TestUDPCheckerNoServer(t *testing.T) {
	c := NewUDPChecker(200*time.Millisecond, 3, 19999)
	result := c.Check(context.Background(), Target{
		PodIP: "127.0.0.1",
	})

	details, ok := result.Details.(*model.UDPDetails)
	if !ok {
		t.Fatal("expected UDPDetails")
	}
	if details.LossRatio != 1.0 {
		t.Errorf("expected 100%% loss, got %.2f", details.LossRatio)
	}
}

func TestUDPCheckerContextCancel(t *testing.T) {
	port, cleanup := startUDPEchoServer(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := NewUDPChecker(1*time.Second, 5, port)
	result := c.Check(ctx, Target{
		PodIP: "127.0.0.1",
	})

	if result.Success {
		t.Error("expected failure on cancelled context")
	}
}

func TestMeanDuration(t *testing.T) {
	tests := []struct {
		name string
		ds   []time.Duration
		want time.Duration
	}{
		{"empty", nil, 0},
		{"single", []time.Duration{100 * time.Millisecond}, 100 * time.Millisecond},
		{"two", []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}, 150 * time.Millisecond},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := meanDuration(tt.ds)
			if got != tt.want {
				t.Errorf("meanDuration() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestJitterDuration(t *testing.T) {
	ds := []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		15 * time.Millisecond,
	}

	j := jitterDuration(ds)
	if j <= 0 {
		t.Error("expected positive jitter")
	}

	if jitterDuration(nil) != 0 {
		t.Error("expected 0 jitter for empty slice")
	}
	if jitterDuration([]time.Duration{10 * time.Millisecond}) != 0 {
		t.Error("expected 0 jitter for single element")
	}
}

func TestParseUDPPacket(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		wantSeq uint32
		wantOk  bool
	}{
		{"valid", func() []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, 42); return b }(), 42, true},
		{"too short", []byte{0x01, 0x02}, 0, false},
		{"empty", nil, 0, false},
		{"zero seq", make([]byte, 4), 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seq, ok := ParseUDPPacket(tt.data)
			if ok != tt.wantOk {
				t.Errorf("ParseUDPPacket() ok = %v, want %v", ok, tt.wantOk)
			}
			if seq != tt.wantSeq {
				t.Errorf("ParseUDPPacket() seq = %d, want %d", seq, tt.wantSeq)
			}
		})
	}
}
