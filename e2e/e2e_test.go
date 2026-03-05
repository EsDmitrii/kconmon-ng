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

func TestHealthz(t *testing.T) {
	baseURL := getBaseURL()
	resp, err := http.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("healthz request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected /healthz 200, got %d", resp.StatusCode)
	}
}

func TestReadyz(t *testing.T) {
	baseURL := getBaseURL()
	resp, err := http.Get(baseURL + "/readyz")
	if err != nil {
		t.Fatalf("readyz request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected /readyz 200, got %d", resp.StatusCode)
	}
}

func TestMetrics(t *testing.T) {
	baseURL := getBaseURL()
	resp, err := http.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatalf("metrics request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected /metrics 200, got %d", resp.StatusCode)
	}

	// Basic check for Prometheus format
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/plain") && !strings.Contains(ct, "text/html") {
		t.Logf("metrics content-type: %s (prometheus may use text/plain)", ct)
	}
}

func TestTopology(t *testing.T) {
	baseURL := getBaseURL()

	// Allow time for agents to register
	time.Sleep(2 * time.Second)

	resp, err := http.Get(baseURL + "/api/v1/topology")
	if err != nil {
		t.Fatalf("topology request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected /api/v1/topology 200, got %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("expected JSON response, got content-type: %s", ct)
	}
}
