package checks_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
)

// bodyRecordingServer captures every POST /api/v1/diagnostics request body VERBATIM (the raw bytes,
// not a decoded struct) and answers a canned success; decoding into a struct would hide exactly
// what this file exists to pin down.
type bodyRecordingServer struct {
	mu     sync.Mutex
	bodies []string
}

func (s *bodyRecordingServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		s.mu.Lock()
		s.bodies = append(s.bodies, string(raw))
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"type":"tcp","duration":5000000}`))
	})
}

func (s *bodyRecordingServer) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.bodies))
	copy(out, s.bodies)
	return out
}

func startBodyRecordingRunner(t *testing.T) (*bodyRecordingServer, *checks.Runner, *checks.MemoryStore) {
	t.Helper()
	rec := &bodyRecordingServer{}
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)
	ctrl := controllerclient.New(srv.URL, 10*time.Second)
	bus := newRecordingBus()
	hub := ws.NewHub(bus, testMetrics(t))
	mem := checks.NewMemoryStore()
	return rec, checks.NewRunner(ctrl, hub, bus, mem, testMetrics(t)), mem
}

func TestDispatchBodyForNodeOnlySpecIsM3Identical(t *testing.T) {
	rec, runner, mem := startBodyRecordingRunner(t)

	spec := checks.Spec{
		Sources: []string{"n1"}, Destinations: []string{"n2"},
		Type: "tcp", Plane: "pod", Timeout: 2 * time.Second,
	}
	id, err := runner.Start(context.Background(), spec, testInitiator())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForTerminal(t, mem, id)

	bodies := rec.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("got %d dispatch bodies, want 1: %q", len(bodies), bodies)
	}
	const want = `{"source":"n1","destination":"n2","type":"tcp","plane":"pod"}`
	if bodies[0] != want {
		t.Errorf("dispatch body =\n  %s\nwant (M3 verbatim) =\n  %s", bodies[0], want)
	}
}

// A target destination travels as destinationKind=external with the address alongside it.
func TestDispatchBodyForTargetDestinationIsExternal(t *testing.T) {
	rec, runner, mem := startBodyRecordingRunner(t)

	spec := checks.Spec{
		Sources: []string{"n1"},
		TypedDestinations: []checks.Destination{
			{Kind: checks.DestKindTarget, Name: "api-prod", Address: "api.example.com:443"},
		},
		Type: "tcp", Plane: "pod", Timeout: 2 * time.Second,
	}
	id, err := runner.Start(context.Background(), spec, testInitiator())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForTerminal(t, mem, id)

	bodies := rec.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("got %d dispatch bodies, want 1: %q", len(bodies), bodies)
	}
	const want = `{"source":"n1","destination":"api-prod","type":"tcp","plane":"pod",` +
		`"destinationKind":"external","destinationAddress":"api.example.com:443"}`
	if bodies[0] != want {
		t.Errorf("dispatch body =\n  %s\nwant =\n  %s", bodies[0], want)
	}
}

// An adhoc (operator-typed) destination dispatches the same external shape;
// with no name given, the address doubles as the label, matching
// Destination.Label's fallback and the controller's own destName fallback.
func TestDispatchBodyForAdhocDestinationUsesAddressAsLabel(t *testing.T) {
	rec, runner, mem := startBodyRecordingRunner(t)

	spec := checks.Spec{
		Sources:           []string{"n1"},
		TypedDestinations: []checks.Destination{{Kind: checks.DestKindAdhoc, Address: "10.0.0.7"}},
		Type:              "icmp", Plane: "pod", Timeout: 2 * time.Second,
	}
	id, err := runner.Start(context.Background(), spec, testInitiator())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForTerminal(t, mem, id)

	bodies := rec.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("got %d dispatch bodies, want 1: %q", len(bodies), bodies)
	}
	const want = `{"source":"n1","destination":"10.0.0.7","type":"icmp","plane":"pod",` +
		`"destinationKind":"external","destinationAddress":"10.0.0.7"}`
	if bodies[0] != want {
		t.Errorf("dispatch body =\n  %s\nwant =\n  %s", bodies[0], want)
	}
}

// The persisted per-pair row and the progress frames must both carry the
// destination's LABEL, not its address -- check_results.destination_node is
// what a metric label and the UI both read.
func TestExternalPairPersistsLabelNotAddress(t *testing.T) {
	_, runner, mem := startBodyRecordingRunner(t)

	spec := checks.Spec{
		Sources: []string{"n1"},
		TypedDestinations: []checks.Destination{
			{Kind: checks.DestKindTarget, Name: "api-prod", Address: "api.example.com:443"},
		},
		Type: "tcp", Plane: "pod", Timeout: 2 * time.Second,
	}
	id, err := runner.Start(context.Background(), spec, testInitiator())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForTerminal(t, mem, id)

	results, err := runner.GetResults(context.Background(), id)
	if err != nil {
		t.Fatalf("GetResults: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].DestinationNode != "api-prod" {
		t.Errorf("result.DestinationNode = %q, want the metric-safe name api-prod", results[0].DestinationNode)
	}
}

// The spec snapshot persisted for a node-only run must stay byte-identical to the too.
func TestSpecSnapshotForNodeOnlyRunIsM3Identical(t *testing.T) {
	_, runner, mem := startBodyRecordingRunner(t)

	spec := checks.Spec{
		Sources: []string{"n1"}, Destinations: []string{"n2"},
		Type: "tcp", Plane: "pod", Timeout: 2 * time.Second,
	}
	id, err := runner.Start(context.Background(), spec, testInitiator())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	run, err := mem.GetRun(context.Background(), id)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	const want = `{"Sources":["n1"],"Destinations":["n2"],"Type":"tcp","Plane":"pod","Timeout":2000000000}`
	if string(run.Spec) != want {
		t.Errorf("spec snapshot =\n  %s\nwant (M3 verbatim) =\n  %s", run.Spec, want)
	}
	waitForTerminal(t, mem, id)
}

func TestDiagnoseRequestNodeRunLeavesExternalFieldsZero(t *testing.T) {
	req := controllerclient.DiagnoseRequest{Source: "n1", Destination: "n2", Type: "tcp", Plane: "pod"}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != `{"source":"n1","destination":"n2","type":"tcp","plane":"pod"}` {
		t.Errorf("marshalled = %s", raw)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := back["destinationKind"]; ok {
		t.Error("destinationKind must be absent from a node run's body")
	}
	if _, ok := back["destinationAddress"]; ok {
		t.Error("destinationAddress must be absent from a node run's body")
	}
}
