package controller

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/google/uuid"
)

// ErrAgentNotSubscribed is returned by Dispatch when the target agent has no
// active WatchTasks stream, so there is nobody to run the task.
var ErrAgentNotSubscribed = errors.New("agent has no active task subscription")

// TaskManager dispatches on-demand diagnostic tasks to agents over their
// WatchTasks streams and correlates the asynchronous ReportTaskResult callback
// back to the waiting Dispatch caller.
//
// A single mutex guards both maps. Callers must never hold the mutex while
// sending on a channel or blocking, so there is no interaction with the
// registry lock or the gRPC stream goroutines beyond the brief map operations.
type TaskManager struct {
	mu          sync.Mutex
	subscribers map[string]chan *pb.TaskRequest
	pending     map[string]chan *pb.TaskResult
}

func NewTaskManager() *TaskManager {
	return &TaskManager{
		subscribers: make(map[string]chan *pb.TaskRequest),
		pending:     make(map[string]chan *pb.TaskResult),
	}
}

// Subscribe registers an agent's task channel and returns it alongside a
// cleanup func that removes the subscription. The cleanup func is idempotent
// and must be called when the WatchTasks stream ends.
//
// The channel is deliberately never closed. Dispatch reads the channel under
// the mutex, releases it, then sends outside the lock; if cleanup closed the
// channel it could do so between that read and the send, and a send on a
// closed channel panics even inside a select. By only deleting the map entry,
// a Dispatch that races the teardown sends into the abandoned buffered channel
// that nobody reads and simply times out on its context. The abandoned channel
// becomes garbage and is collected — no panic, no leak.
func (tm *TaskManager) Subscribe(agentID string) (tasks <-chan *pb.TaskRequest, cleanup func()) {
	ch := make(chan *pb.TaskRequest, 16)

	tm.mu.Lock()
	tm.subscribers[agentID] = ch
	tm.mu.Unlock()

	var once sync.Once
	cleanup = func() {
		once.Do(func() {
			tm.mu.Lock()
			if cur, ok := tm.subscribers[agentID]; ok && cur == ch {
				delete(tm.subscribers, agentID)
			}
			tm.mu.Unlock()
		})
	}
	return ch, cleanup
}

// Dispatch sends req to agentID and blocks until the agent reports a result,
// the context is cancelled, or the context deadline is exceeded. A task ID is
// generated and stamped on the request so the result can be correlated. The
// pending entry is always removed before returning, so a timed-out task does
// not leak.
func (tm *TaskManager) Dispatch(ctx context.Context, agentID string, req *pb.TaskRequest) (*pb.TaskResult, error) {
	taskID := uuid.NewString()
	req.TaskId = taskID

	resultCh := make(chan *pb.TaskResult, 1)

	tm.mu.Lock()
	sub, ok := tm.subscribers[agentID]
	if !ok {
		tm.mu.Unlock()
		return nil, ErrAgentNotSubscribed
	}
	tm.pending[taskID] = resultCh
	tm.mu.Unlock()

	defer func() {
		tm.mu.Lock()
		delete(tm.pending, taskID)
		tm.mu.Unlock()
	}()

	// Enqueue the task on the agent's buffered channel. Respect context so a
	// slow/full subscriber cannot block the caller past its deadline.
	select {
	case sub <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	select {
	case res := <-resultCh:
		return res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Report delivers a task result to the waiting Dispatch caller. Results for
// unknown or already-completed tasks are dropped with a debug log and never
// block.
func (tm *TaskManager) Report(res *pb.TaskResult) {
	taskID := res.GetTaskId()

	tm.mu.Lock()
	ch, ok := tm.pending[taskID]
	tm.mu.Unlock()

	if !ok {
		slog.Debug("dropping task result for unknown task", "taskId", taskID)
		return
	}

	// resultCh is buffered (cap 1) and Dispatch removes the pending entry
	// before returning, so this send never blocks.
	select {
	case ch <- res:
	default:
		slog.Warn("task result dropped (no waiter or duplicate)", "taskId", taskID)
	}
}

// PendingCount reports the number of in-flight tasks. Intended for tests and
// diagnostics.
func (tm *TaskManager) PendingCount() int {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return len(tm.pending)
}

// SubscriberCount reports the number of agents with an active task
// subscription. Intended for tests and diagnostics.
func (tm *TaskManager) SubscriberCount() int {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return len(tm.subscribers)
}
