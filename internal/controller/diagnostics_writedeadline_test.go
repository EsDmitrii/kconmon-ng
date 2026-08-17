package controller

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// This file is the regression suite for the bug where an external MTR trace ran to completion on the
// agent and was then thrown away on the way back: the controller's process-wide
// http.Server.WriteTimeout (10s) is shorter than the dispatch timeout POST /api/v1/diagnostics
// negotiates per request (up to maxDiagnosticsTimeout, 120s). Go arms the connection's write
// deadline when it reads the request, so a handler that legitimately takes 30s writes into a
// connection whose deadline expired at 10s: the response never reaches the wire, the connection is
// dropped, and the caller sees a bare EOF with nothing at all in the controller's logs.
//
// The slow-dispatch tests run inside a synctest bubble over in-memory conns, so a 90s dispatch costs
// no wall clock and the PRODUCTION timeout constants are exercised unchanged.

// bubbleListener hands out net.Pipe conns so a full HTTP round trip stays inside a synctest bubble:
// real sockets are not bubbled, in-memory pipes are (and they honour SetWriteDeadline, which is the
// exact mechanism under test).
type bubbleListener struct {
	conns chan net.Conn
	done  chan struct{}
}

func newBubbleListener() *bubbleListener {
	return &bubbleListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *bubbleListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *bubbleListener) Close() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

func (l *bubbleListener) Addr() net.Addr { return pipeAddr{} }

func (l *bubbleListener) dial(ctx context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.done:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// silentMTRResult is the shape the stand produced: a trace that finished with every TTL silent.
func silentMTRResult(hops int) []byte {
	h := make([]model.MTRHop, 0, hops)
	for i := 1; i <= hops; i++ {
		h = append(h, model.MTRHop{Number: i, IP: "", LossRatio: 1})
	}
	body, err := json.Marshal(model.CheckResult{
		Type:        model.CheckMTR,
		Success:     true,
		Source:      "node-a",
		Destination: "google-dns",
		Details:     model.MTRDetails{Hops: h},
	})
	if err != nil {
		panic(err)
	}
	return body
}

// serveDiagnostics wires the REAL route table onto the REAL controller HTTP server and returns a
// client bound to the bubble's in-memory transport.
func serveDiagnostics(t *testing.T, h http.Handler) (*http.Client, func()) {
	t.Helper()
	ln := newBubbleListener()
	srv := newControllerHTTPServer(":0", h)
	go func() { _ = srv.Serve(ln) }()

	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return ln.dial(ctx) },
	}
	// diagHC's shape on the Console side: no client-wide Timeout, the deadline comes from ?timeout=.
	return &http.Client{Transport: tr}, func() {
		_ = srv.Close()
		tr.CloseIdleConnections()
	}
}

func postDiagnostics(t *testing.T, c *http.Client, query, body string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://controller/api/v1/diagnostics"+query, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.Do(req) //nolint:bodyclose // callers close
}

// TestControllerHTTPServerWriteTimeoutCutsASlowHandler pins the BUG at the server layer, with no
// diagnostics handler involved: any handler on this server that answers later than
// controllerHTTPWriteTimeout has its response dropped and the caller gets EOF. This is why the fix
// has to extend the deadline per request rather than trust the server-wide constant.
func TestControllerHTTPServerWriteTimeoutCutsASlowHandler(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		slow := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(controllerHTTPWriteTimeout + 20*time.Second)
			_, _ = w.Write([]byte(`{"success":true}`))
		})
		c, stop := serveDiagnostics(t, slow)
		defer stop()

		resp, err := postDiagnostics(t, c, "?timeout=110", `{}`)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatalf("expected the server write deadline to drop the response, got %d", resp.StatusCode)
		}
		if !strings.Contains(err.Error(), "EOF") {
			t.Fatalf("expected the reported symptom (EOF), got %v", err)
		}
	})
}

// TestDiagnosticsSlowExternalMTRSurvivesServerWriteTimeout is the end-to-end proof: a 90s external
// MTR dispatch (simulated clock, zero wall clock) is delivered verbatim to the caller even though
// the controller's HTTP server still runs with a 10s WriteTimeout.
func TestDiagnosticsSlowExternalMTRSurvivesServerWriteTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const traceDuration = 90 * time.Second

		details := silentMTRResult(30)
		disp := dispatcherFunc(func(ctx context.Context, _ string, _ *pb.TaskRequest) (*pb.TaskResult, error) {
			// The agent's 30 silent TTLs at 1s each; ctx carries the negotiated dispatch deadline.
			select {
			case <-time.After(traceDuration):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &pb.TaskResult{Success: true, DetailsJson: details}, nil
		})

		diag := newDiagTestHandler(t, disp, false, false)
		srv := NewHTTPServer(NewRegistry(30*time.Second), nil, prometheus.NewRegistry(), nil)
		srv.registry.Register(model.AgentInfo{
			ID: "agent-a", NodeName: "node-a", PodIP: "10.0.0.1", Capabilities: []string{capabilityExternalChecks},
		})
		diag.registry = srv.registry
		srv.SetDiagnosticsHandler(diag)

		c, stop := serveDiagnostics(t, srv.Handler())
		defer stop()

		start := time.Now()
		resp, err := postDiagnostics(t, c, "?timeout=110",
			`{"source":"node-a","type":"mtr","destinationKind":"external","destinationAddress":"8.8.8.8","destination":"google-dns"}`)
		if err != nil {
			t.Fatalf("a %v dispatch must be delivered, got transport error: %v", traceDuration, err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		var got model.CheckResult
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("response is not a CheckResult: %v (%s)", err, body)
		}
		if got.Type != model.CheckMTR || !got.Success {
			t.Fatalf("unexpected result: %+v", got)
		}
		if elapsed := time.Since(start); elapsed < traceDuration {
			t.Fatalf("expected the caller to wait out the whole trace, waited %v", elapsed)
		}
		// The result reached the caller, which is the only path that records an MTR snapshot.
		if !strings.Contains(string(body), `"hops"`) {
			t.Fatalf("hop list missing from the delivered body: %s", body)
		}
	})
}

// deadlineRecorder captures what the handler asked of the connection.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	writeDeadline time.Time
	deadlineSet   bool
	flushErr      error
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.writeDeadline = t
	d.deadlineSet = true
	return nil
}

func (d *deadlineRecorder) FlushError() error { return d.flushErr }

// TestDiagnosticsWriteDeadlineCoversTheNegotiatedTimeout is the regression test for the constant
// that was wrong: whatever ?timeout= the caller negotiated (up to the server-side cap), the write
// deadline the handler arms must outlive it -- and must be strictly longer than the server-wide
// controllerHTTPWriteTimeout that used to govern this response.
func TestDiagnosticsWriteDeadlineCoversTheNegotiatedTimeout(t *testing.T) {
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		query string
		want  time.Duration
	}{
		{"default", "", defaultDiagnosticsTimeout},
		{"negotiated 110s", "?timeout=110", 110 * time.Second},
		{"capped at the server maximum", "?timeout=999", maxDiagnosticsTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: []byte(`{}`)}}
			h := newDiagTestHandler(t, disp, false, false)
			h.now = func() time.Time { return base }

			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
				"/api/v1/diagnostics"+tc.query,
				strings.NewReader(`{"source":"node-a","destination":"node-b","type":"mtr"}`))
			w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			h.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			if !w.deadlineSet {
				t.Fatal("handler did not arm its own write deadline: the server-wide WriteTimeout would cut the response")
			}
			if got := w.writeDeadline.Sub(base); got < tc.want {
				t.Fatalf("write deadline %v does not outlive the negotiated %v dispatch", got, tc.want)
			}
			if got := w.writeDeadline.Sub(base); got <= controllerHTTPWriteTimeout {
				t.Fatalf("write deadline %v is still bounded by controllerHTTPWriteTimeout (%v)", got, controllerHTTPWriteTimeout)
			}
		})
	}
}

// TestControllerHTTPWriteTimeoutStaysShortForTheFastPaths guards the other direction: the fix must
// not become "raise every timeout". /metrics, /healthz, /readyz, topology, version and
// external-checks keep the short server-wide write budget.
func TestControllerHTTPWriteTimeoutStaysShortForTheFastPaths(t *testing.T) {
	srv := newControllerHTTPServer(":8080", http.NewServeMux())
	if srv.WriteTimeout != 10*time.Second {
		t.Fatalf("the fast paths' write budget changed: %v", srv.WriteTimeout)
	}
	if srv.ReadTimeout != 10*time.Second {
		t.Fatalf("the read budget changed: %v", srv.ReadTimeout)
	}
	// The relationship that made the bug possible, kept explicit: the server-wide budget is far
	// shorter than a diagnostics dispatch, which is exactly why the handler arms its own deadline.
	if srv.WriteTimeout >= maxDiagnosticsTimeout {
		t.Fatalf("controllerHTTPWriteTimeout (%v) now covers maxDiagnosticsTimeout (%v); the per-request extension test above is no longer meaningful",
			srv.WriteTimeout, maxDiagnosticsTimeout)
	}
}

// TestDiagnosticsUndeliveredResultIsReportedNotSilent covers the second-order failure: if the
// response still cannot reach the caller, a completed trace must not vanish without a trace. It is
// counted as "undelivered" (not "ok") so the loss is visible in metrics and logs.
func TestDiagnosticsUndeliveredResultIsReportedNotSilent(t *testing.T) {
	disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: silentMTRResult(30)}}
	h := newDiagTestHandler(t, disp, false, false)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/v1/diagnostics?timeout=110",
		strings.NewReader(`{"source":"node-a","destination":"node-b","type":"mtr"}`))
	w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder(), flushErr: net.ErrClosed}
	h.ServeHTTP(w, req)

	if got := testutil.ToFloat64(h.metrics.ControllerDiagnostics.WithLabelValues("mtr", "undelivered")); got != 1 {
		t.Fatalf("expected the lost result to be counted as undelivered, got %v", got)
	}
	if got := testutil.ToFloat64(h.metrics.ControllerDiagnostics.WithLabelValues("mtr", "ok")); got != 0 {
		t.Fatalf("a result that never reached the caller must not count as ok, got %v", got)
	}
}

// TestDiagnosticsDeliveredResultCountsOK is the counterpart: a normal delivery still counts "ok".
func TestDiagnosticsDeliveredResultCountsOK(t *testing.T) {
	disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: []byte(`{"success":true}`)}}
	h := newDiagTestHandler(t, disp, false, false)

	w := doDiag(h, `{"source":"node-a","destination":"node-b","type":"mtr"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := testutil.ToFloat64(h.metrics.ControllerDiagnostics.WithLabelValues("mtr", "ok")); got != 1 {
		t.Fatalf("expected ok=1, got %v", got)
	}
}
