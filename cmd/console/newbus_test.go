package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
)

// syncBuffer is a bytes.Buffer the logger and the test can share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// refusingRedis answers every command with WRONGPASS, the way a server with a password the DSN does
// not carry answers the connection handshake.
func refusingRedis(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				buf := make([]byte, 4096)
				for {
					n, rerr := c.Read(buf)
					if rerr != nil {
						return
					}
					// One reply per RESP command in the chunk: each starts with an array header.
					commands := strings.Count("\n"+string(buf[:n]), "\n*")
					for range commands {
						if _, werr := c.Write([]byte("-WRONGPASS invalid username-password pair or user is disabled.\r\n")); werr != nil {
							return
						}
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// A server that answered and refused the handshake (credentials, ACL, TLS) is a different fix from
// one nothing answers at, so the fallback says which it was.
func TestNewBusTellsARefusedHandshakeFromAnUnreachableServer(t *testing.T) {
	closed, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unreachable := closed.Addr().String()
	_ = closed.Close()

	for name, c := range map[string]struct{ addr, want, notWant string }{
		"nothing listening": {unreachable, "redis unreachable at startup", "handshake"},
		"wrong password":    {refusingRedis(t), "redis refused the connection handshake", "unreachable"},
	} {
		t.Run(name, func(t *testing.T) {
			var logs syncBuffer
			restore := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(restore) })

			cfg := &config.Config{Redis: config.RedisConfig{DSN: "redis://:not-the-password@" + c.addr, DialTimeout: 2 * time.Second}}
			bus, closeBus, err := newBus(context.Background(), cfg)
			if err != nil {
				t.Fatalf("newBus = %v, want the in-process fallback", err)
			}
			defer closeBus()
			if bus.CrossReplica() {
				t.Fatal("newBus returned a cross-replica bus, want the in-process fallback")
			}
			got := logs.String()
			if !strings.Contains(got, c.want) || strings.Contains(got, c.notWant) {
				t.Errorf("log = %s, want %q and no %q", got, c.want, c.notWant)
			}
			if strings.Contains(got, "not-the-password") {
				t.Errorf("the log carries the DSN's password: %s", got)
			}
		})
	}
}
