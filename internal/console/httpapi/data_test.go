package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/httpapi"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/promql"
	"github.com/prometheus/client_golang/prometheus"
)

func newDataServer(t *testing.T, ctrlURL, promURL string) *httpapi.Server {
	t.Helper()
	cfg, err := config.Load("/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	m := metrics.New(cfg.MetricsPrefix, reg)
	var ctrl *controllerclient.Client
	if ctrlURL != "" {
		ctrl = controllerclient.New(ctrlURL, 2*time.Second)
	}
	var prom *promql.Client
	if promURL != "" {
		prom = promql.New(promURL, promql.Guards{QueryTimeout: 2 * time.Second, MaxRange: 24 * time.Hour, MaxResponseBytes: 1 << 20})
	}
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })
	return httpapi.NewServer(httpapi.Deps{
		Config: cfg, Metrics: m, PromRegistry: reg, UI: ui,
		Controller: ctrl, Prometheus: prom,
	})
}

func fakeController(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/topology" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"nodes":[{"name":"n1","zone":"z1","ready":true}],"agents":[],"timestamp":"2026-01-01T00:00:00Z"}`))
	}))
}

func fakePrometheus(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
}

func do(t *testing.T, srv *httpapi.Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, rdr)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestTopologyProxy(t *testing.T) {
	ctrl := fakeController(t)
	defer ctrl.Close()
	rec := do(t, newDataServer(t, ctrl.URL, ""), http.MethodGet, "/api/v1/topology", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var topo controllerclient.Topology
	if err := json.Unmarshal(rec.Body.Bytes(), &topo); err != nil || len(topo.Nodes) != 1 {
		t.Fatalf("bad body: %s (%v)", rec.Body, err)
	}
}

// TestTopologyNullSlicesBecomeEmptyArrays pins the nil-slice fix: a controller
// that answered {"nodes":null,"agents":null} (Go marshals a nil slice as null)
// must reach the console as [], because the frontend indexes into both. An empty
// topology is [], never absent.
func TestTopologyNullSlicesBecomeEmptyArrays(t *testing.T) {
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/topology" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"nodes":null,"agents":null,"timestamp":"2026-01-01T00:00:00Z"}`))
	}))
	defer ctrl.Close()

	rec := do(t, newDataServer(t, ctrl.URL, ""), http.MethodGet, "/api/v1/topology", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, `"nodes":null`) || strings.Contains(body, `"agents":null`) {
		t.Fatalf("nil slice leaked as null: %s", body)
	}
	if !strings.Contains(body, `"nodes":[]`) || !strings.Contains(body, `"agents":[]`) {
		t.Fatalf("empty topology must be []: %s", body)
	}
	// And the decoded shape is a real, non-nil empty slice.
	var topo controllerclient.Topology
	if err := json.Unmarshal(rec.Body.Bytes(), &topo); err != nil {
		t.Fatalf("bad body: %s (%v)", rec.Body, err)
	}
	if topo.Nodes == nil || topo.Agents == nil {
		t.Fatalf("nodes/agents must decode non-nil: %+v", topo)
	}
}

func TestTopologyNotConfigured503(t *testing.T) {
	rec := do(t, newDataServer(t, "", ""), http.MethodGet, "/api/v1/topology", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type: %s", ct)
	}
}

func TestMatrixValidation(t *testing.T) {
	prom := fakePrometheus(t)
	defer prom.Close()
	srv := newDataServer(t, "", prom.URL)

	if rec := do(t, srv, http.MethodGet, "/api/v1/matrix?protocol=http", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("bad protocol: expected 400, got %d", rec.Code)
	}
	if rec := do(t, srv, http.MethodGet, "/api/v1/matrix?plane=host", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("bad plane: expected 400, got %d", rec.Code)
	}
	rec := do(t, srv, http.MethodGet, "/api/v1/matrix", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("default matrix: %d %s", rec.Code, rec.Body)
	}
	var m struct {
		Protocol, Plane string
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m.Protocol != "tcp" || m.Plane != "pod" {
		t.Errorf("defaults: %+v", m)
	}
}

func TestPromQLQuery(t *testing.T) {
	prom := fakePrometheus(t)
	defer prom.Close()
	srv := newDataServer(t, "", prom.URL)

	rec := do(t, srv, http.MethodPost, "/api/v1/promql/query", `{"query":"up"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"success"`) {
		t.Fatalf("query: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, srv, http.MethodPost, "/api/v1/promql/query", `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty query: expected 400, got %d", rec.Code)
	}
}

func TestPromQLQueryRangeGuardsSurface(t *testing.T) {
	prom := fakePrometheus(t)
	defer prom.Close()
	srv := newDataServer(t, "", prom.URL)

	body := `{"query":"up","start":"2026-01-01T00:00:00Z","end":"2026-01-03T00:00:00Z","step":60000000000}`
	if rec := do(t, srv, http.MethodPost, "/api/v1/promql/query_range", body); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("range too large: expected 422, got %d", rec.Code)
	}
	ok := `{"query":"up","start":"2026-01-01T00:00:00Z","end":"2026-01-01T01:00:00Z","step":60000000000}`
	if rec := do(t, srv, http.MethodPost, "/api/v1/promql/query_range", ok); rec.Code != http.StatusOK {
		t.Errorf("valid range: %d %s", rec.Code, rec.Body)
	}
}

func TestPromQLNotConfigured503(t *testing.T) {
	srv := newDataServer(t, "", "")
	if rec := do(t, srv, http.MethodPost, "/api/v1/promql/query", `{"query":"up"}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}
	if rec := do(t, srv, http.MethodGet, "/api/v1/matrix", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("matrix without prometheus: expected 503, got %d", rec.Code)
	}
}
