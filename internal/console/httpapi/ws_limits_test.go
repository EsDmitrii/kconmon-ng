package httpapi

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
)

func startWSLimitServer(t *testing.T, authr authn.Authenticator, limits config.WebSocketConfig) (*ws.Hub, *httptest.Server) {
	t.Helper()
	hub := ws.NewHub(cache.NewInProcessBus(), metrics.New("kconmon_ng", prometheus.NewRegistry()))
	policy := authz.NewPolicy(map[string][]authz.Permission{"tester": {authz.PermEventsRead}})
	s := newAuthzServer(t, authr, policy, Deps{Hub: hub, Roles: fakeRoleResolver{roles: []string{"tester"}}})
	s.cfg.WebSocket = limits
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return hub, srv
}

// dialWS opens /ws and reports whether the hub kept it: a refused socket gets its close frame first.
func dialWS(t *testing.T, srv *httptest.Server) (kept bool, closeErr *websocket.CloseError) {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("dial /ws: %v", err)
	}
	_ = resp.Body.Close()
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	_, _, err = conn.ReadMessage()
	if ce, ok := errors.AsType[*websocket.CloseError](err); ok {
		return false, ce
	}
	return true, nil
}

func waitHubClients(t *testing.T, hub *ws.Hub, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for hub.ClientCount() != want {
		if time.Now().After(deadline) {
			t.Fatalf("hub has %d clients, want %d", hub.ClientCount(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

/*
Under the chart's anonymous default anybody who could reach the console could open sockets until
the pod was OOM-killed. Anonymous callers are one subject, so they are held per address.
*/
func TestWSAnonymousSocketsAreCappedPerAddress(t *testing.T) {
	hub, srv := startWSLimitServer(t, authn.NewAnonymous("tester"),
		config.WebSocketConfig{MaxConnectionsPerAddress: 2, MaxConnectionsPerSubject: 1})

	for i := range 2 {
		if kept, ce := dialWS(t, srv); !kept {
			t.Fatalf("socket %d refused under the address cap: %v", i+1, ce)
		}
	}
	waitHubClients(t, hub, 2)
	kept, ce := dialWS(t, srv)
	if kept {
		t.Fatal("a third socket from the same address was admitted past maxConnectionsPerAddress=2")
	}
	if ce.Code != websocket.CloseTryAgainLater || !strings.Contains(ce.Text, "address") {
		t.Errorf("refusal = %d %q, want 1013 naming the address cap", ce.Code, ce.Text)
	}
	waitHubClients(t, hub, 2)
}

func TestWSSocketsAreCappedPerSubject(t *testing.T) {
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}, mode: "local"}
	hub, srv := startWSLimitServer(t, authr,
		config.WebSocketConfig{MaxConnectionsPerAddress: 10, MaxConnectionsPerSubject: 1})

	if kept, ce := dialWS(t, srv); !kept {
		t.Fatalf("the first socket was refused: %v", ce)
	}
	waitHubClients(t, hub, 1)
	kept, ce := dialWS(t, srv)
	if kept {
		t.Fatal("a second socket of the same user was admitted past maxConnectionsPerSubject=1")
	}
	if ce.Code != websocket.CloseTryAgainLater || !strings.Contains(ce.Text, "user or token") {
		t.Errorf("refusal = %d %q, want 1013 naming the subject cap", ce.Code, ce.Text)
	}
}

func TestWSSocketsAreCappedPerReplica(t *testing.T) {
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}, mode: "local"}
	hub, srv := startWSLimitServer(t, authr, config.WebSocketConfig{MaxConnections: 1})

	if kept, ce := dialWS(t, srv); !kept {
		t.Fatalf("the first socket was refused: %v", ce)
	}
	waitHubClients(t, hub, 1)
	if kept, ce := dialWS(t, srv); kept || ce.Code != websocket.CloseTryAgainLater {
		t.Fatalf("second socket kept=%v close=%v, want a 1013 refusal from maxConnections=1", kept, ce)
	}
}
