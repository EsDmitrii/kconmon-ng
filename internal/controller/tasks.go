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

// ErrLeadershipLost is returned by Dispatch when this replica is demoted while the task is in
// flight: the agent's result now goes to the new leader, which does not know the task.
var ErrLeadershipLost = errors.New("leadership lost while the task was in flight")

// TaskManager dispatches on-demand diagnostic tasks to agents over their WatchTasks streams and
// correlates the asynchronous ReportTaskResult callback back to the waiting Dispatch caller;
// callers must never hold the mutex while sending on a channel or blocking.
type TaskManager struct {
	mu sync.Mutex
	// A set of streams per agent id: the id is client-supplied, so a second subscriber under it must
	// not displace the agent's own stream. A subscriber removes only its own channel, and Dispatch
	// delivers to all of them.
	subscribers map[string]map[chan *pb.TaskRequest]struct{}
	pending     map[string]pendingTask
}

// pendingTask remembers WHICH agent a task went to: the task id is published in events, so a
// result is only accepted from that agent.
type pendingTask struct {
	agentID string
	done    chan taskOutcome // cap 1; the first outcome wins
}

type taskOutcome struct {
	res *pb.TaskResult
	err error
}

func NewTaskManager() *TaskManager {
	return &TaskManager{
		subscribers: make(map[string]map[chan *pb.TaskRequest]struct{}),
		pending:     make(map[string]pendingTask),
	}
}

// Subscribe registers an agent's task channel and returns it alongside a cleanup func that removes
// the subscription; the cleanup func is idempotent and must be called when the WatchTasks stream
// ends.
func (tm *TaskManager) Subscribe(agentID string) (tasks <-chan *pb.TaskRequest, cleanup func()) {
	ch := make(chan *pb.TaskRequest, 16)

	tm.mu.Lock()
	if tm.subscribers[agentID] == nil {
		tm.subscribers[agentID] = make(map[chan *pb.TaskRequest]struct{}, 1)
	}

	tm.subscribers[agentID][ch] = struct{}{}
	tm.mu.Unlock()

	var once sync.Once
	cleanup = func() {
		once.Do(func() {
			tm.mu.Lock()
			if set, ok := tm.subscribers[agentID]; ok {
				delete(set, ch)
				if len(set) == 0 {
					delete(tm.subscribers, agentID)
				}
			}
			tm.mu.Unlock()
		})
	}
	return ch, cleanup
}

// Dispatch sends req to agentID and blocks until the agent reports a result, ctx ends, or FailAll
// ends it (ErrLeadershipLost on demotion).
func (tm *TaskManager) Dispatch(ctx context.Context, agentID string, req *pb.TaskRequest) (*pb.TaskResult, error) {
	taskID := req.GetTaskId()
	if taskID == "" {
		taskID = uuid.NewString()
		req.TaskId = taskID
	}

	done := make(chan taskOutcome, 1)

	tm.mu.Lock()
	set := tm.subscribers[agentID]
	if len(set) == 0 {
		tm.mu.Unlock()
		return nil, ErrAgentNotSubscribed
	}
	subs := make([]chan *pb.TaskRequest, 0, len(set))
	for ch := range set {
		subs = append(subs, ch)
	}
	tm.pending[taskID] = pendingTask{agentID: agentID, done: done}
	tm.mu.Unlock()

	defer func() {
		tm.mu.Lock()
		delete(tm.pending, taskID)
		tm.mu.Unlock()
	}()

	// Every stream open for this agent id gets the task, and the first answer wins the pending slot.
	// Normally there is one; a reconnect overlaps two for a moment, and the dying one never answers.
	sent := false
	for _, sub := range subs {
		select {
		case sub <- req:
			sent = true
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			// A full buffer on one stream must not cost the others their copy.
		}
	}
	if !sent {
		return nil, ErrAgentNotSubscribed
	}

	select {
	case out := <-done:
		return out.res, out.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Report delivers a task result to the waiting Dispatch caller. Results for
// unknown or already-completed tasks are dropped with a warning and never
// block.
func (tm *TaskManager) Report(res *pb.TaskResult) {
	taskID := res.GetTaskId()

	tm.mu.Lock()
	p, ok := tm.pending[taskID]
	tm.mu.Unlock()

	if !ok {
		// With several replicas this is the shape of a result that came back over a connection the
		// Service routed to a replica that never dispatched the task; its dispatcher will time out.
		slog.Warn("dropping task result for unknown task", "taskId", taskID)
		return
	}
	if res.GetAgentId() != p.agentID {
		slog.Warn("dropping task result from an agent the task was not dispatched to",
			"taskId", taskID, "reportedBy", res.GetAgentId(), "dispatchedTo", p.agentID)
		return
	}

	// done is buffered (cap 1) and Dispatch removes the pending entry
	// before returning, so this send never blocks.
	select {
	case p.done <- taskOutcome{res: res}:
	default:
		slog.Warn("task result dropped (no waiter or duplicate)", "taskId", taskID)
	}
}

// FailAll ends every in-flight Dispatch with err; used on demotion, where no result can arrive.
func (tm *TaskManager) FailAll(err error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	for _, p := range tm.pending {
		select {
		case p.done <- taskOutcome{err: err}:
		default:
		}
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
