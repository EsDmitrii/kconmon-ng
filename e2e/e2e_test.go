//go:build e2e

package e2e

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func getBaseURL() string {
	if url := os.Getenv("KCONMON_CONTROLLER_URL"); url != "" {
		return strings.TrimSuffix(url, "/")
	}
	return "http://localhost:8080"
}

func TestControllerPodsRunning(t *testing.T) {
	// This test assumes kubectl/kubernetes context is available.
	// In CI, we verify pods via the helm install wait and smoke test steps.
	// This is a placeholder for future kubectl-based pod checks.
	t.Skip("pod status checked via kubectl wait in workflow")
}

// Every request below goes through console_test.go's mustRequest helper (same package): it carries
// a context.

func TestHealthz(t *testing.T) {
	baseURL := getBaseURL()
	status, _, _ := mustRequest(t, http.MethodGet, baseURL+"/healthz", nil)
	if status != http.StatusOK {
		t.Errorf("expected /healthz 200, got %d", status)
	}
}

func TestReadyz(t *testing.T) {
	baseURL := getBaseURL()
	status, _, _ := mustRequest(t, http.MethodGet, baseURL+"/readyz", nil)
	if status != http.StatusOK {
		t.Errorf("expected /readyz 200, got %d", status)
	}
}

func TestMetrics(t *testing.T) {
	baseURL := getBaseURL()
	status, header, _ := mustRequest(t, http.MethodGet, baseURL+"/metrics", nil)
	if status != http.StatusOK {
		t.Errorf("expected /metrics 200, got %d", status)
	}

	// Basic check for Prometheus format
	ct := header.Get("Content-Type")
	if !strings.Contains(ct, "text/plain") && !strings.Contains(ct, "text/html") {
		t.Logf("metrics content-type: %s (prometheus may use text/plain)", ct)
	}
}

func TestTopology(t *testing.T) {
	baseURL := getBaseURL()

	// Allow time for agents to register
	time.Sleep(2 * time.Second)

	status, header, _ := mustRequest(t, http.MethodGet, baseURL+"/api/v1/topology", nil)
	if status != http.StatusOK {
		t.Errorf("expected /api/v1/topology 200, got %d", status)
	}

	ct := header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("expected JSON response, got content-type: %s", ct)
	}
}

// pollUntil polls fn every interval until it returns true or budget elapses; shared by this file
// and console_test.go for asserting on eventually-consistent state (event history catching up, a
// run reaching a terminal status) instead of each caller hand-rolling its own retry loop.
func pollUntil(t *testing.T, budget, interval time.Duration, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		if fn() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", budget, what)
		}
		time.Sleep(interval)
	}
}
