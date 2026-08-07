package checks

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
)

// This file is white-box (package checks, not checks_test): it calls
// unexported methods (execute, runOneRecovered) directly. That is
// deliberate, not a shortcut around runner_test.go's black-box style --
// Start's own runDeadline computation (batches*perPairTimeout + a 30s slack
// floor) cannot be driven below ~31s through the public API, which is far
// too slow for a unit test that specifically wants to observe "runCtx's
// deadline fires mid-dispatch."

// slowFakeCtrl's Diagnose blocks for delay or until ctx is cancelled,
// whichever comes first -- the shape a slow controller/agent produces, which
// is what actually drives runCtx past its own deadline mid-dispatch below.
type slowFakeCtrl struct{ delay time.Duration }

func (f *slowFakeCtrl) Topology(context.Context) (*controllerclient.Topology, error) {
	return &controllerclient.Topology{}, nil
}

func (f *slowFakeCtrl) Diagnose(ctx context.Context, _ controllerclient.DiagnoseRequest, _ time.Duration) (json.RawMessage, error) {
	select {
	case <-time.After(f.delay):
		return json.RawMessage(`{"success":true}`), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// panicOnceFakeCtrl panics on its first Diagnose call, then answers normally
// -- runOneRecovered's recover() test (task-22-brief.md minor c) wants a
// panic that lands inside one pair's dispatch, not a real controller/network
// failure, which the public Diagnose sentinels already cover. panicked is an
// atomic.Bool, not a plain bool: execute dispatches pairs concurrently (up to
// maxConcurrency in flight), and this fake is shared across every pair's
// goroutine.
type panicOnceFakeCtrl struct {
	panicked atomic.Bool
}

func (f *panicOnceFakeCtrl) Topology(context.Context) (*controllerclient.Topology, error) {
	return &controllerclient.Topology{}, nil
}

func (f *panicOnceFakeCtrl) Diagnose(context.Context, controllerclient.DiagnoseRequest, time.Duration) (json.RawMessage, error) {
	if f.panicked.CompareAndSwap(false, true) {
		panic("induced panic for runOneRecovered's test")
	}
	return json.RawMessage(`{"success":true}`), nil
}

// ctxObservingStore wraps a *MemoryStore and records whether ctx was already
// Done (i.e. runCtx-expired) at the moment FinishRun/UpsertRunResult were
// called -- the exact question I-1 (task-22-brief.md) exists to answer: both
// calls must still happen, and succeed, on a context that has NOT itself
// already expired, even after runCtx (the run's own deadline) has.
type ctxObservingStore struct {
	*MemoryStore

	finishRunCalled        bool
	finishRunCtxWasLive    bool
	upsertResultCalled     bool
	upsertResultCtxWasLive bool
}

func (s *ctxObservingStore) FinishRun(ctx context.Context, id, status string, pairOK, pairFailed int32) error {
	s.finishRunCalled = true
	s.finishRunCtxWasLive = ctx.Err() == nil
	return s.MemoryStore.FinishRun(ctx, id, status, pairOK, pairFailed)
}

func (s *ctxObservingStore) UpsertRunResult(ctx context.Context, in store.RunResultInput) (store.RunResult, error) { //nolint:gocritic // hugeParam: matches store.RunStore.UpsertRunResult's own signature
	s.upsertResultCalled = true
	if ctx.Err() == nil {
		s.upsertResultCtxWasLive = true
	}
	return s.MemoryStore.UpsertRunResult(ctx, in)
}

// A run whose runCtx deadline fires WHILE a pair is still dispatching (a slow
// controller/agent -- exactly the case runDeadline's own slack cannot always
// absorb) must still reach FinishRun and end up in a terminal status, not
// stuck "running" forever, since nothing reaps a run row (task-22-brief.md
// I-1).
func TestExecuteFinishesRunAfterRunCtxDeadlineFiresMidDispatch(t *testing.T) {
	m := metrics.New("kconmon_ng_test_i1", prometheus.NewRegistry())
	bus := cache.NewInProcessBus()
	hub := ws.NewHub(bus, m)
	st := &ctxObservingStore{MemoryStore: NewMemoryStore()}
	ctrl := &slowFakeCtrl{delay: 300 * time.Millisecond}
	r := NewRunner(ctrl, hub, bus, st, m)

	spec := Spec{Sources: []string{"n1"}, Destinations: []string{"n2"}, Type: "tcp", Plane: "pod", Timeout: 1 * time.Second}
	pairs := []Pair{{Source: "n1", Destination: "n2"}}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	run, err := st.CreateRun(context.Background(), "run-i1", spec.Type, spec.Plane, specJSON, "user", "u1", int32(len(pairs)))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// runCtx's deadline (30ms) fires well before the fake controller's 300ms
	// dispatch delay does -- exactly the "runCtx expires mid-flight" case.
	runCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	topic := ws.RunTopic(run.ID)
	topicOpen := hub.OpenTopic(runCtx, topic)

	r.execute(runCtx, cancel, run.ID, pairs, &spec, 1*time.Second, topicOpen)

	if !st.finishRunCalled {
		t.Fatal("FinishRun was never called")
	}
	if !st.finishRunCtxWasLive {
		t.Error("FinishRun's ctx was already Done -- it must run on a context derived from context.WithoutCancel(runCtx), not runCtx itself")
	}
	if !st.upsertResultCalled {
		t.Fatal("UpsertRunResult was never called")
	}
	if !st.upsertResultCtxWasLive {
		t.Error("UpsertRunResult's ctx was already Done -- it must run on a context derived from context.WithoutCancel(runCtx), not runCtx itself")
	}

	got, err := st.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status == "pending" || got.Status == "running" {
		t.Errorf("run.Status = %q, want a terminal status even though runCtx expired mid-flight", got.Status)
	}
}

// A panic inside one pair's dispatch must not crash the process or hang
// wg.Wait() forever waiting on a goroutine that never reached wg.Done(); it
// must be recorded as a failed pair, logged, and the run must still finish
// (task-22-brief.md minor c).
func TestRunOneRecoveredSurvivesAPanicAndRecordsTheirPairFailed(t *testing.T) {
	m := metrics.New("kconmon_ng_test_c", prometheus.NewRegistry())
	bus := cache.NewInProcessBus()
	hub := ws.NewHub(bus, m)
	st := NewMemoryStore()
	ctrl := &panicOnceFakeCtrl{}
	r := NewRunner(ctrl, hub, bus, st, m)

	spec := Spec{Sources: []string{"n1"}, Destinations: []string{"n2", "n3"}, Type: "tcp", Plane: "pod", Timeout: 2 * time.Second}
	pairs := []Pair{{Source: "n1", Destination: "n2"}, {Source: "n1", Destination: "n3"}}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	run, err := st.CreateRun(context.Background(), "run-c", spec.Type, spec.Plane, specJSON, "user", "u1", int32(len(pairs)))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	runCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	topic := ws.RunTopic(run.ID)
	topicOpen := hub.OpenTopic(runCtx, topic)

	// If runOneRecovered did not recover the panic, it would surface on the
	// unexported worker goroutine execute launches internally -- an
	// unrecovered panic on ANY goroutine crashes the whole test binary, not
	// just this test, which is itself the strongest possible signal that the
	// recover() is missing or broken.
	r.execute(runCtx, cancel, run.ID, pairs, &spec, 2*time.Second, topicOpen)

	got, err := st.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status != "partial" {
		t.Fatalf("run.Status = %q, want partial (one panicked pair failed, one succeeded)", got.Status)
	}
	if got.PairOK != 1 || got.PairFailed != 1 {
		t.Errorf("run pair counts = ok:%d failed:%d, want ok:1 failed:1", got.PairOK, got.PairFailed)
	}
}
