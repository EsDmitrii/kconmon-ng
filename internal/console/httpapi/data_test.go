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

/*
TestTopologyProbePlanSurvivesTheProxy pins both halves of the sparse-plan surfacing contract at
the console boundary:

  - a controller running a sparse plan answers with probePlan, and the proxy re-marshals it intact
    (the client struct decodes it; a struct without the field silently dropped it);
  - a full-mesh controller (or one predating the field) answers without it, and the proxied body
    must not grow a probePlan key of any shape — absent means "every pair intended", and the web
    keys the 'not probed' cell state off the field's very presence.
*/
func TestTopologyProbePlanSurvivesTheProxy(t *testing.T) {
	sparse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"nodes":[],"agents":[],"timestamp":"2026-01-01T00:00:00Z",` +
			`"probePlan":{"n1":["n2"],"n2":[]}}`))
	}))
	defer sparse.Close()

	rec := do(t, newDataServer(t, sparse.URL, ""), http.MethodGet, "/api/v1/topology", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var topo controllerclient.Topology
	if err := json.Unmarshal(rec.Body.Bytes(), &topo); err != nil {
		t.Fatalf("bad body: %s (%v)", rec.Body, err)
	}
	if len(topo.ProbePlan) != 2 || len(topo.ProbePlan["n1"]) != 1 || topo.ProbePlan["n1"][0] != "n2" {
		t.Fatalf("probePlan lost or mangled in the proxy: %s", rec.Body)
	}
	if topo.ProbePlan["n2"] == nil {
		t.Fatalf("fail-closed empty list must survive as [], not null: %s", rec.Body)
	}

	full := fakeController(t)
	defer full.Close()
	rec = do(t, newDataServer(t, full.URL, ""), http.MethodGet, "/api/v1/topology", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "probePlan") {
		t.Fatalf("full-mesh proxy body grew a probePlan key: %s", rec.Body)
	}
}

/*
TestTopologyLabelsAndCapabilitiesSurviveTheProxy is the probePlan contract applied to the two
per-agent fields the web needs to tell an external host from a node:

  - an agent that registered with labels and capabilities reaches the browser with both, verbatim
    (the client struct used to have four fields, so the proxy re-marshalled them away);
  - an agent that sent neither must not grow a "labels":null or "capabilities":null key -- absence
    is the pre-2.4.0 shape, and the web's fail-open rule reads absence as "unknown".
*/
func TestTopologyLabelsAndCapabilitiesSurviveTheProxy(t *testing.T) {
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"nodes":[{"name":"n1","zone":"z1","ready":true}],` +
			`"agents":[{"id":"n1-agent","nodeName":"n1","podIP":"10.0.0.1","zone":"z1"},` +
			`{"id":"edge-01-agent","nodeName":"edge-01","podIP":"192.0.2.10","zone":"office",` +
			`"labels":{"kconmon-ng.io/external":"true"},"capabilities":["external-checks","plane:tcp"]}],` +
			`"timestamp":"2026-01-01T00:00:00Z"}`))
	}))
	defer ctrl.Close()

	rec := do(t, newDataServer(t, ctrl.URL, ""), http.MethodGet, "/api/v1/topology", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var topo controllerclient.Topology
	if err := json.Unmarshal(rec.Body.Bytes(), &topo); err != nil {
		t.Fatalf("bad body: %s (%v)", rec.Body, err)
	}
	if len(topo.Agents) != 2 {
		t.Fatalf("agents lost in the proxy: %s", rec.Body)
	}
	ext := topo.Agents[1]
	if ext.Labels["kconmon-ng.io/external"] != "true" {
		t.Errorf("external label dropped by the proxy: %s", rec.Body)
	}
	if len(ext.Capabilities) != 2 || ext.Capabilities[1] != "plane:tcp" {
		t.Errorf("capabilities dropped or reordered by the proxy: %s", rec.Body)
	}
	// The in-cluster agent is re-marshalled without either key, never with a null.
	body := rec.Body.String()
	if strings.Contains(body, `"labels":null`) || strings.Contains(body, `"capabilities":null`) {
		t.Errorf("an agent that sent no labels/capabilities must not grow a null key: %s", body)
	}
	if strings.Count(body, `"labels":`) != 1 || strings.Count(body, `"capabilities":`) != 1 {
		t.Errorf("labels/capabilities must appear exactly once (the external agent's): %s", body)
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
	} else if !strings.Contains(rec.Body.String(), "pmtu") {
		t.Errorf("bad protocol detail must list pmtu among the valid protocols: %s", rec.Body)
	}
	if rec := do(t, srv, http.MethodGet, "/api/v1/matrix?plane=host", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("bad plane: expected 400, got %d", rec.Code)
	} else if detail := problemDetailOf(t, rec); !strings.Contains(detail, `plane must be "pod"`) {
		t.Errorf("bad plane detail = %q, want the pod-only rule the runs and checks routes state", detail)
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

// GET /api/v1/matrix?protocol=pmtu answers the path MTU matrix: the fail ratio plus the measured
// and the probed size for each pair.
func TestMatrixPMTU(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.FormValue("query")
		result := `[]`
		switch {
		case strings.Contains(q, "_pmtu_probe_bytes"):
			result = `[{"metric":{"source_node":"a","destination_node":"b"},"value":[0,"1500"]}]`
		case strings.Contains(q, "_pmtu_bytes"):
			result = `[{"metric":{"source_node":"a","destination_node":"b"},"value":[0,"1400"]}]`
		case strings.Contains(q, "pmtu_results_total"):
			result = `[{"metric":{"source_node":"a","destination_node":"b"},"value":[0,"0.5"]}]`
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":` + result + `}}`))
	}))
	defer prom.Close()
	srv := newDataServer(t, "", prom.URL)

	rec := do(t, srv, http.MethodGet, "/api/v1/matrix?protocol=pmtu", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("pmtu matrix: %d %s", rec.Code, rec.Body)
	}
	var m struct {
		Protocol string
		Cells    []struct {
			Source, Destination string
			FailRatio           *float64
			MTUBytes            *int64 `json:"mtuBytes"`
			ProbeMTUBytes       *int64 `json:"probeMtuBytes"`
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body)
	}
	if m.Protocol != "pmtu" || len(m.Cells) != 1 {
		t.Fatalf("pmtu matrix = %s, want one pmtu cell", rec.Body)
	}
	c := m.Cells[0]
	if c.Source != "a" || c.Destination != "b" || c.FailRatio == nil || *c.FailRatio != 0.5 ||
		c.MTUBytes == nil || *c.MTUBytes != 1400 || c.ProbeMTUBytes == nil || *c.ProbeMTUBytes != 1500 {
		t.Errorf("pmtu cell = %s, want a->b failRatio 0.5, mtuBytes 1400, probeMtuBytes 1500", rec.Body)
	}
}

// With a custom config.metricsPrefix the PromQL proxy forwards the browser's default-prefixed
// metric names under the configured prefix, on both the instant and the range route.
func TestPromQLProxyAppliesTheMetricsPrefix(t *testing.T) {
	var got []string
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.FormValue("query"))
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer prom.Close()
	cfg, err := config.Load("/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	cfg.MetricsPrefix = "netmon"
	reg := prometheus.NewRegistry()
	srv := httpapi.NewServer(httpapi.Deps{
		Config: cfg, Metrics: metrics.New(cfg.MetricsPrefix, reg), PromRegistry: reg,
		UI:         http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) }),
		Prometheus: promql.New(prom.URL, promql.Guards{QueryTimeout: 2 * time.Second, MaxRange: 24 * time.Hour, MaxResponseBytes: 1 << 20}),
	})

	if rec := do(t, srv, http.MethodPost, "/api/v1/promql/query",
		`{"query":"max(kconmon_ng_pmtu_bytes{source_node=\"kconmon_ng_a\"})"}`); rec.Code != http.StatusOK {
		t.Fatalf("query: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, srv, http.MethodPost, "/api/v1/promql/query_range",
		`{"query":"rate(kconmon_ng_tcp_results_total[5m])","start":"2026-01-01T00:00:00Z","end":"2026-01-01T01:00:00Z","step":60000000000}`); rec.Code != http.StatusOK {
		t.Fatalf("query_range: %d %s", rec.Code, rec.Body)
	}
	want := []string{`max(netmon_pmtu_bytes{source_node="kconmon_ng_a"})`, `rate(netmon_tcp_results_total[5m])`}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("forwarded queries = %q, want %q", got, want)
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

// The PromQL bodies are additionalProperties:false like every other request schema: a misspelled
// field is refused by name rather than dropped, which would evaluate the query at a different
// instant or step than the caller asked for.
func TestPromQLBodiesAreStrict(t *testing.T) {
	prom := fakePrometheus(t)
	defer prom.Close()
	srv := newDataServer(t, "", prom.URL)

	const rangeBody = `{"query":"up","start":"2026-01-01T00:00:00Z","end":"2026-01-01T01:00:00Z","step":60000000000}`
	for _, c := range []struct{ path, body, want string }{
		{"/api/v1/promql/query", `{"query":"up","tme":"2001-01-01T00:00:00Z"}`, `unknown field "tme"`},
		{"/api/v1/promql/query", `{"query":"up"}{"query":"down"}`, "more than one JSON value"},
		{"/api/v1/promql/query", `{"query":""}`, `non-empty "query"`},
		{"/api/v1/promql/query_range",
			`{"query":"up","start":"2026-01-01T00:00:00Z","end":"2026-01-01T01:00:00Z","stpe":60000000000}`,
			`unknown field "stpe"`},
		{"/api/v1/promql/query_range", rangeBody + rangeBody, "more than one JSON value"},
	} {
		rec := do(t, srv, http.MethodPost, c.path, c.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s %s = %d, want 400: %s", c.path, c.body, rec.Code, rec.Body)
			continue
		}
		if detail := problemDetailOf(t, rec); !strings.Contains(detail, c.want) {
			t.Errorf("POST %s %s detail = %q, want it to say %q", c.path, c.body, detail, c.want)
		}
	}
}

func problemDetailOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var p struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem body %q: %v", rec.Body, err)
	}
	return p.Detail
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

// Only Prometheus's own API errors are forwarded with their status. An upstream 401/403 (an auth proxy in
// front of the querier) must not reach the SPA as the console's own 401, which it reads as a lost session
// and answers with a redirect to /login.
func TestPromQLUpstreamStatusForwarding(t *testing.T) {
	for _, c := range []struct {
		name       string
		status     int
		body       string
		wantStatus int
		verbatim   bool
	}{
		{"parse error envelope", http.StatusBadRequest, `{"status":"error","errorType":"bad_data","error":"parse error"}`, http.StatusBadRequest, true},
		{"execution error envelope", http.StatusUnprocessableEntity, `{"status":"error","errorType":"execution","error":"many-to-many"}`, http.StatusUnprocessableEntity, true},
		{"timeout envelope", http.StatusServiceUnavailable, `{"status":"error","errorType":"timeout","error":"query timed out"}`, http.StatusServiceUnavailable, true},
		{"auth proxy 401", http.StatusUnauthorized, "Unauthorized\n", http.StatusBadGateway, false},
		{"auth proxy 403", http.StatusForbidden, "Forbidden\n", http.StatusBadGateway, false},
		{"401 dressed as an envelope", http.StatusUnauthorized, `{"status":"error","error":"no token"}`, http.StatusBadGateway, false},
		{"400 without an envelope", http.StatusBadRequest, "<html>bad gateway page</html>", http.StatusBadGateway, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer prom.Close()
			srv := newDataServer(t, "", prom.URL)

			rec := do(t, srv, http.MethodPost, "/api/v1/promql/query", `{"query":"up"}`)
			if rec.Code != c.wantStatus {
				t.Fatalf("upstream %d -> console %d, want %d: %s", c.status, rec.Code, c.wantStatus, rec.Body)
			}
			if c.verbatim && rec.Body.String() != c.body {
				t.Errorf("body = %s, want Prometheus's envelope verbatim", rec.Body)
			}
			if !c.verbatim && rec.Header().Get("Content-Type") != "application/problem+json" {
				t.Errorf("Content-Type = %q, want a problem document", rec.Header().Get("Content-Type"))
			}
		})
	}
}
