package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{HTTPPort: 8080, LogLevel: "info", LogFormat: "json", MetricsPrefix: "kconmon_ng", Auth: config.AuthConfig{Mode: "anonymous", Anonymous: config.AnonymousConfig{Role: "viewer"}}}
	reg := prometheus.NewRegistry()
	m := metrics.New(cfg.MetricsPrefix, reg)
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<div id="root"></div>`))
	})
	return NewServer(Deps{Config: cfg, Metrics: m, PromRegistry: reg, UI: ui})
}

func do(t *testing.T, s *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, http.NoBody)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestHealthz(t *testing.T) {
	w := do(t, newTestServer(t), "/healthz")
	if w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Fatalf("healthz = %d %q", w.Code, w.Body.String())
	}
}

func TestReadyzTogglesWithSetReady(t *testing.T) {
	s := newTestServer(t)
	if w := do(t, s, "/readyz"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz before ready = %d, want 503", w.Code)
	}
	s.SetReady(true)
	if w := do(t, s, "/readyz"); w.Code != http.StatusOK {
		t.Fatalf("readyz after ready = %d, want 200", w.Code)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	s := newTestServer(t)
	_ = do(t, s, "/healthz") // generate one request to record
	w := do(t, s, "/metrics")
	if w.Code != http.StatusOK {
		t.Fatalf("metrics = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "kconmon_ng_console_http_requests_total") {
		t.Errorf("metrics output missing console http counter")
	}
}

func TestVersionEndpoint(t *testing.T) {
	w := do(t, newTestServer(t), "/api/v1/version")
	if w.Code != http.StatusOK {
		t.Fatalf("version = %d", w.Code)
	}
	var body struct {
		Version      string   `json:"version"`
		Commit       string   `json:"commit"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("version json: %v", err)
	}
	if body.Capabilities == nil {
		t.Errorf("capabilities must be present (empty array in M0)")
	}
}

// configBody is GET /api/v1/config's shape, as consumed by the tests below.
type configBody struct {
	Auth struct {
		Mode      string `json:"mode"`
		Role      string `json:"role"`
		LoginPath string `json:"loginPath"`
	} `json:"auth"`
	AnonymousBanner bool `json:"anonymousBanner"`
	Controller      struct {
		Configured bool `json:"configured"`
	} `json:"controller"`
	Prometheus struct {
		Configured bool `json:"configured"`
	} `json:"prometheus"`
	Database struct {
		Configured bool `json:"configured"`
	} `json:"database"`
}

func TestConfigEndpointAdvertisesAnonymousBanner(t *testing.T) {
	w := do(t, newTestServer(t), "/api/v1/config")
	if w.Code != http.StatusOK {
		t.Fatalf("config = %d", w.Code)
	}
	var body configBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("config json: %v", err)
	}
	if body.Auth.Mode != "anonymous" || body.Auth.Role != "viewer" || !body.AnonymousBanner {
		t.Errorf("unexpected config payload: %+v", body)
	}
	if body.Database.Configured {
		t.Errorf("database.configured = true, want false: newTestServer wires no Events dependency")
	}
}

// fakeConfigEventLister is an EventLister double used only to make
// Deps.Events non-nil for TestConfigEndpointAdvertisesDatabaseConfigured — the
// handler under test never calls ListEvents.
type fakeConfigEventLister struct{}

func (fakeConfigEventLister) ListEvents(context.Context, store.EventFilter) (store.EventPage, error) {
	return store.EventPage{}, nil
}

// TestConfigEndpointAdvertisesDatabaseConfigured pins the M3 contract: the
// "database":{"configured":...} bit in /api/v1/config must track whether
// cmd/console wired an EventLister (i.e. db != nil in main.go), the same
// signal handleEvents' own 503 gate reads, so this endpoint never grows a
// second, independently-driftable notion of "is the database up".
func TestConfigEndpointAdvertisesDatabaseConfigured(t *testing.T) {
	cfg := &config.Config{HTTPPort: 8080, LogLevel: "info", LogFormat: "json", MetricsPrefix: "kconmon_ng", Auth: config.AuthConfig{Mode: "anonymous", Anonymous: config.AnonymousConfig{Role: "viewer"}}}
	reg := prometheus.NewRegistry()
	m := metrics.New(cfg.MetricsPrefix, reg)
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })
	s := NewServer(Deps{Config: cfg, Metrics: m, PromRegistry: reg, UI: ui, Events: fakeConfigEventLister{}})

	w := do(t, s, "/api/v1/config")
	if w.Code != http.StatusOK {
		t.Fatalf("config = %d", w.Code)
	}
	var body configBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("config json: %v", err)
	}
	if !body.Database.Configured {
		t.Errorf("database.configured = false, want true: Events is wired")
	}
}

// TestConfigEndpointLoginPathPerMode pins Task 18's authLoginPath: the
// frontend feature-detects the login flow from GET /api/v1/config instead of
// hardcoding auth.mode's four cases itself.
func TestConfigEndpointLoginPathPerMode(t *testing.T) {
	cases := []struct {
		mode string
		want string
	}{
		{"anonymous", ""},
		{"local", "/api/v1/auth/login"},
		{"header", ""},
		{"oidc", "/api/v1/auth/oidc/start"},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			cfg := authTestConfig(c.mode)
			reg := prometheus.NewRegistry()
			m := metrics.New(cfg.MetricsPrefix, reg)
			ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })
			s := NewServer(Deps{Config: cfg, Metrics: m, PromRegistry: reg, UI: ui})

			w := do(t, s, "/api/v1/config")
			if w.Code != http.StatusOK {
				t.Fatalf("config = %d", w.Code)
			}
			var body configBody
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("config json: %v", err)
			}
			if body.Auth.LoginPath != c.want {
				t.Errorf("mode=%q loginPath = %q, want %q", c.mode, body.Auth.LoginPath, c.want)
			}
		})
	}
}

func TestSPAFallbackServesUI(t *testing.T) {
	w := do(t, newTestServer(t), "/investigate")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `id="root"`) {
		t.Fatalf("SPA fallback = %d %q", w.Code, w.Body.String())
	}
}

func TestRunShutsDownOnContextCancel(t *testing.T) {
	s := newTestServer(t)
	s.cfg.HTTPPort = 0 // let the OS pick a free port to avoid clashes
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(ctx) }()
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error on clean shutdown: %v", err)
	}
}

// fakeRealtime is a RealtimeStatus double. The whole point of the interface is
// that a test needs one bool, not a controller and a gRPC stream.
type fakeRealtime struct{ healthy atomic.Bool }

func (f *fakeRealtime) Healthy() bool { return f.healthy.Load() }

// newRealtimeTestServer is newTestServer plus a hub and a realtime status.
func newRealtimeTestServer(t *testing.T, hub *ws.Hub, realtime RealtimeStatus) *Server {
	t.Helper()
	cfg := &config.Config{HTTPPort: 8080, LogLevel: "info", LogFormat: "json", MetricsPrefix: "kconmon_ng", Auth: config.AuthConfig{Mode: "anonymous", Anonymous: config.AnonymousConfig{Role: "viewer"}}}
	reg := prometheus.NewRegistry()
	m := metrics.New(cfg.MetricsPrefix, reg)
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<div id="root"></div>`))
	})
	return NewServer(Deps{Config: cfg, Metrics: m, PromRegistry: reg, UI: ui, Hub: hub, Realtime: realtime})
}

func versionCapabilities(t *testing.T, s *Server) []string {
	t.Helper()
	w := do(t, s, "/api/v1/version")
	if w.Code != http.StatusOK {
		t.Fatalf("version = %d", w.Code)
	}
	var body struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("version json: %v", err)
	}
	return body.Capabilities
}

func TestVersionCapabilitiesEmptyWithoutRealtime(t *testing.T) {
	s := newTestServer(t)
	if got := versionCapabilities(t, s); len(got) != 0 {
		t.Errorf("capabilities = %v, want empty when no realtime status is wired", got)
	}
	// The JSON must be [] and never null: the frontend indexes into it.
	if body := do(t, s, "/api/v1/version").Body.String(); !strings.Contains(body, `"capabilities":[]`) {
		t.Errorf("version body = %s, want a literal empty array", body)
	}
}

func TestVersionCapabilitiesFollowIngesterHealth(t *testing.T) {
	realtime := &fakeRealtime{}
	s := newRealtimeTestServer(t, nil, realtime)

	if got := versionCapabilities(t, s); len(got) != 0 {
		t.Errorf("capabilities = %v, want empty while the ingester is unhealthy", got)
	}

	realtime.healthy.Store(true)
	got := versionCapabilities(t, s)
	if len(got) != 1 || got[0] != "events" {
		t.Errorf(`capabilities = %v, want ["events"] while the ingester holds a stream`, got)
	}

	// Computed per request, not cached at boot: losing the stream mid-session
	// must withdraw the capability.
	realtime.healthy.Store(false)
	if got := versionCapabilities(t, s); len(got) != 0 {
		t.Errorf("capabilities = %v, want empty after the stream dropped", got)
	}
}

func TestWSWithoutHubReturns503Problem(t *testing.T) {
	w := do(t, newTestServer(t), "/ws")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("/ws without a hub = %d, want 503", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

// /ws is top level. If it were registered under /api/v1 this path would
// upgrade; instead it is an unknown API route.
//
// The expected answer here changed with QA round 5's finding #20: it used to
// be the SPA's 200, which is what that finding was about. The CLAIM this test
// makes is unchanged and is the one that matters -- /api/v1/ws does not
// upgrade, so /ws is not registered under the API prefix.
func TestWSIsNotUnderAPIV1(t *testing.T) {
	hub := ws.NewHub(cache.NewInProcessBus(), metrics.New("kconmon_ng", prometheus.NewRegistry()))
	w := do(t, newRealtimeTestServer(t, hub, nil), "/api/v1/ws")
	if w.Code != http.StatusNotFound {
		t.Fatalf("/api/v1/ws = %d %q, want 404 — it must not upgrade", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no such API route") {
		t.Errorf("/api/v1/ws body = %q, want the API catch-all's problem body", w.Body.String())
	}
}

// The route upgrades for real through the whole chi + instrument chain, which is
// also what proves statusRecorder forwards Hijack.
func TestWSRouteUpgradesThroughTheMiddlewareChain(t *testing.T) {
	hub := ws.NewHub(cache.NewInProcessBus(), metrics.New("kconmon_ng", prometheus.NewRegistry()))
	s := newRealtimeTestServer(t, hub, nil)
	httpSrv := httptest.NewServer(s.Handler())
	defer httpSrv.Close()

	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(httpSrv.URL, "http")+"/ws", nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
			_ = resp.Body.Close()
		}
		t.Fatalf("dial /ws: %v (http status %d)", err, status)
	}
	defer func() { _ = conn.Close() }()
	if resp != nil {
		_ = resp.Body.Close()
	}

	deadline := time.Now().Add(5 * time.Second)
	for hub.ClientCount() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("hub client count = %d after 5s, want 1", hub.ClientCount())
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := conn.WriteJSON(ws.ClientMessage{Action: ws.ActionSubscribe, Topic: ws.TopicTopology}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	var env ws.Envelope
	for {
		hub.Broadcast(ws.TopicTopology, ws.TypeSnapshot, json.RawMessage(`{"nodes":[]}`))
		if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		if err := conn.ReadJSON(&env); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no snapshot arrived over the /ws route within 5s")
		}
	}
	if env.Topic != ws.TopicTopology || env.Type != ws.TypeSnapshot {
		t.Errorf("envelope = %+v, want a topology snapshot", env)
	}

	// The long-lived upgrade is recorded once, when it closes, as path="/ws".
	_ = conn.Close()
	metricsBody := ""
	for time.Now().Before(deadline) {
		metricsBody = do(t, s, "/metrics").Body.String()
		if strings.Contains(metricsBody, `path="/ws"`) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf(`/metrics never recorded path="/ws"; body was:\n%s`, metricsBody)
}

/* ── QA round 5, finding #20: an unknown /api/* route is a 404, not the SPA ── */

// TestUnknownAPIRouteIs404ProblemJSON pins the catch-all. Before it,
// GET /api/v1/nope fell through to r.NotFound and got the SPA's index.html
// with a 200: a client (or a curl, or a test) asking for a route that does not
// exist was told everything is fine and handed HTML, which is the single most
// confusing answer an API can give.
//
// The whole /api prefix is covered, not just /api/v1: an operator who types
// /api/targets has the same right to be told there is no such route.
func TestUnknownAPIRouteIs404ProblemJSON(t *testing.T) {
	s := newTestServer(t)
	for _, target := range []string{
		"/api/v1/nope",
		"/api/v1/targets/extra/segments",
		"/api/v2/version",
		"/api/nope",
		"/api",
	} {
		t.Run(target, func(t *testing.T) {
			w := do(t, s, target)
			if w.Code != http.StatusNotFound {
				t.Fatalf("GET %s = %d, want 404 (body: %s)", target, w.Code, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("GET %s Content-Type = %q, want application/problem+json", target, ct)
			}
			var p map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
				t.Fatalf("GET %s body is not JSON: %v (%s)", target, err, w.Body.String())
			}
			if p["title"] != "no such API route" {
				t.Errorf("GET %s title = %v, want %q", target, p["title"], "no such API route")
			}
		})
	}
}

// The catch-all answers every METHOD, not just GET: a POST to a route that
// does not exist is the same mistake and deserves the same answer, not an
// HTML page a client will try to parse as a create response.
func TestUnknownAPIRouteIs404ForEveryMethod(t *testing.T) {
	s := newTestServer(t)
	for _, method := range []string{
		http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
	} {
		req := httptest.NewRequestWithContext(context.Background(), method, "/api/v1/nope", http.NoBody)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s /api/v1/nope = %d, want 404", method, w.Code)
		}
	}
}

// …and it must not have eaten anything. The SPA fallback, the probes and a
// real API route all keep answering exactly as before.
func TestAPICatchAllLeavesEveryOtherRouteAlone(t *testing.T) {
	s := newTestServer(t)
	for _, tc := range []struct {
		target string
		code   int
		body   string
	}{
		{"/", http.StatusOK, `<div id="root"></div>`},
		{"/targets", http.StatusOK, `<div id="root"></div>`},
		{"/api-keys", http.StatusOK, `<div id="root"></div>`}, // a UI path that merely STARTS with "api"
		{"/healthz", http.StatusOK, "ok"},
	} {
		w := do(t, s, tc.target)
		if w.Code != tc.code || w.Body.String() != tc.body {
			t.Errorf("GET %s = %d %q, want %d %q", tc.target, w.Code, w.Body.String(), tc.code, tc.body)
		}
	}
	if w := do(t, s, "/api/v1/version"); w.Code != http.StatusOK {
		t.Errorf("GET /api/v1/version = %d, want 200 — the catch-all must not shadow a real route", w.Code)
	}
}
