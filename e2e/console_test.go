//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// consoleBaseURL returns KCONMON_CONSOLE_URL, or skips the test when unset.
// Unlike getBaseURL (the controller helper above), there is deliberately no
// localhost default: the console and controller are separate Services on
// separate ports, and guessing a port here would silently point console
// assertions at whatever happens to be listening instead of skipping
// cleanly on a workflow run that never wired the console up.
func consoleBaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("KCONMON_CONSOLE_URL")
	if url == "" {
		t.Skip("KCONMON_CONSOLE_URL not set")
	}
	return strings.TrimSuffix(url, "/")
}

func TestConsoleHealthz(t *testing.T) {
	base := consoleBaseURL(t)
	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("healthz request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected /healthz 200, got %d", resp.StatusCode)
	}
}

func TestConsoleReadyz(t *testing.T) {
	base := consoleBaseURL(t)
	resp, err := http.Get(base + "/readyz")
	if err != nil {
		t.Fatalf("readyz request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected /readyz 200, got %d", resp.StatusCode)
	}
}

func TestConsoleMetrics(t *testing.T) {
	base := consoleBaseURL(t)
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatalf("metrics request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected /metrics 200, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read metrics body: %v", err)
	}
	text := string(body)

	if !strings.Contains(text, "kconmon_ng_console_") {
		t.Errorf("expected /metrics to contain kconmon_ng_console_ series")
	}

	// Proof the store was actually exercised, not merely compiled in: this
	// series is only emitted once the console issues a query against
	// PostgreSQL, which console-values.yaml's database.mode=external makes
	// happen (e.g. GET /api/v1/config's own database.configured check, or
	// the events/runs polling elsewhere in this file). It rides the same
	// ingester/store pipeline as TestConsoleEvents, so it is polled rather
	// than asserted on a single GET: this is observability-of-arrival for
	// that pipeline, not a race assertion on unrelated behavior.
	pollUntil(t, 60*time.Second, 2*time.Second,
		"kconmon_ng_console_store_queries_total to appear in /metrics (store not exercised)", func() bool {
			resp, err := http.Get(base + "/metrics")
			if err != nil {
				t.Logf("metrics request failed (will retry): %v", err)
				return false
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Logf("metrics request returned %d (will retry)", resp.StatusCode)
				return false
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Logf("read metrics body failed (will retry): %v", err)
				return false
			}
			return strings.Contains(string(body), "kconmon_ng_console_store_queries_total")
		})
}

func TestConsoleVersion(t *testing.T) {
	base := consoleBaseURL(t)
	resp, err := http.Get(base + "/api/v1/version")
	if err != nil {
		t.Fatalf("version request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected /api/v1/version 200, got %d", resp.StatusCode)
	}

	var body struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode version response: %v", err)
	}
	// capabilities is asserted to be a present array only -- NOT that it
	// contains "events". Whether the realtime pipeline is healthy at the
	// moment this request lands depends on leader/reconnect timing; asserting
	// its presence would make this test flake on exactly the kind of blip
	// that is not a real bug.
	if body.Capabilities == nil {
		t.Errorf("expected capabilities to be a JSON array (possibly empty), got null")
	}
}

func TestConsoleConfig(t *testing.T) {
	base := consoleBaseURL(t)
	resp, err := http.Get(base + "/api/v1/config")
	if err != nil {
		t.Fatalf("config request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected /api/v1/config 200, got %d", resp.StatusCode)
	}

	var body struct {
		Database struct {
			Configured bool `json:"configured"`
		} `json:"database"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode config response: %v", err)
	}
	if !body.Database.Configured {
		t.Errorf("expected database.configured=true (console-values.yaml sets console.database.mode=external)")
	}
}

// TestConsoleEvents is the single most valuable assertion in this file:
// agents registering with the controller produce topology_changed domain
// events, which -- with controller.events.enabled=true and
// database.mode=external wired -- the console's ingester consumes and
// persists. A non-empty history here is end-to-end proof that
// ingester -> store -> API works in a real cluster, not just in a unit test.
func TestConsoleEvents(t *testing.T) {
	base := consoleBaseURL(t)

	var last struct {
		Events []json.RawMessage `json:"events"`
	}

	pollUntil(t, 60*time.Second, 2*time.Second,
		"at least one persisted event (topology_changed from agent registration)", func() bool {
			resp, err := http.Get(base + "/api/v1/events")
			if err != nil {
				t.Logf("events request failed (will retry): %v", err)
				return false
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Logf("events request returned %d (will retry)", resp.StatusCode)
				return false
			}
			if err := json.NewDecoder(resp.Body).Decode(&last); err != nil {
				t.Logf("decode events response failed (will retry): %v", err)
				return false
			}
			return len(last.Events) > 0
		})
}

// TestConsoleRuns exercises POST /api/v1/runs end to end against the real
// agents in the cluster: an empty sources/destinations pair fans out to
// every node in the current topology (checks.Plan's documented fallback), so
// on a 3-node kind cluster this dispatches several TCP pairs through the
// real controller/agent path, not a stub.
func TestConsoleRuns(t *testing.T) {
	base := consoleBaseURL(t)

	reqBody := map[string]any{
		"sources":      []string{},
		"destinations": []string{},
		"type":         "tcp",
		"plane":        "pod",
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal run request: %v", err)
	}

	resp, err := http.Post(base+"/api/v1/runs", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("create run request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		errBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected POST /api/v1/runs 202, got %d: %s", resp.StatusCode, errBody)
	}
	if resp.Header.Get("Location") == "" {
		t.Errorf("expected a Location header on the 202 response")
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create-run response: %v", err)
	}
	if created.ID == "" {
		t.Fatalf("expected a non-empty run id in the create-run response")
	}

	var final struct {
		Status    string `json:"status"`
		PairTotal int32  `json:"pairTotal"`
	}
	pollUntil(t, 60*time.Second, 2*time.Second,
		fmt.Sprintf("run %s to reach a terminal status", created.ID), func() bool {
			resp, err := http.Get(base + "/api/v1/runs/" + created.ID)
			if err != nil {
				t.Logf("get run request failed (will retry): %v", err)
				return false
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Logf("get run returned %d (will retry)", resp.StatusCode)
				return false
			}
			if err := json.NewDecoder(resp.Body).Decode(&final); err != nil {
				t.Logf("decode run response failed (will retry): %v", err)
				return false
			}
			// finalStatus's closed set (internal/console/checks/runner.go):
			// "succeeded", "failed", or "partial". "pending"/"running" are
			// not terminal.
			switch final.Status {
			case "succeeded", "failed", "partial":
				return true
			default:
				return false
			}
		})

	if final.PairTotal <= 0 {
		t.Errorf("expected pairTotal > 0, got %d", final.PairTotal)
	}

	listResp, err := http.Get(base + "/api/v1/runs")
	if err != nil {
		t.Fatalf("list runs request failed: %v", err)
	}
	defer listResp.Body.Close()

	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("expected GET /api/v1/runs 200, got %d", listResp.StatusCode)
	}

	var list struct {
		Runs []struct {
			ID string `json:"id"`
		} `json:"runs"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatalf("decode runs list: %v", err)
	}

	found := false
	for _, r := range list.Runs {
		if r.ID == created.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected run %s to appear in GET /api/v1/runs", created.ID)
	}
}

// TestConsoleDegradedMode runs against a SEPARATE console rollout with
// console.database.mode=disabled (.github/workflows/e2e.yaml's "Reinstall
// console with database disabled" + "Run degraded-mode E2E tests" steps
// redeploy and re-forward before invoking just this test by name) -- it
// reuses KCONMON_CONSOLE_URL, now pointed at that rollout's fresh
// port-forward. This is the Phase A guarantee (M3 plan Decision) verified in
// a real cluster: with no database wired, the M1/M2 surface (healthz,
// topology) stays fully up and only the M3 database-backed endpoints degrade
// to 503 -- never a 500, never a hang.
func TestConsoleDegradedMode(t *testing.T) {
	base := consoleBaseURL(t)

	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("healthz request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected /healthz 200 in degraded mode, got %d", resp.StatusCode)
	}

	topoResp, err := http.Get(base + "/api/v1/topology")
	if err != nil {
		t.Fatalf("topology request failed: %v", err)
	}
	defer topoResp.Body.Close()
	if topoResp.StatusCode != http.StatusOK {
		t.Errorf("expected /api/v1/topology 200 in degraded mode, got %d", topoResp.StatusCode)
	}

	eventsResp, err := http.Get(base + "/api/v1/events")
	if err != nil {
		t.Fatalf("events request failed: %v", err)
	}
	defer eventsResp.Body.Close()
	if eventsResp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected /api/v1/events 503 with console.database.mode=disabled, got %d", eventsResp.StatusCode)
	}
}
