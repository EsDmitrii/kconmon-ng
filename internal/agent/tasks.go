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

// TaskExecutor runs on-demand diagnostic tasks pushed by the controller over WatchTasks; it reuses
// the agent's existing checker instances.
type TaskExecutor struct {
	checkers map[model.CheckType]checker.Checker
	mtr      *checker.MTRChecker
	source   checker.Target
	// own is this agent's listener ports, dialled on a task target that reported none.
	own      checker.PeerPorts
	reporter taskReporter
	sem      chan struct{}
	external ExternalPolicy
	zone     func() string // the effective zone, which re-registration can change; nil means source.Zone
	// current returns the probe set in force, which a config reload replaces; nil means the fields
	// above, fixed at construction.
	current func() (map[model.CheckType]checker.Checker, *checker.MTRChecker, ExternalPolicy)
}

// ExternalPolicy is the agent's gate on destinations that are not registered peers; its zero value
// is a closed gate: Enabled false.
type ExternalPolicy struct {
	Enabled   bool
	Allowlist *checker.Allowlist
	Resolver  checker.Resolver
	// It deliberately does not bound the probe itself: that stays governed by the checker's own
	// timeout.
	Timeout time.Duration
	// MaxTargets is the per-agent ceiling on a CONTINUOUS assignment
	// (checkers.external.maxTargets). Zero means no ceiling.
	MaxTargets int
}

// defaultExternalTCPPort is the port an external TCP probe dials when the request carries no
// explicit port; a peer TCP probe dials the httpPort the peer reported (this agent's own when it
// reported none) because the far end is another kconmon agent.
const defaultExternalTCPPort = 80

/*
externalCapableChecks are the check types that can actually SAY something about an external
destination; the rest are refused rather than silently mislabelled.

UDP is absent, and that is the correction rather than an omission. The UDP checker measures loss by
sending a 4-byte sequence number and counting a packet as received only when the reply's first four
bytes are that number — the kconmon probe server's protocol, which nothing else speaks. Against a
real external host every packet is therefore "lost" and the result is a confident 100% loss that is
a fact about the protocol, not about the network. The CONTINUOUS external path already refuses UDP
for exactly this reason (internal/controller/external.go's validExternalCheckTypes, and the
ExternalCheckSpec comment in the proto); the on-demand path listed it and could never satisfy it.

ICMP and MTR reach an external host honestly, and TCP observes a connect.
*/
var externalCapableChecks = map[model.CheckType]struct{}{
	model.CheckTCP:  {},
	model.CheckICMP: {},
	model.CheckMTR:  {},
}

// NewTaskExecutor builds an executor.
func NewTaskExecutor(
	checkers map[model.CheckType]checker.Checker,
	mtr *checker.MTRChecker,
	source checker.Target, //nolint:gocritic // hugeParam: Target copied intentionally, mirrors scheduler
	own checker.PeerPorts,
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
		own:      own,
		reporter: reporter,
		sem:      make(chan struct{}, maxConcurrent),
		external: external,
	}
}

func (e *TaskExecutor) probes() (map[model.CheckType]checker.Checker, *checker.MTRChecker, ExternalPolicy) {
	if e.current != nil {
		return e.current()
	}
	return e.checkers, e.mtr, e.external
}

func (e *TaskExecutor) sourceZone() string {
	if e.zone != nil {
		return e.zone()
	}
	return e.source.Zone
}

// Handle processes an incoming task; it acquires a concurrency slot and runs the execution in a
// goroutine tied to ctx so it aborts on shutdown and never outlives the root context.
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

// executeOne runs the requested check synchronously and returns the marshalled TaskResult; it never
// blocks on a semaphore and does no reporting, so tests and Handle can both use.
func (e *TaskExecutor) executeOne(ctx context.Context, req *pb.TaskRequest) *pb.TaskResult {
	checkType := model.CheckType(req.GetCheckType())
	checkers, mtr, external := e.probes()

	target := e.targetFromRequest(req)

	// The external-destination gate runs BEFORE any checker is looked up or
	// invoked, so a refused destination never reaches a socket and never reveals
	// which checkers this agent has enabled.
	if ext := req.GetExternalTarget(); ext != nil {
		approved, err := e.approveExternalTarget(ctx, checkType, req, ext, &external)
		if err != nil {
			return e.errorResult(req, err)
		}
		target = approved
	}

	var result model.CheckResult
	switch checkType {
	case model.CheckMTR:
		if mtr == nil {
			return e.errorResult(req, fmt.Errorf("mtr checker not configured"))
		}
		// On-demand MTR deliberately bypasses the scheduler's TryAcquire cooldown: an operator explicitly
		// asked for this trace now.
		result = mtr.Check(ctx, target)
	case model.CheckDNS, model.CheckHTTP:
		// NodeLocal checks ignore the target; run the agent's configured check.
		c, ok := checkers[checkType]
		if !ok {
			return e.errorResult(req, fmt.Errorf("check type %q not enabled on this agent", checkType))
		}
		result = c.Check(ctx, checker.Target{})
	case model.CheckTCP, model.CheckUDP, model.CheckICMP, model.CheckPMTU:
		c, ok := checkers[checkType]
		if !ok {
			return e.errorResult(req, fmt.Errorf("check type %q not enabled on this agent", checkType))
		}
		result = c.Check(ctx, target)
	case model.CheckExternal:
		// "external" is the wrapper type of a CONTINUOUS assignment sweep, not something an operator can
		// ask for on demand.
		return e.errorResult(req, fmt.Errorf("check type %q is not valid for an on-demand task", checkType))
	default:
		return e.errorResult(req, fmt.Errorf("unknown check type %q", req.GetCheckType()))
	}

	// Stamp source/destination labels the same way the scheduler does, so the
	// on-demand result is consistent with periodic results.
	result.Source = e.source.NodeName
	result.SourceZone = e.sourceZone()
	redactHTTPResult(&result)
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

// targetFromRequest builds a checker.Target from the task's target AgentMeta, the controller's full
// projection of the peer: its ports are dialled as reported, with the same zero-means-own fallback as
// the scheduler's peer list.
func (e *TaskExecutor) targetFromRequest(req *pb.TaskRequest) checker.Target {
	t := req.GetTarget()
	return checker.Target{
		AgentID:  t.GetId(),
		NodeName: t.GetNodeName(),
		PodIP:    t.GetPodIp(),
		Zone:     t.GetZone(),
		Port:     portOr(t.GetHttpPort(), e.own.HTTP),
		UDPPort:  portOr(t.GetUdpPort(), e.own.UDP),
	}
}

// approveExternalTarget is the whole external-destination gate; it returns the checker.Target to
// probe only when the destination survived every check.
func (e *TaskExecutor) approveExternalTarget(
	ctx context.Context,
	checkType model.CheckType,
	req *pb.TaskRequest,
	ext *pb.ExternalTarget,
	external *ExternalPolicy,
) (checker.Target, error) {
	// Exactly one of target/external_target is ever populated (see the proto
	// comment). Both set is malformed: refuse rather than guess which
	// destination was meant.
	if req.GetTarget() != nil {
		return checker.Target{}, fmt.Errorf("malformed task: target and external_target are mutually exclusive")
	}

	if !external.Enabled {
		return checker.Target{}, fmt.Errorf(
			"external destinations are not enabled on this agent (set checkers.external.enabled)")
	}

	if _, ok := externalCapableChecks[checkType]; !ok {
		return checker.Target{}, fmt.Errorf(
			"external destinations support tcp, icmp and mtr checks only; udp is excluded because the " +
				"UDP probe measures loss by requiring the destination to echo its own sequence number back, " +
				"which only another kconmon agent does — against any other host it can only ever report 100%% loss")
	}

	port, err := externalPort(checkType, ext.GetPort())
	if err != nil {
		return checker.Target{}, err
	}

	// Bound resolution+authorisation so a hung resolver cannot pin a task slot.
	// The returned target is probed with the caller's ctx, not this one.
	authCtx := ctx
	if external.Timeout > 0 {
		var cancel context.CancelFunc
		authCtx, cancel = context.WithTimeout(ctx, external.Timeout)
		defer cancel()
	}

	// Handed raw to the allowlist, a host:port string fails the literal-IP parse, goes to DNS as a
	// name that cannot resolve.
	host := ext.GetAddress()
	if h, p, splitErr := net.SplitHostPort(host); splitErr == nil {
		if parsed, perr := strconv.ParseUint(p, 10, 16); perr == nil && parsed > 0 {
			host = h
			if ext.GetPort() == 0 {
				port = int(parsed)
			}
		}
	}

	/* Refused HERE, where the request still exists to name it, rather than at the socket.

	   externalPort defaults only TCP (defaultExternalTCPPort, 80); UDP has no port worth inventing, and the address form
	   `host:port` above is the second place a UDP target can get one. If neither supplied a port the
	   check cannot run — the UDP checker would otherwise dial the agent's own echo port on the
	   operator's host (see internal/checker/udp.go) — so the answer is an error naming the missing
	   field, not a probe that reports 100% loss against a port nobody chose. ICMP and MTR are exempt:
	   they carry no port at all. */
	if checkType == model.CheckUDP && port == 0 {
		return checker.Target{}, fmt.Errorf(
			"external udp destination %q has no port: give it one as address host:port or in the port field",
			ext.GetName())
	}

	addrs, err := external.Allowlist.ResolveAllowed(authCtx, external.Resolver, host)
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

	// Every returned address passed the allowlist, so any of them is authorised; the first is the one
	// dialled.
	return checker.Target{
		// NodeName is the only field that becomes a metric/result label, and it
		// carries the target NAME, never the address (see the ExternalTarget
		// proto comment).
		NodeName: ext.GetName(),
		PodIP:    addrs[0].String(),
		Port:     port,
		// Not a peer: the checkers must stop at the transport rather than speak the agent's own
		// protocol to something that is not an agent.
		External: true,
	}, nil
}

// externalPort resolves the port an external probe dials: the requested port when the request
// carries one. Zero means "not decided here" — the address may still carry `host:port`, and the
// caller refuses a UDP target that reaches the socket without a port either way. ICMP and MTR do
// not use a port at all.
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
	// DetailsJson must carry a real CheckResult even for a refusal: the controller returns these
	// bytes verbatim as the diagnostics response body, so an empty payload reaches the Console as
	// "unexpected end of JSON input" and destroys the actual reason for the failure.
	target := e.targetFromRequest(req)
	if ext := req.GetExternalTarget(); ext != nil && req.GetTarget() == nil {
		target = checker.Target{NodeName: ext.GetName()}
	}
	result := model.CheckResult{
		Type:        model.CheckType(req.GetCheckType()),
		Success:     false,
		Source:      e.source.NodeName,
		SourceZone:  e.sourceZone(),
		Destination: target.NodeName,
		DestZone:    target.Zone,
		Error:       err.Error(),
		Timestamp:   time.Now(),
	}
	detailsJSON, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		// A CheckResult with no Details cannot fail to marshal; keep the payload valid regardless.
		detailsJSON = []byte(`{"success":false}`)
	}

	return &pb.TaskResult{
		TaskId:      req.GetTaskId(),
		AgentId:     e.source.AgentID,
		Success:     false,
		Error:       err.Error(),
		DetailsJson: detailsJSON,
		Timestamp:   timestamppb.Now(),
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
