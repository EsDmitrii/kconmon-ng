package checker

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

/*
Two checkers speak the agent's own protocol once the transport is up, and against a plain host both
asked the wrong question: TCP followed a successful connect with GET /readyz, so an open port was
reported as a failure; UDP ignored the requested port entirely and probed the agent's own echo port,
so every packet went somewhere else and the check reported total loss.
*/

func TestTCPExternalStopsAtTheConnect(t *testing.T) {
	// A listener that accepts and says nothing: no HTTP, like any ordinary port.
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	c := NewTCPChecker(2 * time.Second)

	external := c.Check(context.Background(), Target{PodIP: host, Port: port, External: true})
	if !external.Success {
		t.Errorf("external check of an OPEN port failed: %s", external.Error)
	}

	// A peer is still asked the peer question, and this listener is not one.
	peer := c.Check(context.Background(), Target{PodIP: host, Port: port})
	if peer.Success {
		t.Error("a peer check passed against a listener with no /readyz behind it")
	}
}

func TestUDPExternalProbesTheRequestedPort(t *testing.T) {
	// An echo on a port that is NOT the checker's configured one.
	var lc net.ListenConfig
	pc, err := lc.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer func() { _ = pc.Close() }()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, rerr := pc.ReadFrom(buf)
			if rerr != nil {
				return
			}
			_, _ = pc.WriteTo(buf[:n], addr)
		}
	}()

	host, portStr, _ := net.SplitHostPort(pc.LocalAddr().String())
	port, _ := strconv.Atoi(portStr)
	// Configured with a DIFFERENT port, which is what the peer path would use.
	c := NewUDPChecker(time.Second, 3, port+1)

	external := c.Check(context.Background(), Target{PodIP: host, Port: port, External: true})
	if !external.Success {
		t.Errorf("external UDP check of the port that was asked for failed: %s (%+v)", external.Error, external.Details)
	}
	if external.Type != model.CheckUDP {
		t.Errorf("check type = %s", external.Type)
	}
}
