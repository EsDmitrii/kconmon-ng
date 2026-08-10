package checks_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
)

// gatedCtrl answers instantly for destinations in fast, and blocks until its own ctx is cancelled
// for every other destination.
type gatedCtrl struct {
	fast     map[string]bool
	calls    atomic.Int32
	blocking chan struct{} // one token per pair that reached the blocking wait
}

func newGatedCtrl(fast ...string) *gatedCtrl {
	c := &gatedCtrl{fast: make(map[string]bool), blocking: make(chan struct{}, 1024)}
	for _, f := range fast {
		c.fast[f] = true
	}
	return c
}

func (c *gatedCtrl) Topology(context.Context) (*controllerclient.Topology, error) {
	return &controllerclient.Topology{}, nil
}

func (c *gatedCtrl) Diagnose(ctx context.Context, req controllerclient.DiagnoseRequest, _ time.Duration) (json.RawMessage, error) {
	c.calls.Add(1)
	if c.fast[req.Destination] {
		return json.RawMessage(`{"success":true,"type":"tcp"}`), nil
	}
	c.blocking <- struct{}{}
	<-ctx.Done()
	return nil, ctx.Err()
}

// awaitBlocked waits until n pairs have reached the blocking wait.
func (c *gatedCtrl) awaitBlocked(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-c.blocking:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d pairs reached the blocking dispatch within 10s", i, n)
		}
	}
}

func newCancelRunner(t *testing.T, ctrl *gatedCtrl) (*checks.Runner, *checks.MemoryStore) {
	t.Helper()
	bus := newRecordingBus()
	hub := ws.NewHub(bus, testMetrics(t))
	mem := checks.NewMemoryStore()
	return checks.NewRunner(ctrl, hub, bus, mem, testMetrics(t)), mem
}

// The arithmetic is deterministic, not a race: 20 pairs; that pins exactly 8 blocked pairs, 4
// completed, and 8 (dst-12..dst-19) never dispatched.
func TestCancelMidRunKeepsCompletedResultsAndStopsDispatching(t *testing.T) {
	ctrl := newGatedCtrl("dst-00", "dst-01", "dst-02", "dst-03")
	runner, mem := newCancelRunner(t, ctrl)

	dests := make([]string, 20)
	for i := range dests {
		dests[i] = fmt.Sprintf("dst-%02d", i)
	}
	spec := checks.Spec{Sources: []string{"n1"}, Destinations: dests, Type: "tcp", Plane: "pod", Timeout: 5 * time.Second}
	id, err := runner.Start(context.Background(), spec, testInitiator())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctrl.awaitBlocked(t, 8)

	if cerr := runner.Cancel(context.Background(), id); cerr != nil {
		t.Fatalf("Cancel: %v", cerr)
	}

	run := waitForTerminal(t, mem, id)
	if run.Status != "cancelled" {
		t.Fatalf("run.Status = %q, want cancelled", run.Status)
	}

	if calls := ctrl.calls.Load(); calls != 12 {
		t.Errorf("Diagnose calls = %d, want exactly 12 (8 blocked + 4 fast; the remaining 8 pairs must never dispatch)", calls)
	}
	// Nothing may dispatch after the run is terminal either.
	settled := ctrl.calls.Load()
	time.Sleep(100 * time.Millisecond)
	if after := ctrl.calls.Load(); after != settled {
		t.Errorf("Diagnose calls grew from %d to %d after the run finished", settled, after)
	}

	results, err := runner.GetResults(context.Background(), id)
	if err != nil {
		t.Fatalf("GetResults: %v", err)
	}
	var ok int
	for i := range results {
		if results[i].Success {
			ok++
		}
	}
	if ok != 4 {
		t.Errorf("successful results = %d, want 4 (already-completed pairs must survive the cancel intact)", ok)
	}
	if len(results) != 12 {
		t.Errorf("persisted results = %d, want 12 (every dispatched pair records an outcome, cancelled or not)", len(results))
	}
	if run.PairTotal != 20 {
		t.Errorf("run.PairTotal = %d, want 20 (the planned total is not rewritten by a cancel)", run.PairTotal)
	}
}

// A cancelled run's WS stream still terminates with a finished frame carrying
// the cancelled status -- a browser watching the run must learn the outcome,
// not just see the topic close.
func TestCancelPublishesFinishedFrameWithCancelledStatus(t *testing.T) {
	ctrl := newGatedCtrl()
	bus := newRecordingBus()
	hub := ws.NewHub(bus, testMetrics(t))
	mem := checks.NewMemoryStore()
	runner := checks.NewRunner(ctrl, hub, bus, mem, testMetrics(t))

	spec := checks.Spec{Sources: []string{"n1"}, Destinations: []string{"n2"}, Type: "tcp", Plane: "pod", Timeout: 5 * time.Second}
	id, err := runner.Start(context.Background(), spec, testInitiator())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctrl.awaitBlocked(t, 1)
	if cerr := runner.Cancel(context.Background(), id); cerr != nil {
		t.Fatalf("Cancel: %v", cerr)
	}
	run := waitForTerminal(t, mem, id)
	if run.Status != "cancelled" {
		t.Fatalf("run.Status = %q, want cancelled", run.Status)
	}
}

// Cancelling a run that already reached a terminal status is a NO-OP.
func TestCancelTerminalRunIsANoOp(t *testing.T) {
	fake, ctrl := startFakeDiagnosticsServer(t)
	_ = fake
	bus := newRecordingBus()
	hub := ws.NewHub(bus, testMetrics(t))
	mem := checks.NewMemoryStore()
	runner := checks.NewRunner(ctrl, hub, bus, mem, testMetrics(t))

	spec := checks.Spec{Sources: []string{"n1"}, Destinations: []string{"n2"}, Type: "tcp", Plane: "pod", Timeout: 2 * time.Second}
	id, err := runner.Start(context.Background(), spec, testInitiator())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	run := waitForTerminal(t, mem, id)
	if run.Status != "succeeded" {
		t.Fatalf("run.Status = %q, want succeeded", run.Status)
	}

	if cerr := runner.Cancel(context.Background(), id); cerr != nil {
		t.Errorf("Cancel on a terminal run = %v, want nil (no-op)", cerr)
	}
	after, err := mem.GetRun(context.Background(), id)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if after.Status != "succeeded" {
		t.Errorf("run.Status = %q after cancelling a finished run, want succeeded (untouched)", after.Status)
	}
}

// Cancelling an id that names no run at all is ErrNotFound, so httpapi can
// answer 404 rather than pretending it cancelled something.
func TestCancelUnknownRunIsNotFound(t *testing.T) {
	runner := checks.NewRunner(nil, nil, nil, checks.NewMemoryStore(), testMetrics(t))
	err := runner.Cancel(context.Background(), "never-existed")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Cancel(unknown) err = %v, want store.ErrNotFound", err)
	}
}

// Cancel is idempotent: a second Cancel on the same run, once it has already
// gone terminal, is the no-op case above rather than a new error.
func TestCancelTwiceIsIdempotent(t *testing.T) {
	ctrl := newGatedCtrl()
	runner, mem := newCancelRunner(t, ctrl)

	spec := checks.Spec{Sources: []string{"n1"}, Destinations: []string{"n2"}, Type: "tcp", Plane: "pod", Timeout: 5 * time.Second}
	id, err := runner.Start(context.Background(), spec, testInitiator())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctrl.awaitBlocked(t, 1)
	if cerr := runner.Cancel(context.Background(), id); cerr != nil {
		t.Fatalf("first Cancel: %v", cerr)
	}
	waitForTerminal(t, mem, id)
	if cerr := runner.Cancel(context.Background(), id); cerr != nil {
		t.Errorf("second Cancel = %v, want nil", cerr)
	}
}

// MemoryStore must accept running -> cancelled exactly as *store.DB's FinishRun guard does.
func TestMemoryStoreFinishRunAcceptsCancelled(t *testing.T) {
	m := checks.NewMemoryStore()
	ctx := context.Background()
	if _, err := m.CreateRun(ctx, "c1", "tcp", "pod", json.RawMessage(`{}`), "user", "u1", 3); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := m.FinishRun(ctx, "c1", "cancelled", 0, 0); !errors.Is(err, store.ErrWrongState) {
		t.Fatalf("FinishRun(pending -> cancelled) err = %v, want ErrWrongState", err)
	}
	if err := m.MarkRunStarted(ctx, "c1"); err != nil {
		t.Fatalf("MarkRunStarted: %v", err)
	}
	if err := m.FinishRun(ctx, "c1", "cancelled", 1, 2); err != nil {
		t.Fatalf("FinishRun(running -> cancelled): %v", err)
	}
	run, err := m.GetRun(ctx, "c1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != "cancelled" || run.FinishedAt == nil {
		t.Errorf("run = %+v, want status cancelled with FinishedAt set", run)
	}
	if run.PairOK != 1 || run.PairFailed != 2 {
		t.Errorf("pair counts = ok:%d failed:%d, want ok:1 failed:2 (a cancel keeps whatever landed)", run.PairOK, run.PairFailed)
	}
}
