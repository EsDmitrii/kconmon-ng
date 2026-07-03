package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// taskReporter delivers a completed task result back to the controller. It is
// the narrow slice of the gRPC client used by the executor, kept as an
// interface so execution can be tested without a live stream.
type taskReporter interface {
	ReportTaskResult(ctx context.Context, res *pb.TaskResult) error
}

// TaskExecutor runs on-demand diagnostic tasks pushed by the controller over
// WatchTasks, out of band from the periodic scheduler. It reuses the agent's
// existing checker instances (their Check methods are safe for concurrent use:
// each holds only immutable config plus per-call connections; the shared
// http.Client in the TCP/HTTP checkers is itself concurrency-safe, and the
// MTRChecker's mutex guards only TryAcquire, not Check). A bounded semaphore
// caps concurrent executions so a burst of API calls cannot fork-bomb the
// agent.
type TaskExecutor struct {
	checkers map[model.CheckType]checker.Checker
	mtr      *checker.MTRChecker
	source   checker.Target
	httpPort int
	reporter taskReporter
	sem      chan struct{}
}

// NewTaskExecutor builds an executor. checkers maps a check type to the agent's
// existing checker instance; mtr is the shared MTR checker (may be nil).
// maxConcurrent bounds simultaneous executions.
func NewTaskExecutor(
	checkers map[model.CheckType]checker.Checker,
	mtr *checker.MTRChecker,
	source checker.Target, //nolint:gocritic // hugeParam: Target copied intentionally, mirrors scheduler
	httpPort int,
	reporter taskReporter,
	maxConcurrent int,
) *TaskExecutor {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &TaskExecutor{
		checkers: checkers,
		mtr:      mtr,
		source:   source,
		httpPort: httpPort,
		reporter: reporter,
		sem:      make(chan struct{}, maxConcurrent),
	}
}

// Handle processes an incoming task. It acquires a concurrency slot and runs
// the execution in a goroutine tied to ctx so it aborts on shutdown and never
// outlives the root context. If the executor is saturated, it reports an
// immediate error result instead of queueing, so a burst of API calls cannot
// pile up unbounded work on the agent.
func (e *TaskExecutor) Handle(ctx context.Context, req *pb.TaskRequest) {
	select {
	case e.sem <- struct{}{}:
		// slot acquired
	default:
		slog.Warn("on-demand task rejected: executor saturated", "taskId", req.GetTaskId(), "checkType", req.GetCheckType())
		e.report(ctx, e.errorResult(req, fmt.Errorf("agent busy: too many concurrent diagnostic tasks")))
		return
	}

	go func() {
		defer func() { <-e.sem }()
		res := e.executeOne(ctx, req)
		e.report(ctx, res)
	}()
}

// executeOne runs the requested check synchronously and returns the marshalled
// TaskResult. It never blocks on a semaphore and does no reporting, so tests
// and Handle can both use it. Unknown check types and marshal failures produce
// an error result rather than a panic.
func (e *TaskExecutor) executeOne(ctx context.Context, req *pb.TaskRequest) *pb.TaskResult {
	checkType := model.CheckType(req.GetCheckType())

	target := e.targetFromRequest(req)

	var result model.CheckResult
	switch checkType {
	case model.CheckMTR:
		if e.mtr == nil {
			return e.errorResult(req, fmt.Errorf("mtr checker not configured"))
		}
		// On-demand MTR deliberately bypasses the scheduler's TryAcquire
		// cooldown: an operator explicitly asked for this trace now, so we call
		// Check directly rather than gating it behind the per-pair cooldown that
		// exists to rate-limit automatic failure-triggered traces.
		result = e.mtr.Check(ctx, target)
	case model.CheckDNS, model.CheckHTTP:
		// NodeLocal checks ignore the target; run the agent's configured check.
		c, ok := e.checkers[checkType]
		if !ok {
			return e.errorResult(req, fmt.Errorf("check type %q not enabled on this agent", checkType))
		}
		result = c.Check(ctx, checker.Target{})
	case model.CheckTCP, model.CheckUDP, model.CheckICMP:
		c, ok := e.checkers[checkType]
		if !ok {
			return e.errorResult(req, fmt.Errorf("check type %q not enabled on this agent", checkType))
		}
		result = c.Check(ctx, target)
	default:
		return e.errorResult(req, fmt.Errorf("unknown check type %q", req.GetCheckType()))
	}

	// Stamp source/destination labels the same way the scheduler does, so the
	// on-demand result is consistent with periodic results.
	result.Source = e.source.NodeName
	result.SourceZone = e.source.Zone
	if checkType != model.CheckDNS && checkType != model.CheckHTTP {
		result.Destination = target.NodeName
		result.DestZone = target.Zone
	}

	detailsJSON, err := json.Marshal(result)
	if err != nil {
		return e.errorResult(req, fmt.Errorf("marshalling result: %w", err))
	}

	return &pb.TaskResult{
		TaskId:      req.GetTaskId(),
		AgentId:     e.source.AgentID,
		Success:     result.Success,
		Error:       result.Error,
		DetailsJson: detailsJSON,
		Timestamp:   timestamppb.Now(),
	}
}

// targetFromRequest builds a checker.Target from the task's target AgentMeta.
// The port mirrors protoToTargets (peer targets dial httpPort for TCP); the UDP
// checker ignores this port and uses its own configured grpcPort, and ICMP/MTR
// ignore the port entirely.
func (e *TaskExecutor) targetFromRequest(req *pb.TaskRequest) checker.Target {
	t := req.GetTarget()
	return checker.Target{
		AgentID:  t.GetId(),
		NodeName: t.GetNodeName(),
		PodIP:    t.GetPodIp(),
		Zone:     t.GetZone(),
		Port:     e.httpPort,
	}
}

// errorResult builds a failed TaskResult for an execution that could not run,
// echoing the task ID and source agent ID so the controller can correlate it.
func (e *TaskExecutor) errorResult(req *pb.TaskRequest, err error) *pb.TaskResult {
	return &pb.TaskResult{
		TaskId:    req.GetTaskId(),
		AgentId:   e.source.AgentID,
		Success:   false,
		Error:     err.Error(),
		Timestamp: timestamppb.Now(),
	}
}

// report delivers a result to the controller, logging any transport failure.
// It uses ctx so a shutdown cancels the in-flight report rather than blocking.
func (e *TaskExecutor) report(ctx context.Context, res *pb.TaskResult) {
	if e.reporter == nil {
		return
	}
	if err := e.reporter.ReportTaskResult(ctx, res); err != nil {
		slog.Warn("reporting task result failed", "taskId", res.GetTaskId(), "error", err)
	}
}
