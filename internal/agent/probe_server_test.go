package agent

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

func TestProbeServerUDP(t *testing.T) {
	srv := NewProbeServer(0)

	lc := net.ListenConfig{}
	listener, err := lc.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.listener = listener
	srv.running.Store(true)
	go srv.serveUDP()
	defer func() { _ = srv.Close() }()

	addr := listener.LocalAddr()

	dialer := net.Dialer{}
	conn, err := dialer.DialContext(context.Background(), "udp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	for seq := range uint32(3) {
		payload := make([]byte, 4)
		binary.BigEndian.PutUint32(payload, seq)

		if _, err := conn.Write(payload); err != nil {
			t.Fatal(err)
		}

		if err := conn.SetReadDeadline(time.Now().Add(1 * time.Second)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 1024)
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("read error for seq %d: %v", seq, err)
		}

		if n < 4 {
			t.Fatalf("response too short: %d bytes", n)
		}

		respSeq := binary.BigEndian.Uint32(buf[:4])
		if respSeq != seq {
			t.Errorf("expected seq %d, got %d", seq, respSeq)
		}
	}
}

func TestProbeServerShortPacket(t *testing.T) {
	srv := NewProbeServer(0)

	lc2 := net.ListenConfig{}
	listener2, err := lc2.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.listener = listener2
	srv.running.Store(true)
	go srv.serveUDP()
	defer func() { _ = srv.Close() }()

	addr := listener2.LocalAddr()

	dialer2 := net.Dialer{}
	conn, err := dialer2.DialContext(context.Background(), "udp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	// Send a packet shorter than 4 bytes -- should be silently ignored
	if _, writeErr := conn.Write([]byte{0x01, 0x02}); writeErr != nil {
		t.Fatal(writeErr)
	}

	if deadlineErr := conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); deadlineErr != nil {
		t.Fatal(deadlineErr)
	}
	buf := make([]byte, 1024)
	_, err = conn.Read(buf)
	if err == nil {
		t.Error("expected timeout for short packet, got response")
	}
}

// A pmtu probe sends up to the interface MTU, jumbo included, and needs its seq back. The server
// reads the whole datagram rather than relying on a truncated read keeping the first four bytes,
// which holds on Linux and Darwin but is an error on other stacks.
func TestProbeServerEchoesJumboDatagram(t *testing.T) {
	srv := NewProbeServer(0)
	lc := net.ListenConfig{}
	listener, err := lc.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.listener = listener
	srv.running.Store(true)
	go srv.serveUDP()
	defer func() { _ = srv.Close() }()

	dialer := net.Dialer{}
	conn, err := dialer.DialContext(context.Background(), "udp", listener.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	payload := make([]byte, 8972) // 9000 bytes on the wire
	binary.BigEndian.PutUint32(payload, 42)
	if _, err = conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	resp := make([]byte, 16)
	n, err := conn.Read(resp)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 || binary.BigEndian.Uint32(resp[:4]) != 42 {
		t.Fatalf("reply = %v (%d bytes), want the 4-byte seq 42", resp[:n], n)
	}
}
