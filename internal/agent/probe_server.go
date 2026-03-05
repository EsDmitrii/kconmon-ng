package agent

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"
)

type ProbeServer struct {
	udpPort  int
	listener net.PacketConn
	running  atomic.Bool
}

func NewProbeServer(udpPort int) *ProbeServer {
	return &ProbeServer{
		udpPort: udpPort,
	}
}

func (s *ProbeServer) ListenUDP(ctx context.Context) error {
	lc := net.ListenConfig{}
	conn, err := lc.ListenPacket(ctx, "udp", fmt.Sprintf(":%d", s.udpPort))
	if err != nil {
		return err
	}
	s.listener = conn
	s.running.Store(true)

	go s.serveUDP()
	return nil
}

func (s *ProbeServer) serveUDP() {
	buf := make([]byte, 1024)
	for s.running.Load() {
		n, addr, err := s.listener.ReadFrom(buf)
		if err != nil {
			if s.running.Load() {
				slog.Error("UDP read error", "error", err)
			}
			continue
		}

		if n < 4 {
			continue
		}

		seq := binary.BigEndian.Uint32(buf[:4])
		resp := make([]byte, 4)
		binary.BigEndian.PutUint32(resp, seq)

		if _, err := s.listener.WriteTo(resp, addr); err != nil {
			slog.Error("UDP write error", "error", err, "addr", addr)
		}
	}
}

func (s *ProbeServer) Close() error {
	s.running.Store(false)
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}
