package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

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
	external ExternalPolicy
}

// ExternalPolicy is the agent's gate on destinations that are not registered
// peers. Its zero value is a closed gate: Enabled false, no allowlist, no
// resolver, which is exactly what an agent whose operator never set
// checkers.external.enabled must behave like.
//
// Allowlist and Resolver are consulted on EVERY external probe, never once at
// startup: a name's resolution changes, so an authorisation taken at boot is
// worth nothing by the time the probe runs.
type ExternalPolicy struct {
	Enabled   bool
	Allowlist *checker.Allowlist
	Resolver  checker.Resolver
	// Timeout bounds resolution and authorisation of one external destination.
	// It deliberately does not bound the probe itself: that stays governed by
	// the checker's own timeout, so turning external checks on cannot silently
	// truncate a long MTR trace. Zero means "inherit the task context".
	Timeout time.Duration
}

// defaultExternalTCPPort is the port an external TCP probe dials when the
// request carries no explicit port. A peer TCP probe dials the agent's own
// httpPort because the far end is another kconmon agent; an external host is
// not, so the agent's port would be meaningless there.
const defaultExternalTCPPort = 80

// externalCapableChecks are the check types that actually probe the destination
// address. dns and http are node-local: they ignore Target entirely and run the
// agent's own configured probes, so pointing one at an external destination
// would report the agent's local results as if they described that destination.
// They are refused rather than silently mislabelled.
var externalCapableChecks = map[model.CheckType]struct{}{
	model.CheckTCP:  {},
	model.CheckUDP:  {},
	model.CheckICMP: {},
	model.CheckMTR:  {},
}

// NewTaskExecutor builds an executor. checkers maps a check type to the agent's
// existing checker instance; mtr is the shared MTR checker (may be nil).
// maxConcurrent bounds simultaneous executions. external carries the
// external-destination gate; its zero value keeps the gate closed.
func NewTaskExecutor(
	checkers map[model.CheckType]checker.Checker,
	mtr *checker.MTRChecker,
	source checker.Target, //nolint:gocritic // hugeParam: Target copied intentionally, mirrors scheduler
	httpPort int,
	reporter taskReporter,
	maxConcurrent int,
	external ExternalPolicy, //nolint:gocritic // hugeParam: policy copied intentionally, it is immutable config
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
		external: external,
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

	// The external-destination gate runs BEFORE any checker is looked up or
	// invoked, so a refused destination never reaches a socket and never reveals
	// which checkers this agent has enabled.
	if ext := req.GetExternalTarget(); ext != nil {
		approved, err := e.approveExternalTarget(ctx, checkType, req, ext)
		if err != nil {
			return e.errorResult(req, err)
		}
		target = approved
	}

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
	case model.CheckExternal:
		// "external" is the wrapper type of a CONTINUOUS assignment sweep, not
		// something an operator can ask for on demand: a single on-demand
		// external probe is expressed as its own check type (tcp/icmp/dns/http)
		// plus an external_target, which is the path above. Refused explicitly
		// rather than left to the default so the intent is on the record.
		return e.errorResult(req, fmt.Errorf("check type %q is not valid for an on-demand task", checkType))
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

// approveExternalTarget is the whole external-destination gate. It returns the
// checker.Target to probe only when the destination survived every check, and
// otherwise an error whose message is safe to ship back to the controller.
//
// Refusal messages name at most an ADDRESS FAMILY. They never contain the
// hostname, the address, or the target name: all three are chosen by whoever
// filed the diagnostic request, and a refusal that echoes them turns the event
// stream into a reflection channel. The detail is logged locally instead, where
// the operator can see it and an attacker cannot.
func (e *TaskExecutor) approveExternalTarget(
	ctx context.Context,
	checkType model.CheckType,
	req *pb.TaskRequest,
	ext *pb.ExternalTarget,
) (checker.Target, error) {
	// Exactly one of target/external_target is ever populated (see the proto
	// comment). Both set is malformed: refuse rather than guess which
	// destination was meant.
	if req.GetTarget() != nil {
		return checker.Target{}, fmt.Errorf("malformed task: target and external_target are mutually exclusive")
	}

	if !e.external.Enabled {
		return checker.Target{}, fmt.Errorf(
			"external destinations are not enabled on this agent (set checkers.external.enabled)")
	}

	if _, ok := externalCapableChecks[checkType]; !ok {
		return checker.Target{}, fmt.Errorf(
			"external destinations support only tcp, udp, icmp and mtr checks")
	}

	port, err := externalPort(checkType, ext.GetPort())
	if err != nil {
		return checker.Target{}, err
	}

	// Bound resolution+authorisation so a hung resolver cannot pin a task slot.
	// The returned target is probed with the caller's ctx, not this one.
	authCtx := ctx
	if e.external.Timeout > 0 {
		var cancel context.CancelFunc
		authCtx, cancel = context.WithTimeout(ctx, e.external.Timeout)
		defer cancel()
	}

	// The Console's one-shot path sends the operator's address VERBATIM --
	// "10.0.0.1:8080" arrives as address "10.0.0.1:8080", port 0 -- while the
	// reconciler splits the port out before it ever reaches the wire. Handed
	// raw to the allowlist, a host:port string fails the literal-IP parse,
	// goes to DNS as a name that cannot resolve, and every legal one-shot
	// external probe with an embedded port is refused as "resolve" (found
	// live by the M4 final-gate smoke). Split here, at the boundary, so
	// every sender's spelling authorises the same way: the split port fills
	// in only when the explicit field is unset, and an unparseable address
	// just flows on to the allowlist, which stays the fail-closed arbiter.
	host := ext.GetAddress()
	if h, p, splitErr := net.SplitHostPort(host); splitErr == nil {
		if parsed, perr := strconv.ParseUint(p, 10, 16); perr == nil && parsed > 0 {
			host = h
			if ext.GetPort() == 0 {
				port = int(parsed)
			}
		}
	}

	addrs, err := e.external.Allowlist.ResolveAllowed(authCtx, e.external.Resolver, host)
	if err != nil {
		slog.Warn("external destination refused",
			"taskId", req.GetTaskId(),
			"checkType", req.GetCheckType(),
			"targetName", ext.GetName(),
			"address", ext.GetAddress(),
			"reason", err,
		)
		return checker.Target{}, err
	}

	// Every returned address passed the allowlist, so any of them is authorised;
	// the first is the one dialled. Handing the approved IP to the checker is
	// what closes the TOCTOU window: TCP/UDP/ICMP/MTR all dial Target.PodIP
	// directly (the TCP checker's follow-up request is an IP-literal URL), so no
	// second resolution happens between the decision and the connection.
	return checker.Target{
		// NodeName is the only field that becomes a metric/result label, and it
		// carries the target NAME, never the address (see the ExternalTarget
		// proto comment).
		NodeName: ext.GetName(),
		PodIP:    addrs[0].String(),
		Port:     port,
	}, nil
}

// externalPort resolves the port an external probe dials: the requested port
// when the request carries one, otherwise the check type's own default.
//
// Only TCP actually reads it. ICMP and MTR ignore the port entirely, and the
// UDP checker dials its own configured gRPC probe port rather than Target.Port,
// so an explicit port on a UDP external task is currently ignored. That is a
// reporting inaccuracy, not a hole: the address dialled is still the one the
// allowlist approved.
func externalPort(checkType model.CheckType, requested uint32) (int, error) {
	if requested > 65535 {
		return 0, fmt.Errorf("external destination port is out of range")
	}
	if requested != 0 {
		return int(requested), nil
	}
	if checkType == model.CheckTCP {
		return defaultExternalTCPPort, nil
	}
	return 0, nil
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
