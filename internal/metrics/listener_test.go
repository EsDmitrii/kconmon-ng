package metrics //nolint:revive // var-naming: "metrics" is a valid internal package name, not a stdlib conflict

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The three routes every component's metrics listener serves, with the readiness plumbing.
func TestListenerHandlerFixedRoutes(t *testing.T) {
	reg := prometheus.NewRegistry()
	ready := false
	h := NewListenerHandler(reg, func() bool { return ready })

	if rec := get(t, h, "/metrics"); rec.Code != http.StatusOK {
		t.Errorf("/metrics status = %d, want 200", rec.Code)
	}
	if rec := get(t, h, "/healthz"); rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("/healthz = %d %q, want 200 ok", rec.Code, rec.Body.String())
	}
	if rec := get(t, h, "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz status before ready = %d, want 503", rec.Code)
	}
	ready = true
	if rec := get(t, h, "/readyz"); rec.Code != http.StatusOK {
		t.Errorf("/readyz status when ready = %d, want 200", rec.Code)
	}
}

// A nil readiness func means always ready (the console passes its own; the tests here do not).
func TestListenerHandlerNilReadyIsReady(t *testing.T) {
	h := NewListenerHandler(prometheus.NewRegistry(), nil)
	if rec := get(t, h, "/readyz"); rec.Code != http.StatusOK {
		t.Errorf("/readyz with nil ready = %d, want 200", rec.Code)
	}
}

// The controller mounts its Prometheus SD body here because metricsPort is the one port the
// chart opens to the scraper; the extra route rides on the same mux.
func TestListenerHandlerExtraRoute(t *testing.T) {
	extra := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("sd"))
	})
	h := NewListenerHandler(prometheus.NewRegistry(), nil,
		Route{Pattern: "GET /api/v1/prometheus/sd", Handler: extra})

	rec := get(t, h, "/api/v1/prometheus/sd")
	if rec.Code != http.StatusTeapot || rec.Body.String() != "sd" {
		t.Errorf("extra route = %d %q, want 418 sd", rec.Code, rec.Body.String())
	}
	// The fixed routes are untouched by the addition.
	if rec := get(t, h, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("/healthz next to an extra route = %d, want 200", rec.Code)
	}
}

// The two-argument call the agent and the console make keeps its old surface: nothing but the
// three fixed routes, so an SD path on an agent's listener is a 404.
func TestListenerHandlerNoExtraRouteIs404(t *testing.T) {
	h := NewListenerHandler(prometheus.NewRegistry(), nil)
	if rec := get(t, h, "/api/v1/prometheus/sd"); rec.Code != http.StatusNotFound {
		t.Errorf("unmounted path = %d, want 404", rec.Code)
	}
}
