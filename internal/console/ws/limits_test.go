package ws_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// startLimitedServer serves every socket with the limits limitsFor picks for its request.
func startLimitedServer(t *testing.T, limitsFor func(*http.Request) []ws.ConnLimit) (*ws.Hub, *httptest.Server, *metrics.Metrics) {
	t.Helper()
	m := metrics.New("kconmon_ng", prometheus.NewRegistry())
	hub := ws.NewHub(cache.NewInProcessBus(), m)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.ServeWSWithOptions(w, r, ws.ConnOptions{Limits: limitsFor(r)})
	}))
	t.Cleanup(srv.Close)
	return hub, srv, m
}

// refusedBy reads the refusal counter for one limit name.
func refusedBy(m *metrics.Metrics, limit string) float64 {
	return testutil.ToFloat64(m.WSRefused.WithLabelValues(limit))
}

// expectRefused reads until the server's close frame and checks it is the limit refusal.
func expectRefused(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, _, err := conn.ReadMessage()
	ce, ok := errors.AsType[*websocket.CloseError](err)
	if !ok {
		t.Fatalf("read on a socket past the limit: %v, want a close frame", err)
	}
	if ce.Code != websocket.CloseTryAgainLater {
		t.Errorf("close code = %d (%q), want %d (try again later)", ce.Code, ce.Text, websocket.CloseTryAgainLater)
	}
	if ce.Text == "" {
		t.Error("the close frame names no reason")
	}
}

// Past a cap the socket is refused with 1013, counted under the cap's name, and the admitted ones
// stay untouched.
func TestServeWSRefusesSocketsPastTheLimit(t *testing.T) {
	limits := []ws.ConnLimit{{Name: ws.LimitAddress, Key: "addr:192.0.2.1", Max: 2, Reason: "too many connections from this address"}}
	hub, srv, m := startLimitedServer(t, func(*http.Request) []ws.ConnLimit { return limits })

	first := dial(t, srv, nil)
	dial(t, srv, nil)
	waitForClients(t, hub, 2)

	expectRefused(t, dial(t, srv, nil))
	if got := hub.ClientCount(); got != 2 {
		t.Errorf("ClientCount = %d after a refusal, want the 2 admitted sockets", got)
	}
	if got := refusedBy(m, ws.LimitAddress); got != 1 {
		t.Errorf("refused{limit=address} = %v, want 1", got)
	}
	for _, other := range []string{ws.LimitTotal, ws.LimitSubject} {
		if got := refusedBy(m, other); got != 0 {
			t.Errorf("refused{limit=%s} = %v, want 0", other, got)
		}
	}

	// Closing an admitted socket frees its slot.
	_ = first.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"))
	_ = first.Close()
	waitForClients(t, hub, 1)
	dial(t, srv, nil)
	waitForClients(t, hub, 2)
}

// Each key is counted on its own, and every limit a socket names has to have room.
func TestServeWSLimitsAreCountedPerKey(t *testing.T) {
	hub, srv, m := startLimitedServer(t, func(r *http.Request) []ws.ConnLimit {
		return []ws.ConnLimit{
			{Name: ws.LimitSubject, Key: "subject:" + r.URL.Query().Get("who"), Max: 1, Reason: "subject"},
			{Name: ws.LimitTotal, Key: "total", Max: 3, Reason: "total"},
		}
	})
	dialAs := func(who string) *websocket.Conn {
		t.Helper()
		conn, resp, err := websocket.DefaultDialer.Dial(wsURL(srv)+"?who="+who, nil)
		if err != nil {
			t.Fatalf("dial as %s: %v", who, err)
		}
		_ = resp.Body.Close()
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}

	dialAs("a")
	dialAs("b")
	waitForClients(t, hub, 2)
	expectRefused(t, dialAs("a"))
	dialAs("c")
	waitForClients(t, hub, 3)
	expectRefused(t, dialAs("d"))
	if got := hub.ClientCount(); got != 3 {
		t.Errorf("ClientCount = %d, want 3", got)
	}
	if s, tot := refusedBy(m, ws.LimitSubject), refusedBy(m, ws.LimitTotal); s != 1 || tot != 1 {
		t.Errorf("refused{subject, total} = %v, %v, want 1, 1", s, tot)
	}
}

// Max <= 0 is "no cap", so an unset limit never refuses anybody.
func TestServeWSZeroLimitIsNoCap(t *testing.T) {
	hub, srv, m := startLimitedServer(t, func(*http.Request) []ws.ConnLimit {
		return []ws.ConnLimit{{Name: ws.LimitAddress, Key: "addr:x", Max: 0, Reason: "never"}}
	})
	for range 5 {
		dial(t, srv, nil)
	}
	waitForClients(t, hub, 5)
	if got := refusedBy(m, ws.LimitAddress); got != 0 {
		t.Errorf("a zero limit refused %v sockets", got)
	}
}
