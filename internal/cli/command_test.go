package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

// fakeConnector returns a Connection pointing at a test server; Close is a
// no-op teardown.
type fakeConnector struct{ baseURL string }

func (f fakeConnector) Connect(_ context.Context) (*Connection, error) {
	return &Connection{BaseURL: f.baseURL, Close: func() {}}, nil
}

// withFakeController spins up an httptest.Server, points connectorFactory at
// it, runs the given command args, and returns stdout plus the exit code from
// Execute-equivalent error mapping. The original factory is restored on cleanup.
func runCLI(t *testing.T, handler http.Handler, args ...string) (string, string, int) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	orig := connectorFactory
	connectorFactory = func(_ *globalOptions) Connector { return fakeConnector{baseURL: srv.URL} }
	t.Cleanup(func() { connectorFactory = orig })

	root := newRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(args)

	err := root.Execute()
	code := exitCodeFor(err)
	return stdout.String(), stderr.String(), code
}

// exitCodeFor mirrors Execute's error-to-code mapping for tests.
func exitCodeFor(err error) int {
	switch {
	case err == nil:
		return exitOK
	case isCheckFailed(err):
		return exitCheck
	default:
		return exitError
	}
}

func isCheckFailed(err error) bool {
	return err != nil && strings.Contains(err.Error(), errCheckFailed.Error())
}

const topologyFixture = `{
  "nodes": [
    {"name": "node-1", "zone": "us-east-1a", "ready": true},
    {"name": "node-2", "zone": "us-east-1b", "ready": true}
  ],
  "agents": [
    {"id": "node-1-kconmon-ng-agent-aaaaa", "nodeName": "node-1", "podIP": "10.0.0.1", "zone": "us-east-1a", "lastSeen": "2025-01-01T00:00:00Z"},
    {"id": "node-2-kconmon-ng-agent-bbbbb", "nodeName": "node-2", "podIP": "10.0.0.2", "zone": "us-east-1b", "lastSeen": "2025-01-01T00:00:00Z"}
  ],
  "timestamp": "2025-01-01T00:00:00Z"
}`

func topologyHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/topology", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, topologyFixture)
	})
	return mux
}

func TestTopologyCommandTable(t *testing.T) {
	out, _, code := runCLI(t, topologyHandler(), "topology")
	if code != exitOK {
		t.Fatalf("exit=%d, want 0", code)
	}
	if !strings.Contains(out, "NODE") || !strings.Contains(out, "node-1-kconmon-ng-agent-aaaaa") {
		t.Errorf("unexpected topology table:\n%s", out)
	}
}

func TestTopologyCommandJSON(t *testing.T) {
	out, _, code := runCLI(t, topologyHandler(), "topology", "-o", "json")
	if code != exitOK {
		t.Fatalf("exit=%d, want 0", code)
	}
	var snap model.TopologySnapshot
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		t.Fatalf("json output not decodable: %v\n%s", err, out)
	}
	if len(snap.Nodes) != 2 || len(snap.Agents) != 2 {
		t.Errorf("unexpected decoded snapshot: %+v", snap)
	}
}

func TestAgentsCommand(t *testing.T) {
	out, _, code := runCLI(t, topologyHandler(), "agents")
	if code != exitOK {
		t.Fatalf("exit=%d, want 0", code)
	}
	if !strings.Contains(out, "POD IP") || !strings.Contains(out, "10.0.0.2") {
		t.Errorf("unexpected agents table:\n%s", out)
	}
}

func diagnosticsHandler(status int, body string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/diagnostics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
	return mux
}

func TestCheckCommandSuccessExit0(t *testing.T) {
	body := `{"type":"icmp","success":true,"source":"node-1","destination":"node-2","sourceZone":"us-east-1a","destZone":"us-east-1b","duration":1500000,"details":{"rtt":2000000,"lossRatio":0}}`
	out, _, code := runCLI(t, diagnosticsHandler(http.StatusOK, body), "check", "node-1", "node-2")
	if code != exitOK {
		t.Fatalf("exit=%d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "OK") || !strings.Contains(out, "rtt=2ms") {
		t.Errorf("unexpected check output:\n%s", out)
	}
}

func TestCheckCommandFailureExit2(t *testing.T) {
	body := `{"type":"tcp","success":false,"source":"node-1","destination":"node-2","duration":50000000,"error":"connection refused"}`
	out, stderr, code := runCLI(t, diagnosticsHandler(http.StatusOK, body), "check", "node-1", "node-2", "--type", "tcp")
	if code != exitCheck {
		t.Fatalf("exit=%d, want 2 (check failed)\nout=%s\nerr=%s", code, out, stderr)
	}
	if !strings.Contains(out, "FAIL") {
		t.Errorf("expected FAIL summary:\n%s", out)
	}
}

func TestCheckCommandAPIErrorExit1(t *testing.T) {
	// 404 unknown node from the controller => CLI/API error => exit 1.
	_, _, code := runCLI(t, diagnosticsHandler(http.StatusNotFound, "no agent registered on source node\n"),
		"check", "ghost", "node-2")
	if code != exitError {
		t.Fatalf("exit=%d, want 1 (api error)", code)
	}
}

func TestCheckCommandJSONExit2(t *testing.T) {
	// JSON output still yields exit 2 on success=false.
	body := `{"type":"icmp","success":false,"source":"node-1","destination":"node-2","duration":0,"error":"timeout"}`
	out, _, code := runCLI(t, diagnosticsHandler(http.StatusOK, body), "check", "node-1", "node-2", "-o", "json")
	if code != exitCheck {
		t.Fatalf("exit=%d, want 2\n%s", code, out)
	}
	var res model.CheckResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("json output not decodable: %v\n%s", err, out)
	}
}

func TestMTRCommand(t *testing.T) {
	body := `{"type":"mtr","success":true,"source":"node-1","destination":"node-2","duration":3000000,"details":{"target":"10.0.0.2","hops":[{"number":1,"ip":"10.0.0.254","rtt":500000,"lossRatio":0},{"number":2,"ip":"*","rtt":0,"lossRatio":1},{"number":3,"ip":"10.0.0.2","rtt":2000000,"lossRatio":0}]}}`
	out, _, code := runCLI(t, diagnosticsHandler(http.StatusOK, body), "mtr", "node-1", "node-2")
	if code != exitOK {
		t.Fatalf("exit=%d, want 0\n%s", code, out)
	}
	for _, want := range []string{"HOP", "10.0.0.254", "no reply", "500µs", "reached 10.0.0.2"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in mtr output:\n%s", want, out)
		}
	}
}

func TestVersionCommand(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"version":"v1.2.3","commit":"abc123"}`)
	})
	out, _, code := runCLI(t, mux, "version")
	if code != exitOK {
		t.Fatalf("exit=%d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "Client:") || !strings.Contains(out, "v1.2.3") {
		t.Errorf("unexpected version output:\n%s", out)
	}
}

func TestInvalidOutputExit1(t *testing.T) {
	_, stderr, code := runCLI(t, topologyHandler(), "topology", "-o", "yaml")
	if code != exitError {
		t.Fatalf("exit=%d, want 1", code)
	}
	_ = stderr
}

// TestCheckCommandRespectsClientDeadline proves the diagnostics call gives up
// once the client-side deadline (--timeout + slack) elapses, instead of
// hanging on a controller that never responds (e.g. a stalled port-forward).
// The slack is shrunk for the duration of the test so a ~100ms deadline is
// enough to exercise the real code path without slowing the suite down.
func TestCheckCommandRespectsClientDeadline(t *testing.T) {
	origSlack := diagnosticsDeadlineSlack
	diagnosticsDeadlineSlack = 50 * time.Millisecond
	t.Cleanup(func() { diagnosticsDeadlineSlack = origSlack })

	block := make(chan struct{})
	// defer, not t.Cleanup: cleanups run LIFO, so srv.Close (registered later by
	// runCLI) would wait for the blocked handler before this close ever ran.
	defer close(block)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/diagnostics", func(w http.ResponseWriter, r *http.Request) {
		// Drain the body so net/http starts its client-disconnect watcher and
		// the ctx.Done branch is a real escape hatch, not dead code.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-block:
		case <-r.Context().Done():
		}
	})

	start := time.Now()
	_, _, code := runCLI(t, mux, "check", "node-1", "node-2", "--timeout", "50ms")
	elapsed := time.Since(start)

	if code != exitError {
		t.Fatalf("exit=%d, want 1 (client deadline exceeded)", code)
	}
	// Client deadline is ~100ms (50ms timeout + 50ms slack); allow generous
	// headroom above that so this isn't flaky under load, while still proving
	// we didn't hang indefinitely.
	if elapsed > 5*time.Second {
		t.Fatalf("command took %s, want well under 5s", elapsed)
	}
}
