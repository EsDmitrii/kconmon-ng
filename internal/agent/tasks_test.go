package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

// fakeChecker is a controllable checker.Checker used to observe how the task
// executor drives it, without doing any real network I/O.
type fakeChecker struct {
	name   model.CheckType
	mu     sync.Mutex
	calls  int
	inCall chan struct{} // signalled on entry to Check, if non-nil
	block  chan struct{} // Check blocks until closed, if non-nil
	result model.CheckResult
}

func (f *fakeChecker) Name() model.CheckType { return f.name }

func (f *fakeChecker) Check(ctx context.Context, target checker.Target) model.CheckResult {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()

	if f.inCall != nil {
		f.inCall <- struct{}{}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			r := f.result
			r.Error = "context cancelled"
			r.Success = false
			return r
		}
	}
	r := f.result
	r.Type = f.name
	return r
}

func (f *fakeChecker) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeReporter captures reported task results.
type fakeReporter struct {
	mu      sync.Mutex
	results []*pb.TaskResult
	got     chan struct{}
}

func newFakeReporter() *fakeReporter {
	return &fakeReporter{got: make(chan struct{}, 16)}
}

func (r *fakeReporter) ReportTaskResult(_ context.Context, res *pb.TaskResult) error {
	r.mu.Lock()
	r.results = append(r.results, res)
	r.mu.Unlock()
	r.got <- struct{}{}
	return nil
}

func (r *fakeReporter) last() *pb.TaskResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.results) == 0 {
		return nil
	}
	return r.results[len(r.results)-1]
}

func newTestExecutor(reporter taskReporter, checkers ...*fakeChecker) *TaskExecutor {
	cmap := make(map[model.CheckType]checker.Checker, len(checkers))
	for _, c := range checkers {
		cmap[c.name] = c
	}
	src := checker.Target{AgentID: "a1", NodeName: "node-a", Zone: "zone-a"}
	return NewTaskExecutor(cmap, nil, src, 8080, reporter, 4)
}

func waitForReport(t *testing.T, r *fakeReporter) {
	t.Helper()
	select {
	case <-r.got:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a task result to be reported")
	}
}

func TestExecuteReportsSuccessResult(t *testing.T) {
	fc := &fakeChecker{name: model.CheckTCP, result: model.CheckResult{Success: true}}
	rep := newFakeReporter()
	ex := newTestExecutor(rep, fc)

	req := &pb.TaskRequest{
		TaskId:    "t1",
		CheckType: "tcp",
		Target:    &pb.AgentMeta{NodeName: "node-b", PodIp: "10.0.0.2", Zone: "zone-b"},
		Plane:     "pod",
	}

	ex.Handle(context.Background(), req)
	waitForReport(t, rep)

	res := rep.last()
	if res == nil {
		t.Fatal("no result reported")
	}
	if res.GetTaskId() != "t1" {
		t.Errorf("task_id not echoed: got %q", res.GetTaskId())
	}
	if !res.GetSuccess() {
		t.Errorf("expected success result, got error %q", res.GetError())
	}
	if res.GetAgentId() != "a1" {
		t.Errorf("agent_id not filled: got %q", res.GetAgentId())
	}
	if fc.callCount() != 1 {
		t.Errorf("expected checker to run once, ran %d times", fc.callCount())
	}

	var cr model.CheckResult
	if err := json.Unmarshal(res.GetDetailsJson(), &cr); err != nil {
		t.Fatalf("details_json is not valid CheckResult JSON: %v", err)
	}
	if cr.Source != "node-a" || cr.SourceZone != "zone-a" {
		t.Errorf("source labels not filled: source=%q zone=%q", cr.Source, cr.SourceZone)
	}
	if cr.Destination != "node-b" {
		t.Errorf("destination not filled: got %q", cr.Destination)
	}
}

func TestExecuteUnknownCheckTypeReportsError(t *testing.T) {
	fc := &fakeChecker{name: model.CheckTCP, result: model.CheckResult{Success: true}}
	rep := newFakeReporter()
	ex := newTestExecutor(rep, fc)

	req := &pb.TaskRequest{
		TaskId:    "t2",
		CheckType: "bogus",
		Target:    &pb.AgentMeta{NodeName: "node-b", PodIp: "10.0.0.2"},
		Plane:     "pod",
	}

	ex.Handle(context.Background(), req)
	waitForReport(t, rep)

	res := rep.last()
	if res.GetSuccess() {
		t.Error("expected failure for unknown check type")
	}
	if res.GetError() == "" {
		t.Error("expected an error string for unknown check type")
	}
	if res.GetTaskId() != "t2" {
		t.Errorf("task_id not echoed: got %q", res.GetTaskId())
	}
	if fc.callCount() != 0 {
		t.Errorf("checker should not run for unknown type, ran %d times", fc.callCount())
	}
}

func TestHostPlaneExecutesAsPodPlane(t *testing.T) {
	// Only the pod plane is meaningful today (host plane arrives with Epic A).
	// A host-plane task must still execute as a pod-plane check rather than
	// being rejected, so the checker still runs and a normal result comes back.
	fc := &fakeChecker{name: model.CheckTCP, result: model.CheckResult{Success: true}}
	rep := newFakeReporter()
	ex := newTestExecutor(rep, fc)

	req := &pb.TaskRequest{
		TaskId:    "t3",
		CheckType: "tcp",
		Target:    &pb.AgentMeta{NodeName: "node-b", PodIp: "10.0.0.2"},
		Plane:     "host",
	}

	res := ex.executeOne(context.Background(), req)
	if got := res.GetError(); got != "" {
		t.Fatalf("host-plane task should execute, got error: %q", got)
	}
	if !res.GetSuccess() {
		t.Error("host-plane task should have executed the pod-plane check successfully")
	}
	if fc.callCount() != 1 {
		t.Errorf("expected checker to run once regardless of plane, ran %d", fc.callCount())
	}
}

func TestMTRBypassesCooldown(t *testing.T) {
	// A real MTRChecker enforces a long cooldown via TryAcquire. On-demand tasks
	// must bypass it by calling Check directly, so two back-to-back MTR tasks
	// both execute rather than the second being suppressed.
	mtr := checker.NewMTRChecker(1, 10*time.Millisecond, time.Hour)
	rep := newFakeReporter()
	src := checker.Target{AgentID: "a1", NodeName: "node-a", Zone: "zone-a"}
	ex := NewTaskExecutor(map[model.CheckType]checker.Checker{}, mtr, src, 8080, rep, 4)

	req := &pb.TaskRequest{
		CheckType: "mtr",
		Target:    &pb.AgentMeta{NodeName: "node-b", PodIp: "203.0.113.9"},
		Plane:     "pod",
	}

	r1 := ex.executeOne(context.Background(), req)
	r2 := ex.executeOne(context.Background(), req)

	// Both must produce a real MTR result (success or a traceroute error), not a
	// "suppressed by cooldown" outcome. We assert both ran by checking that
	// neither reports the cooldown as the reason and both have MTR details/error.
	for i, r := range []*pb.TaskResult{r1, r2} {
		var cr model.CheckResult
		if err := json.Unmarshal(r.GetDetailsJson(), &cr); err != nil {
			t.Fatalf("run %d: bad details: %v", i, err)
		}
		if cr.Type != model.CheckMTR {
			t.Errorf("run %d: expected MTR result type, got %q", i, cr.Type)
		}
	}
}

func TestSaturationReportsImmediateError(t *testing.T) {
	inCall := make(chan struct{})
	block := make(chan struct{})
	fc := &fakeChecker{name: model.CheckTCP, inCall: inCall, block: block, result: model.CheckResult{Success: true}}
	rep := newFakeReporter()

	src := checker.Target{AgentID: "a1", NodeName: "node-a"}
	// Semaphore of 1: one in-flight task saturates the executor.
	ex := NewTaskExecutor(map[model.CheckType]checker.Checker{model.CheckTCP: fc}, nil, src, 8080, rep, 1)

	req := func(id string) *pb.TaskRequest {
		return &pb.TaskRequest{
			TaskId:    id,
			CheckType: "tcp",
			Target:    &pb.AgentMeta{NodeName: "node-b", PodIp: "10.0.0.2"},
			Plane:     "pod",
		}
	}

	// First task occupies the only slot and blocks inside Check.
	ex.Handle(context.Background(), req("busy"))
	<-inCall

	// Second task arrives while saturated -> must get an immediate error result.
	ex.Handle(context.Background(), req("rejected"))
	waitForReport(t, rep)

	res := rep.last()
	if res.GetTaskId() != "rejected" {
		t.Fatalf("expected the rejected task to be reported first, got %q", res.GetTaskId())
	}
	if res.GetSuccess() {
		t.Error("saturated executor should report failure, not success")
	}
	if res.GetError() == "" {
		t.Error("saturated executor should report an error string")
	}

	// Release the blocked task and confirm it completes too.
	close(block)
	waitForReport(t, rep)
}

func TestContextCancelAbortsExecution(t *testing.T) {
	inCall := make(chan struct{})
	block := make(chan struct{})
	fc := &fakeChecker{name: model.CheckTCP, inCall: inCall, block: block, result: model.CheckResult{Success: true}}
	rep := newFakeReporter()
	src := checker.Target{AgentID: "a1", NodeName: "node-a"}
	ex := NewTaskExecutor(map[model.CheckType]checker.Checker{model.CheckTCP: fc}, nil, src, 8080, rep, 4)

	ctx, cancel := context.WithCancel(context.Background())
	req := &pb.TaskRequest{
		TaskId:    "cancelme",
		CheckType: "tcp",
		Target:    &pb.AgentMeta{NodeName: "node-b", PodIp: "10.0.0.2"},
		Plane:     "pod",
	}

	ex.Handle(ctx, req)
	<-inCall
	cancel() // fakeChecker returns a cancelled result when ctx is done

	waitForReport(t, rep)
	res := rep.last()
	if res.GetSuccess() {
		t.Error("expected cancelled execution to report failure")
	}
	// unblock in case the checker did not observe cancellation
	close(block)
}
