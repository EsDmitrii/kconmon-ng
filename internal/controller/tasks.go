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

// TaskManager dispatches on-demand diagnostic tasks to agents over their WatchTasks streams and
// correlates the asynchronous ReportTaskResult callback back to the waiting Dispatch caller;
// callers must never hold the mutex while sending on a channel or blocking.
type TaskManager struct {
	mu sync.Mutex
	/* A SET of streams per agent id, not one stream per agent id.

	   The agent id on a WatchTasks request is whatever the client sent, and this channel is what a
	   diagnostic dispatch is delivered on. With one entry per id the map was last-writer-wins: a
	   second caller subscribing under an existing agent's id took delivery of that agent's tasks,
	   and when it disconnected its cleanup — which owned the mapped entry — removed the id
	   altogether, so every later dispatch to a healthy, connected agent answered 404 "source agent
	   has no active diagnostics stream". Nothing was logged on either side and the agent still
	   counted as registered.

	   A set cannot be displaced: a subscriber only ever removes its OWN channel, and Dispatch
	   delivers to all of them. This does not authenticate the channel — the gRPC surface has no
	   authentication at all, which is a deployment-level decision the NetworkPolicy expresses — but
	   it does mean no caller can take a subscription away from the agent that owns it. */
	subscribers map[string]map[chan *pb.TaskRequest]struct{}
	pending     map[string]chan *pb.TaskResult
}

func NewTaskManager() *TaskManager {
	return &TaskManager{
		subscribers: make(map[string]map[chan *pb.TaskRequest]struct{}),
		pending:     make(map[string]chan *pb.TaskResult),
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

// Dispatch sends req to agentID and blocks until the agent reports a result, the context is
// cancelled.
func (tm *TaskManager) Dispatch(ctx context.Context, agentID string, req *pb.TaskRequest) (*pb.TaskResult, error) {
	taskID := req.GetTaskId()
	if taskID == "" {
		taskID = uuid.NewString()
		req.TaskId = taskID
	}

	resultCh := make(chan *pb.TaskResult, 1)

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
	tm.pending[taskID] = resultCh
	tm.mu.Unlock()

	defer func() {
		tm.mu.Lock()
		delete(tm.pending, taskID)
		tm.mu.Unlock()
	}()

	// Enqueue the task on the agent's buffered channel. Respect context so a
	// slow/full subscriber cannot block the caller past its deadline.
	/* Every stream open for this agent id gets the task, and the first agent to answer wins the
	   pending slot. Normally there is exactly one; a reconnect overlaps two for a moment, and the
	   dying one simply never answers. Sending to ALL of them rather than to "the" subscriber is
	   what makes a second subscriber unable to intercept the dispatch. */
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
		// With several replicas this is the shape of a result that came back over a connection the
		// Service routed to a replica that never dispatched the task; its dispatcher will time out.
		slog.Warn("dropping task result for unknown task", "taskId", taskID)
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
