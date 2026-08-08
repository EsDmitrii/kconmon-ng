package ws_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// startServer serves hub.ServeWS on a loopback listener.
func startServer(t *testing.T, bus cache.Bus) (*ws.Hub, *metrics.Metrics, *httptest.Server) {
	t.Helper()
	m := metrics.New("kconmon_ng", prometheus.NewRegistry())
	hub := ws.NewHub(bus, m)
	srv := httptest.NewServer(http.HandlerFunc(hub.ServeWS))
	t.Cleanup(srv.Close)
	return hub, m, srv
}

// startAuthorizedServer is startServer's per-connection-authorization
// variant: authorize is applied to every subscribe that arrives on any socket
// this handler serves.
func startAuthorizedServer(t *testing.T, bus cache.Bus, authorize ws.TopicAuthorizer) (*ws.Hub, *httptest.Server) {
	t.Helper()
	hub := ws.NewHub(bus, metrics.New("kconmon_ng", prometheus.NewRegistry()))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.ServeWSAuthorized(w, r, authorize)
	}))
	t.Cleanup(srv.Close)
	return hub, srv
}

// runsReadOnly is the authorizer the whole M3-carry exists for: a custom role
// holding runs:read but NOT events:read. It may watch its own run and nothing
// else.
func runsReadOnly(topic string) error {
	if strings.HasPrefix(topic, "run:") {
		return nil
	}
	return errors.New("this connection may subscribe to run:{id} topics only; " + topic + " needs events:read")
}

// wsURL turns the test server's http URL into a ws one.
func wsURL(srv *httptest.Server) string { return "ws" + strings.TrimPrefix(srv.URL, "http") }

// dial opens a client socket, optionally with extra request headers.
func dial(t *testing.T, srv *httptest.Server, header http.Header) *websocket.Conn {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL(srv), header)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
			_ = resp.Body.Close()
		}
		t.Fatalf("dial: %v (http status %d)", err, status)
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func send(t *testing.T, conn *websocket.Conn, msg ws.ClientMessage) {
	t.Helper()
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("write %+v: %v", msg, err)
	}
}

func readEnvelope(t *testing.T, conn *websocket.Conn) ws.Envelope {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	var env ws.Envelope
	if err := conn.ReadJSON(&env); err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	return env
}

// waitForClients polls until the hub reports want clients.
func waitForClients(t *testing.T, hub *ws.Hub, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if hub.ClientCount() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("ClientCount = %d after 5s, want %d", hub.ClientCount(), want)
}

func TestServeWSUpgradesAndDeliversBroadcasts(t *testing.T) {
	hub, m, srv := startServer(t, cache.NewInProcessBus())
	conn := dial(t, srv, nil)
	waitForClients(t, hub, 1)

	send(t, conn, ws.ClientMessage{Action: ws.ActionSubscribe, Topic: ws.TopicTopology})
	// The subscribe is processed asynchronously by the read pump, so keep
	// broadcasting until the first frame lands rather than sleeping on a guess.
	deadline := time.Now().Add(5 * time.Second)
	var env ws.Envelope
	for {
		hub.Broadcast(ws.TopicTopology, ws.TypeSnapshot, json.RawMessage(`{"nodes":[]}`))
		if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		err := conn.ReadJSON(&env)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no snapshot arrived within 5s: %v", err)
		}
	}

	if env.Topic != ws.TopicTopology || env.Type != ws.TypeSnapshot {
		t.Errorf("envelope = %+v, want topic=topology type=snapshot", env)
	}
	if env.Seq == 0 {
		t.Error("envelope seq = 0, want a hub-assigned sequence starting at 1")
	}
	if string(env.Data) != `{"nodes":[]}` {
		t.Errorf("envelope data = %s, want the broadcast payload verbatim", env.Data)
	}
	if got := testutil.ToFloat64(m.WSClients.WithLabelValues()); got != 1 {
		t.Errorf("ws_clients = %v, want 1", got)
	}
}

// A subscriber must get the current snapshot at once rather than waiting up to a
// full push interval: the ring's single entry is replayed on subscribe.
func TestServeWSReplaysTheCurrentSnapshotOnSubscribe(t *testing.T) {
	hub, _, srv := startServer(t, cache.NewInProcessBus())
	hub.Broadcast(ws.MatrixTopic("tcp"), ws.TypeSnapshot, json.RawMessage(`{"protocol":"tcp"}`))

	conn := dial(t, srv, nil)
	send(t, conn, ws.ClientMessage{Action: ws.ActionSubscribe, Topic: ws.MatrixTopic("tcp")})

	env := readEnvelope(t, conn)
	if env.Topic != ws.MatrixTopic("tcp") || string(env.Data) != `{"protocol":"tcp"}` {
		t.Errorf("envelope = %+v, want the replayed tcp matrix snapshot", env)
	}
}

func TestServeWSUnknownTopicGetsAnErrorFrameOverTheWire(t *testing.T) {
	_, _, srv := startServer(t, cache.NewInProcessBus())
	conn := dial(t, srv, nil)

	send(t, conn, ws.ClientMessage{Action: ws.ActionSubscribe, Topic: "run:42"})

	env := readEnvelope(t, conn)
	if env.Type != ws.TypeError {
		t.Errorf("envelope type = %q, want %q", env.Type, ws.TypeError)
	}
	if env.Topic != "run:42" {
		t.Errorf("envelope topic = %q, want the requested topic echoed back", env.Topic)
	}
}

// TestServeWSAuthorizedDeniesTopicsTheConnectionMayNotRead is the wire-level
// half of the M3-carry fix (SECURITY.md §10.2's "splitting the two properly
// means teaching the hub subject-aware subscribe authorization"). A connection
// opened by a subject holding runs:read but not events:read may watch its own
// run and must be refused the fleet-wide topics — with an error frame, not
// silence, and with the topic genuinely NOT subscribed, which the second half
// of this test is what proves.
func TestServeWSAuthorizedDeniesTopicsTheConnectionMayNotRead(t *testing.T) {
	hub, srv := startAuthorizedServer(t, cache.NewInProcessBus(), runsReadOnly)
	const runTopic = "run:42"
	if !hub.OpenTopic(context.Background(), runTopic) {
		t.Fatal("OpenTopic(run:42) = false")
	}
	conn := dial(t, srv, nil)
	waitForClients(t, hub, 1)

	send(t, conn, ws.ClientMessage{Action: ws.ActionSubscribe, Topic: ws.TopicLive})
	env := readEnvelope(t, conn)
	if env.Type != ws.TypeError {
		t.Fatalf("subscribe to live on a runs:read-only connection: envelope = %+v, want an error frame", env)
	}
	if env.Topic != ws.TopicLive {
		t.Errorf("error frame topic = %q, want the requested topic echoed back", env.Topic)
	}
	if !strings.Contains(string(env.Data), "events:read") {
		t.Errorf("error frame data = %s, want the authorizer's own detail (naming the missing permission)", env.Data)
	}

	// The refusal must be a refusal, not a warning: a subsequent broadcast on
	// the denied topic must not reach this client at all. If the denied
	// subscribe had still set c.topics[live], this live frame would arrive
	// BEFORE the run frame below and the read would fail the assertion.
	hub.Broadcast(ws.TopicLive, ws.TypeEvent, json.RawMessage(`{"leaked":true}`))

	send(t, conn, ws.ClientMessage{Action: ws.ActionSubscribe, Topic: runTopic})
	deadline := time.Now().Add(5 * time.Second)
	for {
		hub.Broadcast(runTopic, ws.TypeEvent, json.RawMessage(`{"progress":1}`))
		if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		var got ws.Envelope
		if err := conn.ReadJSON(&got); err != nil {
			if time.Now().After(deadline) {
				t.Fatalf("no run:42 frame arrived within 5s: %v", err)
			}
			continue
		}
		if got.Topic == ws.TopicLive {
			t.Fatalf("a live frame reached a connection whose subscribe to live was denied: %+v", got)
		}
		if got.Topic != runTopic {
			t.Fatalf("envelope = %+v, want a run:42 frame", got)
		}
		return
	}
}

// TestServeWSAuthorizedIsPerConnectionNotPerHub is what makes the fix honest:
// the two sockets below are served by ONE hub, and the same subscribe gets
// opposite answers on each. A hub-level (or handler-level) gate could not
// produce this, and a hub-level gate is exactly what SECURITY.md §10.2
// documented as the reason /ws could not be lowered to runs:read.
func TestServeWSAuthorizedIsPerConnectionNotPerHub(t *testing.T) {
	m := metrics.New("kconmon_ng", prometheus.NewRegistry())
	hub := ws.NewHub(cache.NewInProcessBus(), m)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// One header decides which authorizer this connection gets, standing
		// in for the two different subjects httpapi resolves per request.
		if r.Header.Get("X-Test-Role") == "runs-only" {
			hub.ServeWSAuthorized(w, r, runsReadOnly)
			return
		}
		hub.ServeWSAuthorized(w, r, nil)
	}))
	t.Cleanup(srv.Close)

	restricted := dial(t, srv, http.Header{"X-Test-Role": []string{"runs-only"}})
	full := dial(t, srv, nil)
	waitForClients(t, hub, 2)

	send(t, restricted, ws.ClientMessage{Action: ws.ActionSubscribe, Topic: ws.TopicTopology})
	if env := readEnvelope(t, restricted); env.Type != ws.TypeError {
		t.Errorf("restricted connection: envelope = %+v, want an error frame for topology", env)
	}

	hub.Broadcast(ws.TopicTopology, ws.TypeSnapshot, json.RawMessage(`{"nodes":[]}`))
	send(t, full, ws.ClientMessage{Action: ws.ActionSubscribe, Topic: ws.TopicTopology})
	if env := readEnvelope(t, full); env.Type != ws.TypeSnapshot || env.Topic != ws.TopicTopology {
		t.Errorf("unrestricted connection on the SAME hub: envelope = %+v, want the topology snapshot", env)
	}
}

// TestServeWSNilAuthorizerAllowsEverySubscribableTopic pins the back-compat
// half of the seam: plain ServeWS (and ServeWSAuthorized with a nil
// authorizer) keeps the pre-M7 behaviour, where the route's single permission
// decided the whole socket. Every other test in this file relies on it.
func TestServeWSNilAuthorizerAllowsEverySubscribableTopic(t *testing.T) {
	hub, srv := startAuthorizedServer(t, cache.NewInProcessBus(), nil)
	conn := dial(t, srv, nil)
	waitForClients(t, hub, 1)

	hub.Broadcast(ws.TopicTopology, ws.TypeSnapshot, json.RawMessage(`{"nodes":[]}`))
	send(t, conn, ws.ClientMessage{Action: ws.ActionSubscribe, Topic: ws.TopicTopology})
	if env := readEnvelope(t, conn); env.Type != ws.TypeSnapshot {
		t.Errorf("nil authorizer: envelope = %+v, want the topology snapshot", env)
	}
}

// A malformed frame is answered and the socket keeps working: the socket carries
// every topic, so one bad frame must not cost the browser the rest.
func TestServeWSMalformedFrameKeepsTheConnection(t *testing.T) {
	hub, _, srv := startServer(t, cache.NewInProcessBus())
	conn := dial(t, srv, nil)
	waitForClients(t, hub, 1)

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{not json`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	env := readEnvelope(t, conn)
	if env.Type != ws.TypeError {
		t.Errorf("envelope type = %q, want %q", env.Type, ws.TypeError)
	}

	// Still usable afterwards.
	send(t, conn, ws.ClientMessage{Action: ws.ActionSubscribe, Topic: ws.TopicTopology})
	hub.Broadcast(ws.TopicTopology, ws.TypeSnapshot, json.RawMessage(`{}`))
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		var got ws.Envelope
		if err := conn.ReadJSON(&got); err == nil {
			if got.Type != ws.TypeSnapshot {
				t.Errorf("envelope type = %q, want %q", got.Type, ws.TypeSnapshot)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the connection stopped working after a malformed frame")
		}
		hub.Broadcast(ws.TopicTopology, ws.TypeSnapshot, json.RawMessage(`{}`))
	}
}

// The read limit is a real limit, not a comment: an oversized frame ends the
// connection with close code 1009 instead of being allocated.
func TestServeWSClosesOnOversizedFrame(t *testing.T) {
	hub, _, srv := startServer(t, cache.NewInProcessBus())
	conn := dial(t, srv, nil)
	waitForClients(t, hub, 1)

	oversized := make([]byte, 8<<10)
	for i := range oversized {
		oversized[i] = 'x'
	}
	if err := conn.WriteMessage(websocket.TextMessage, oversized); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, _, err := conn.ReadMessage()
	if err == nil {
		t.Fatal("expected the connection to be closed after an oversized frame")
	}
	if !websocket.IsCloseError(err, websocket.CloseMessageTooBig) {
		t.Errorf("close error = %v, want close code %d (CloseMessageTooBig)", err, websocket.CloseMessageTooBig)
	}
	waitForClients(t, hub, 0)
}

// The read pump handles control frames, so a client ping is answered with a
// pong. The server's own 30s ping cadence is a constant (pingPeriod) rather than
// something worth a 30-second test.
func TestServeWSAnswersClientPingWithPong(t *testing.T) {
	_, _, srv := startServer(t, cache.NewInProcessBus())
	conn := dial(t, srv, nil)

	pongs := make(chan struct{}, 1)
	conn.SetPongHandler(func(string) error {
		select {
		case pongs <- struct{}{}:
		default:
		}
		return nil
	})
	if err := conn.WriteControl(websocket.PingMessage, []byte("probe"), time.Now().Add(5*time.Second)); err != nil {
		t.Fatalf("write ping: %v", err)
	}

	// Pongs are delivered through the read loop, so something must be reading.
	go func() {
		for {
			if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return
			}
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	select {
	case <-pongs:
	case <-time.After(5 * time.Second):
		t.Fatal("no pong within 5s")
	}
}

func TestServeWSUnregistersWhenTheClientDisconnects(t *testing.T) {
	hub, m, srv := startServer(t, cache.NewInProcessBus())
	conn := dial(t, srv, nil)
	waitForClients(t, hub, 1)

	if err := conn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye")); err != nil {
		t.Fatalf("write close: %v", err)
	}
	_ = conn.Close()

	waitForClients(t, hub, 0)
	if got := testutil.ToFloat64(m.WSClients.WithLabelValues()); got != 0 {
		t.Errorf("ws_clients = %v, want 0", got)
	}
}

// Cancelling the hub's context must release live sockets: http.Server.Shutdown
// does not track hijacked connections, so this is the only thing that does.
func TestHubRunClosesLiveSocketsOnShutdown(t *testing.T) {
	hub, _, srv := startServer(t, cache.NewInProcessBus())

	ctx, cancel := context.WithCancel(context.Background())
	hubDone := make(chan struct{})
	go func() { defer close(hubDone); hub.Run(ctx) }()

	conn := dial(t, srv, nil)
	waitForClients(t, hub, 1)

	cancel()
	select {
	case <-hubDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("the socket was still readable after the hub shut down")
	}
}

// An upgrade that lands after the hub has already shut down must be torn down
// promptly rather than hanging: register refuses the client with an
// already-closed done channel, and the pumps have to notice that from the start.
// Without it a hijacked connection would be leaked for the process's lifetime,
// since nothing runs closeAllClients a second time.
func TestServeWSTerminatesWhenTheHubIsAlreadyStopped(t *testing.T) {
	hub, _, srv := startServer(t, cache.NewInProcessBus())

	ctx, cancel := context.WithCancel(context.Background())
	hubDone := make(chan struct{})
	go func() { defer close(hubDone); hub.Run(ctx) }()
	cancel()
	select {
	case <-hubDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	// The upgrade itself still succeeds — the refusal happens at register.
	conn := dial(t, srv, nil)

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, _, err := conn.ReadMessage()
	if err == nil {
		t.Fatal("a socket upgraded on a stopped hub stayed readable")
	}
	// The read must fail because the SERVER tore the socket down. Accepting any
	// error here would let the client's own deadline expiring count as a pass,
	// which is exactly the bug this test exists to catch.
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read timed out instead of being closed by the server: %v", err)
	}
	if hub.ClientCount() != 0 {
		t.Errorf("ClientCount = %d, want 0 — a refused client must not be registered", hub.ClientCount())
	}
}

func TestCheckOriginAllowsAbsentOrigin(t *testing.T) {
	hub, _, srv := startServer(t, cache.NewInProcessBus())
	// websocket.DefaultDialer does not add an Origin header, so this is the
	// non-browser (websocat, probes, these tests) path.
	_ = dial(t, srv, nil)
	waitForClients(t, hub, 1)
}

func TestCheckOriginAllowsSameOrigin(t *testing.T) {
	hub, _, srv := startServer(t, cache.NewInProcessBus())
	_ = dial(t, srv, http.Header{"Origin": []string{srv.URL}})
	waitForClients(t, hub, 1)
}

func TestCheckOriginRejectsCrossOrigin(t *testing.T) {
	hub, _, srv := startServer(t, cache.NewInProcessBus())

	conn, resp, err := websocket.DefaultDialer.Dial(wsURL(srv), http.Header{
		"Origin": []string{"http://evil.example"},
	})
	if err == nil {
		_ = conn.Close()
		t.Fatal("a cross-origin upgrade was accepted")
	}
	if resp == nil {
		t.Fatalf("no HTTP response for the rejected upgrade: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	if hub.ClientCount() != 0 {
		t.Errorf("ClientCount = %d, want 0 — a rejected upgrade must not register a client", hub.ClientCount())
	}
}

func TestServeWSRejectsPlainHTTPRequest(t *testing.T) {
	hub, _, srv := startServer(t, cache.NewInProcessBus())

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d for a non-WebSocket request", resp.StatusCode, http.StatusBadRequest)
	}
	if hub.ClientCount() != 0 {
		t.Errorf("ClientCount = %d, want 0", hub.ClientCount())
	}
}
