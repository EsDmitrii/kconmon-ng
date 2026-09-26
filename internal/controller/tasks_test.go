package controller

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"testing"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
)

func TestTaskManagerDispatchReportRoundtrip(t *testing.T) {
	tm := NewTaskManager()

	sub, unsub := tm.Subscribe("agent-1")
	defer unsub()

	resultCh := make(chan *pb.TaskResult, 1)
	go func() {
		res, err := tm.Dispatch(context.Background(), "agent-1", &pb.TaskRequest{CheckType: "icmp"})
		if err != nil {
			t.Errorf("Dispatch returned error: %v", err)
			resultCh <- nil
			return
		}
		resultCh <- res
	}()

	var dispatched *pb.TaskRequest
	select {
	case dispatched = <-sub:
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not receive dispatched task")
	}

	if dispatched.GetTaskId() == "" {
		t.Fatal("dispatched task has empty task ID")
	}
	if dispatched.GetCheckType() != "icmp" {
		t.Errorf("expected check type icmp, got %q", dispatched.GetCheckType())
	}

	// Agent reports the result back keyed by the same task ID.
	tm.Report(&pb.TaskResult{TaskId: dispatched.GetTaskId(), AgentId: "agent-1", Success: true, DetailsJson: []byte(`{"ok":true}`)})

	select {
	case res := <-resultCh:
		if res == nil {
			t.Fatal("nil result from Dispatch")
		}
		if res.GetTaskId() != dispatched.GetTaskId() {
			t.Errorf("result task ID mismatch: got %q want %q", res.GetTaskId(), dispatched.GetTaskId())
		}
		if !res.GetSuccess() {
			t.Error("expected success result")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Dispatch did not return after Report")
	}
}

// TestTaskManagerDispatchKeepsCallerTaskID pins the id ownership: a caller that
// pre-stamped a task ID (to correlate its dispatch-start event with the outcome)
// keeps it, while an empty one still gets a fresh uuid.
func TestTaskManagerDispatchKeepsCallerTaskID(t *testing.T) {
	tm := NewTaskManager()

	sub, unsub := tm.Subscribe("agent-1")
	defer unsub()

	go func() {
		_, _ = tm.Dispatch(context.Background(), "agent-1",
			&pb.TaskRequest{TaskId: "caller-supplied-id", CheckType: "icmp"})
	}()

	select {
	case dispatched := <-sub:
		if dispatched.GetTaskId() != "caller-supplied-id" {
			t.Errorf("Dispatch overwrote the caller's task ID: got %q", dispatched.GetTaskId())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not receive dispatched task")
	}

	// The result is still correlated under the caller's ID.
	tm.Report(&pb.TaskResult{TaskId: "caller-supplied-id", AgentId: "agent-1", Success: true})
}

func TestTaskManagerDispatchTimeout(t *testing.T) {
	tm := NewTaskManager()

	sub, unsub := tm.Subscribe("agent-1")
	defer unsub()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Drain the subscriber so Dispatch can enqueue, but never Report.
	go func() { <-sub }()

	_, err := tm.Dispatch(ctx, "agent-1", &pb.TaskRequest{CheckType: "icmp"})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}

	// Pending map must not leak the timed-out task.
	if n := tm.PendingCount(); n != 0 {
		t.Errorf("expected 0 pending tasks after timeout, got %d", n)
	}
}

func TestTaskManagerDispatchUnknownAgent(t *testing.T) {
	tm := NewTaskManager()

	_, err := tm.Dispatch(context.Background(), "no-such-agent", &pb.TaskRequest{CheckType: "icmp"})
	if err == nil {
		t.Fatal("expected error dispatching to unsubscribed agent, got nil")
	}
	if !errors.Is(err, ErrAgentNotSubscribed) {
		t.Errorf("expected ErrAgentNotSubscribed, got %v", err)
	}
	if n := tm.PendingCount(); n != 0 {
		t.Errorf("expected 0 pending tasks, got %d", n)
	}
}

func TestTaskManagerSubscribeCleanup(t *testing.T) {
	tm := NewTaskManager()

	_, unsub := tm.Subscribe("agent-1")
	if n := tm.SubscriberCount(); n != 1 {
		t.Fatalf("expected 1 subscriber, got %d", n)
	}

	unsub()
	if n := tm.SubscriberCount(); n != 0 {
		t.Errorf("expected 0 subscribers after cleanup, got %d", n)
	}

	// After cleanup the agent is no longer dispatchable.
	_, err := tm.Dispatch(context.Background(), "agent-1", &pb.TaskRequest{CheckType: "icmp"})
	if !errors.Is(err, ErrAgentNotSubscribed) {
		t.Errorf("expected ErrAgentNotSubscribed after unsubscribe, got %v", err)
	}
}

/*
A second subscriber under an existing agent's id cannot take that agent's tasks away from it.

The agent id on a WatchTasks request is whatever the client sent, and this registry used to hold one
channel per id — last-writer-wins. A second subscriber therefore received the first's dispatches,
and its cleanup, owning the mapped entry, removed the id outright: every later dispatch to a
connected, healthy agent answered ErrAgentNotSubscribed, which the HTTP layer turns into
404 "source agent has no active diagnostics stream". Nothing was logged and the agent still counted
as registered.
*/
func TestTaskManagerSecondSubscriberCannotDisplaceTheFirst(t *testing.T) {
	tm := NewTaskManager()

	victim, victimCleanup := tm.Subscribe("agent-1")
	defer victimCleanup()

	// A second stream arrives under the same id.
	intruder, intruderCleanup := tm.Subscribe("agent-1")

	// The original still receives dispatches.
	go func() {
		select {
		case req := <-victim:
			tm.Report(&pb.TaskResult{TaskId: req.GetTaskId(), AgentId: "agent-1", Success: true})
		case <-time.After(2 * time.Second):
		}
	}()
	// Drain the second stream so its buffer cannot mask the first's delivery.
	go func() {
		select {
		case <-intruder:
		case <-time.After(2 * time.Second):
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, err := tm.Dispatch(ctx, "agent-1", &pb.TaskRequest{CheckType: "icmp"})
	if err != nil {
		t.Fatalf("Dispatch while a second subscriber holds the same id: %v", err)
	}
	if !res.GetSuccess() {
		t.Error("the agent's own result did not come back")
	}

	// And when the second stream ends, the original is still subscribed.
	intruderCleanup()
	if n := tm.SubscriberCount(); n != 1 {
		t.Fatalf("SubscriberCount after the second stream left = %d, want 1", n)
	}

	go func() {
		select {
		case req := <-victim:
			tm.Report(&pb.TaskResult{TaskId: req.GetTaskId(), AgentId: "agent-1", Success: true})
		case <-time.After(2 * time.Second):
		}
	}()
	if _, err := tm.Dispatch(ctx, "agent-1", &pb.TaskRequest{CheckType: "icmp"}); err != nil {
		t.Fatalf("the agent lost its subscription when an unrelated stream disconnected: %v", err)
	}
}

func TestTaskManagerReportUnknownTaskNoBlock(t *testing.T) {
	tm := NewTaskManager()
	// Reporting a result for a task nobody is waiting on must not panic or block.
	tm.Report(&pb.TaskResult{TaskId: "ghost", Success: true})
}

// TestTaskManagerDispatchRacesCleanup drives Dispatch concurrently with the subscription teardown;
// before the fix, cleanup closed the subscriber channel and a Dispatch that read the channel before
// the close but sent after it would panic on a send to a closed channel.
func TestTaskManagerDispatchRacesCleanup(t *testing.T) {
	tm := NewTaskManager()

	const iterations = 100
	for i := range iterations {
		sub, cleanup := tm.Subscribe("agent-1")

		var wg sync.WaitGroup

		// Dispatch racing the teardown. A short deadline bounds the case where
		// the task lands in the abandoned buffer that nobody drains.
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			_, err := tm.Dispatch(ctx, "agent-1", &pb.TaskRequest{CheckType: "icmp"})
			// Any of these outcomes is acceptable; a panic is not, and a
			// non-nil result would come with a nil error.
			if err != nil &&
				!errors.Is(err, ErrAgentNotSubscribed) &&
				!errors.Is(err, context.DeadlineExceeded) &&
				!errors.Is(err, context.Canceled) {
				t.Errorf("iteration %d: unexpected Dispatch error: %v", i, err)
			}
		})

		// A drainer that may or may not receive the task before teardown,
		// mimicking the WatchTasks loop consuming the channel.
		wg.Go(func() {
			select {
			case <-sub:
			case <-time.After(15 * time.Millisecond):
			}
		})

		// Teardown racing the Dispatch.
		wg.Go(func() {
			time.Sleep(time.Duration(rand.Intn(3)) * time.Millisecond) //nolint:gosec // test jitter only
			cleanup()
		})

		wg.Wait()
	}

	// After every subscription has been torn down the manager holds no
	// subscribers and no leaked pending tasks.
	if n := tm.SubscriberCount(); n != 0 {
		t.Errorf("expected 0 subscribers after races, got %d", n)
	}
	if n := tm.PendingCount(); n != 0 {
		t.Errorf("expected 0 pending tasks after races, got %d", n)
	}
}

// A task id leaks to anyone who sees the dispatch (events, logs), so a result is accepted only
// from the agent the task was dispatched to; any other reporter must not complete the dispatch.
func TestTaskManagerReportFromAnotherAgentIsDropped(t *testing.T) {
	tm := NewTaskManager()

	sub, unsub := tm.Subscribe("agent-1")
	defer unsub()

	resultCh := make(chan *pb.TaskResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		res, err := tm.Dispatch(ctx, "agent-1", &pb.TaskRequest{CheckType: "icmp"})
		if err != nil {
			t.Errorf("Dispatch: %v", err)
		}
		resultCh <- res
	}()

	var req *pb.TaskRequest
	select {
	case req = <-sub:
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not receive dispatched task")
	}

	tm.Report(&pb.TaskResult{TaskId: req.GetTaskId(), AgentId: "ext-1-x", Success: true, DetailsJson: []byte(`{"forged":true}`)})
	tm.Report(&pb.TaskResult{TaskId: req.GetTaskId(), Success: true, DetailsJson: []byte(`{"forged":true}`)})
	tm.Report(&pb.TaskResult{TaskId: req.GetTaskId(), AgentId: "agent-1", Success: true, DetailsJson: []byte(`{"real":true}`)})

	select {
	case res := <-resultCh:
		if got := string(res.GetDetailsJson()); got != `{"real":true}` {
			t.Fatalf("Dispatch returned %s, want the dispatched agent's own result", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Dispatch did not return")
	}
}
