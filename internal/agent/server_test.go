package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestAgentHealthz(t *testing.T) {
	promReg := prometheus.NewRegistry()
	srv := NewHTTPServer(promReg)

	req := httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestAgentReadyzNotReady(t *testing.T) {
	promReg := prometheus.NewRegistry()
	srv := NewHTTPServer(promReg)

	req := httptest.NewRequest(http.MethodGet, "/readyz", http.NoBody)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when not ready, got %d", w.Code)
	}
}

func TestAgentReadyzReady(t *testing.T) {
	promReg := prometheus.NewRegistry()
	srv := NewHTTPServer(promReg)
	srv.SetReady(true)

	req := httptest.NewRequest(http.MethodGet, "/readyz", http.NoBody)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 when ready, got %d", w.Code)
	}
}

func TestAgentVersion(t *testing.T) {
	promReg := prometheus.NewRegistry()
	srv := NewHTTPServer(promReg)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/version", http.NoBody)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	if _, ok := resp["version"]; !ok {
		t.Error("expected version in response")
	}
	if _, ok := resp["commit"]; !ok {
		t.Error("expected commit in response")
	}
}

func TestAgentMetrics(t *testing.T) {
	promReg := prometheus.NewRegistry()
	srv := NewHTTPServer(promReg)

	req := httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}
