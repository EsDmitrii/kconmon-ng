//go:build e2e

package e2e

import (
	"encoding/json"
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

// Every request below goes through console_test.go's request helper, directly or via mustRequest, so
// it carries a timeout.

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
	status, _, body := mustRequest(t, http.MethodGet, baseURL+"/metrics", nil)
	if status != http.StatusOK {
		t.Errorf("expected /metrics 200, got %d", status)
	}
	// The controller sets this gauge at startup, so an exposition without it is not the controller's.
	if !strings.Contains(string(body), "\nkconmon_ng_controller_registered_agents ") {
		t.Errorf("/metrics lacks kconmon_ng_controller_registered_agents")
	}
}

func TestTopology(t *testing.T) {
	baseURL := getBaseURL()

	// The kind config has two workers, so at least two agents must register.
	var agents int
	pollUntil(t, 60*time.Second, 2*time.Second, "at least two agents in /api/v1/topology", func() bool {
		status, header, body, err := request(t, http.MethodGet, baseURL+"/api/v1/topology", nil)
		if err != nil || status != http.StatusOK || !strings.Contains(header.Get("Content-Type"), "application/json") {
			t.Logf("topology: status %d, content-type %q, err %v (will retry)", status, header.Get("Content-Type"), err)
			return false
		}
		var topo struct {
			Agents []json.RawMessage `json:"agents"`
		}
		if err := json.Unmarshal(body, &topo); err != nil {
			t.Logf("decode topology failed (will retry): %v", err)
			return false
		}
		agents = len(topo.Agents)
		return agents >= 2
	})
	t.Logf("topology lists %d agents", agents)
}

// pollUntil polls fn every interval until it returns true or budget elapses; the package's retry
// loop for eventually consistent state (event history catching up, a run reaching a terminal status).
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
