package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/promql"
	"github.com/prometheus/client_golang/prometheus"
)

/*
GET /api/v1/matrix runs two or three fleet-wide instant queries per call, and viewer (the anonymous
default) holds matrix:read. A client looping on it used to put every one of those queries on
Prometheus, around the budget that guards /api/v1/promql.
*/

// matrixProm is a fake Prometheus that counts matrix computations by their fail-ratio query (one per
// matrix.Compute) and can hold the first query until release is closed.
type matrixProm struct {
	computations atomic.Int64
	failNext     atomic.Bool
	hold         chan struct{}
	release      chan struct{}
	holdOnce     sync.Once
	releaseOnce  sync.Once
}

func newMatrixProm() *matrixProm {
	return &matrixProm{hold: make(chan struct{}), release: make(chan struct{})}
}

func (p *matrixProm) releaseAll() { p.releaseOnce.Do(func() { close(p.release) }) }

// startMatrixProm serves prom and, on a failed test too, releases its parked handlers before the
// server's Close waits for them (cleanups run last-registered first).
func startMatrixProm(t *testing.T, prom *matrixProm) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(prom)
	t.Cleanup(srv.Close)
	t.Cleanup(prom.releaseAll)
	return srv
}

// matrixWait bounds every wait in these tests, so a changed behaviour fails instead of hanging the
// package until go test's own timeout.
const matrixWait = 10 * time.Second

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(matrixWait):
		t.Fatalf("timed out after %v waiting for %s", matrixWait, what)
	}
}

func waitCode(t *testing.T, ch <-chan int, what string) int {
	t.Helper()
	select {
	case code := <-ch:
		return code
	case <-time.After(matrixWait):
		t.Fatalf("timed out after %v waiting for %s", matrixWait, what)
		return 0
	}
}

func (p *matrixProm) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.FormValue("query"), "_results_total") {
		p.computations.Add(1)
		p.holdOnce.Do(func() { close(p.hold) })
		<-p.release
	}
	if p.failNext.CompareAndSwap(true, false) {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
}

func newMatrixServer(t *testing.T, promURL string) *Server {
	t.Helper()
	cfg := &config.Config{HTTPPort: 8080, LogLevel: "info", LogFormat: "json", MetricsPrefix: "kconmon_ng", Auth: config.AuthConfig{Mode: "anonymous", Anonymous: config.AnonymousConfig{Role: "viewer"}}}
	reg := prometheus.NewRegistry()
	return NewServer(Deps{
		Config: cfg, Metrics: metrics.New(cfg.MetricsPrefix, reg), PromRegistry: reg,
		UI:         http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) }),
		Prometheus: promql.New(promURL, promql.Guards{QueryTimeout: 5 * time.Second, MaxRange: 24 * time.Hour, MaxResponseBytes: 1 << 20}),
	})
}

func getMatrix(ctx context.Context, t *testing.T, s *Server, protocol string) int {
	t.Helper()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/matrix?protocol="+protocol, http.NoBody)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w.Code
}

func TestMatrixConcurrentCallersShareOneComputation(t *testing.T) {
	prom := newMatrixProm()
	promSrv := startMatrixProm(t, prom)
	s := newMatrixServer(t, promSrv.URL)

	const callers = 8
	codes := make(chan int, callers)
	for range callers {
		go func() { codes <- getMatrix(context.Background(), t, s, "tcp") }()
	}
	waitClosed(t, prom.hold, "the first fail-ratio query to reach Prometheus")
	// Let every caller reach the handler while the first computation is held.
	time.Sleep(200 * time.Millisecond)
	prom.releaseAll()

	for i := range callers {
		if code := waitCode(t, codes, fmt.Sprintf("caller %d of %d", i+1, callers)); code != http.StatusOK {
			t.Errorf("GET /api/v1/matrix = %d, want 200", code)
		}
	}
	if got := prom.computations.Load(); got != 1 {
		t.Errorf("%d concurrent GET /api/v1/matrix ran %d matrix computations against Prometheus, want 1", callers, got)
	}
}

func TestMatrixIsReusedWithinTheTTLPerProtocol(t *testing.T) {
	prom := newMatrixProm()
	prom.releaseAll()
	promSrv := startMatrixProm(t, prom)
	s := newMatrixServer(t, promSrv.URL)

	for range 3 {
		if code := getMatrix(context.Background(), t, s, "tcp"); code != http.StatusOK {
			t.Fatalf("GET /api/v1/matrix?protocol=tcp = %d, want 200", code)
		}
	}
	if got := prom.computations.Load(); got != 1 {
		t.Errorf("three back-to-back tcp matrix reads ran %d computations, want 1", got)
	}
	if code := getMatrix(context.Background(), t, s, "udp"); code != http.StatusOK {
		t.Fatalf("GET /api/v1/matrix?protocol=udp = %d, want 200", code)
	}
	if got := prom.computations.Load(); got != 2 {
		t.Errorf("a udp read after a cached tcp one ran %d computations in total, want 2 (one per protocol)", got)
	}
}

func TestMatrixIsRecomputedOnceTheTTLRunsOut(t *testing.T) {
	prom := newMatrixProm()
	prom.releaseAll()
	promSrv := startMatrixProm(t, prom)
	s := newMatrixServer(t, promSrv.URL)

	if code := getMatrix(context.Background(), t, s, "tcp"); code != http.StatusOK {
		t.Fatalf("GET /api/v1/matrix = %d, want 200", code)
	}
	s.matrixMu.Lock()
	e := s.matrixCache["tcp"]
	e.at = e.at.Add(-matrixCacheTTL)
	s.matrixCache["tcp"] = e
	s.matrixMu.Unlock()

	if code := getMatrix(context.Background(), t, s, "tcp"); code != http.StatusOK {
		t.Fatalf("GET /api/v1/matrix = %d, want 200", code)
	}
	if got := prom.computations.Load(); got != 2 {
		t.Errorf("a read after the TTL ran %d computations in total, want 2", got)
	}
}

// A Prometheus failure is not remembered: the next read asks again rather than serving a 502 for
// the rest of the TTL.
func TestMatrixFailureIsNotCached(t *testing.T) {
	prom := newMatrixProm()
	prom.releaseAll()
	prom.failNext.Store(true)
	promSrv := startMatrixProm(t, prom)
	s := newMatrixServer(t, promSrv.URL)

	if code := getMatrix(context.Background(), t, s, "tcp"); code != http.StatusBadGateway {
		t.Fatalf("GET /api/v1/matrix with Prometheus failing = %d, want 502", code)
	}
	if code := getMatrix(context.Background(), t, s, "tcp"); code != http.StatusOK {
		t.Fatalf("GET /api/v1/matrix after Prometheus recovered = %d, want 200", code)
	}
}

// The caller that started the shared computation going away must not fail the callers waiting on it.
func TestMatrixFirstCallerCancellingDoesNotFailTheOthers(t *testing.T) {
	prom := newMatrixProm()
	promSrv := startMatrixProm(t, prom)
	s := newMatrixServer(t, promSrv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan int, 1)
	go func() { first <- getMatrix(ctx, t, s, "tcp") }()
	waitClosed(t, prom.hold, "the first fail-ratio query to reach Prometheus")

	second := make(chan int, 1)
	go func() { second <- getMatrix(context.Background(), t, s, "tcp") }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	waitCode(t, first, "the cancelled caller to return")
	prom.releaseAll()

	if code := waitCode(t, second, "the caller sharing the flight"); code != http.StatusOK {
		t.Errorf("a caller sharing the flight of a cancelled one got %d, want 200", code)
	}
	if got := prom.computations.Load(); got != 1 {
		t.Errorf("ran %d computations, want 1", got)
	}
}
