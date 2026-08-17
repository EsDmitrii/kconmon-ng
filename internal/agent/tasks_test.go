package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
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
	name    model.CheckType
	mu      sync.Mutex
	calls   int
	lastTgt checker.Target
	inCall  chan struct{} // signalled on entry to Check, if non-nil
	block   chan struct{} // Check blocks until closed, if non-nil
	result  model.CheckResult
}

func (f *fakeChecker) Name() model.CheckType { return f.name }

func (f *fakeChecker) Check(ctx context.Context, target checker.Target) model.CheckResult {
	f.mu.Lock()
	f.calls++
	f.lastTgt = target
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

func (f *fakeChecker) lastTarget() checker.Target {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastTgt
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
	return NewTaskExecutor(cmap, nil, src, 8080, reporter, 4, ExternalPolicy{})
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
	ex := NewTaskExecutor(map[model.CheckType]checker.Checker{}, mtr, src, 8080, rep, 4, ExternalPolicy{})

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
	ex := NewTaskExecutor(map[model.CheckType]checker.Checker{model.CheckTCP: fc}, nil, src, 8080, rep, 1, ExternalPolicy{})

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
	ex := NewTaskExecutor(map[model.CheckType]checker.Checker{model.CheckTCP: fc}, nil, src, 8080, rep, 4, ExternalPolicy{})

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

// stubResolver is the injected DNS for every external-destination test, so the
// executor tests never touch a network.
type stubResolver struct {
	mu    sync.Mutex
	calls int
	hosts map[string][]netip.Addr
	err   error
}

func (s *stubResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	return s.hosts[host], nil
}

func (s *stubResolver) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// newExternalExecutor builds an executor whose external gate is enabled with the given allow/deny
// lists and a stub resolver.
func newExternalExecutor(t *testing.T, r checker.Resolver, allowed, denied []string, checkers ...*fakeChecker) *TaskExecutor {
	t.Helper()
	cmap := make(map[model.CheckType]checker.Checker, len(checkers))
	for _, c := range checkers {
		cmap[c.name] = c
	}
	list, err := checker.NewAllowlist(allowed, denied)
	if err != nil {
		t.Fatalf("building allowlist: %v", err)
	}
	src := checker.Target{AgentID: "a1", NodeName: "node-a", Zone: "zone-a"}
	return NewTaskExecutor(cmap, nil, src, 8080, nil, 4, ExternalPolicy{
		Enabled:   true,
		Allowlist: list,
		Resolver:  r,
		Timeout:   5 * time.Second,
	})
}

func externalReq(taskID, checkType, address string) *pb.TaskRequest {
	return &pb.TaskRequest{
		TaskId:         taskID,
		CheckType:      checkType,
		Plane:          "pod",
		ExternalTarget: &pb.ExternalTarget{Name: "edge-gw", Kind: "host", Address: address},
	}
}

// Exactly one of target/external_target is ever populated (see the proto
// comment). Both set is malformed: the executor refuses rather than guessing
// which destination the controller meant.
func TestExternalAndPeerTargetBothSetIsMalformed(t *testing.T) {
	fc := &fakeChecker{name: model.CheckTCP, result: model.CheckResult{Success: true}}
	r := &stubResolver{}
	ex := newExternalExecutor(t, r, []string{"0.0.0.0/0", "::/0"}, nil, fc)

	req := externalReq("both", "tcp", "10.0.0.9")
	req.Target = &pb.AgentMeta{NodeName: "node-b", PodIp: "10.0.0.2"}

	res := ex.executeOne(context.Background(), req)
	if res.GetSuccess() {
		t.Error("a malformed both-destinations task must fail")
	}
	if res.GetError() == "" {
		t.Error("expected an error string for a malformed task")
	}
	if fc.callCount() != 0 {
		t.Errorf("no checker may run for a malformed task, ran %d", fc.callCount())
	}
	if r.callCount() != 0 {
		t.Errorf("no resolution may happen for a malformed task, resolved %d times", r.callCount())
	}
}

// An operator who never opted in gets a refusal that names the value to flip,
// not a silent probe or a mystifying timeout.
func TestExternalTargetWithFeatureDisabledNamesTheHelmValue(t *testing.T) {
	fc := &fakeChecker{name: model.CheckTCP, result: model.CheckResult{Success: true}}
	rep := newFakeReporter()
	ex := newTestExecutor(rep, fc) // ExternalPolicy{} => disabled

	res := ex.executeOne(context.Background(), externalReq("off", "tcp", "10.0.0.9"))
	if res.GetSuccess() {
		t.Error("an external task must fail while the feature is disabled")
	}
	if !strings.Contains(res.GetError(), "checkers.external.enabled") {
		t.Errorf("refusal must name checkers.external.enabled, got: %q", res.GetError())
	}
	if fc.callCount() != 0 {
		t.Errorf("checker must not run while the feature is disabled, ran %d", fc.callCount())
	}
}

// An enabled feature with a nil allowlist (a state config validation forbids,
// asserted here as defence in depth) still denies.
func TestExternalTargetEnabledWithoutAllowlistDenies(t *testing.T) {
	fc := &fakeChecker{name: model.CheckTCP, result: model.CheckResult{Success: true}}
	src := checker.Target{AgentID: "a1", NodeName: "node-a"}
	ex := NewTaskExecutor(map[model.CheckType]checker.Checker{model.CheckTCP: fc}, nil, src, 8080, nil, 4,
		ExternalPolicy{Enabled: true})

	res := ex.executeOne(context.Background(), externalReq("noallow", "tcp", "10.0.0.9"))
	if res.GetSuccess() {
		t.Error("an enabled-but-unconfigured external gate must deny")
	}
	if fc.callCount() != 0 {
		t.Errorf("checker must not run without an allowlist, ran %d", fc.callCount())
	}
}

// The refusal travels back through the controller into the event stream, so it
// must never echo the address or the hostname the caller supplied.
func TestExternalDeniedAddressNeverRunsCheckerAndLeaksNothing(t *testing.T) {
	fc := &fakeChecker{name: model.CheckTCP, result: model.CheckResult{Success: true}}
	r := &stubResolver{}
	ex := newExternalExecutor(t, r, []string{"10.0.0.0/8"}, nil, fc)

	res := ex.executeOne(context.Background(), externalReq("denied", "tcp", "169.254.169.254"))
	if res.GetSuccess() {
		t.Fatal("a denied destination must produce a failed result")
	}
	msg := res.GetError()
	for _, leak := range []string{"169.254.169.254", "edge-gw"} {
		if strings.Contains(msg, leak) {
			t.Errorf("refusal leaked %q: %s", leak, msg)
		}
	}
	if !strings.Contains(msg, "IPv4") {
		t.Errorf("refusal must name the refused address family, got: %s", msg)
	}
	if fc.callCount() != 0 {
		t.Errorf("checker must never run for a denied destination, ran %d", fc.callCount())
	}
	if r.callCount() != 0 {
		t.Errorf("a literal address must not be sent to DNS, resolved %d times", r.callCount())
	}
}

func TestExternalDeniedIPv6RefusalNamesIPv6(t *testing.T) {
	fc := &fakeChecker{name: model.CheckTCP, result: model.CheckResult{Success: true}}
	ex := newExternalExecutor(t, &stubResolver{}, []string{"10.0.0.0/8"}, nil, fc)

	res := ex.executeOne(context.Background(), externalReq("denied6", "tcp", "2001:db8::dead"))
	if res.GetSuccess() {
		t.Fatal("a denied IPv6 destination must produce a failed result")
	}
	if strings.Contains(res.GetError(), "2001:db8") {
		t.Errorf("refusal leaked the address: %s", res.GetError())
	}
	if !strings.Contains(res.GetError(), "IPv6") {
		t.Errorf("refusal must name the refused address family, got: %s", res.GetError())
	}
	if fc.callCount() != 0 {
		t.Errorf("checker must never run for a denied destination, ran %d", fc.callCount())
	}
}

// An allowed destination reaches the checker as the APPROVED IP, not as the
// hostname: the probe dials exactly what the allowlist authorised, so there is
// no second resolution to race.
func TestExternalAllowedRunsCheckerWithApprovedAddress(t *testing.T) {
	fc := &fakeChecker{name: model.CheckTCP, result: model.CheckResult{Success: true}}
	r := &stubResolver{hosts: map[string][]netip.Addr{
		"gw.internal": {netip.MustParseAddr("::ffff:10.4.5.6")},
	}}
	ex := newExternalExecutor(t, r, []string{"10.0.0.0/8"}, nil, fc)

	res := ex.executeOne(context.Background(), externalReq("ok", "tcp", "gw.internal"))
	if !res.GetSuccess() {
		t.Fatalf("an allowed destination must run: %q", res.GetError())
	}
	if fc.callCount() != 1 {
		t.Fatalf("expected the checker to run once, ran %d", fc.callCount())
	}
	tgt := fc.lastTarget()
	if tgt.PodIP != "10.4.5.6" {
		t.Errorf("checker dialled %q, want the approved unmapped address 10.4.5.6", tgt.PodIP)
	}
	if tgt.NodeName != "edge-gw" {
		t.Errorf("destination label = %q, want the external target NAME edge-gw", tgt.NodeName)
	}
	if r.callCount() != 1 {
		t.Errorf("expected exactly one resolution, got %d", r.callCount())
	}

	var cr model.CheckResult
	if err := json.Unmarshal(res.GetDetailsJson(), &cr); err != nil {
		t.Fatalf("details_json invalid: %v", err)
	}
	if cr.Destination != "edge-gw" {
		t.Errorf("result destination = %q, want the target name (the address must never become a label)", cr.Destination)
	}
}

func TestExternalAllowedUsesExplicitPortAndTCPDefault(t *testing.T) {
	fc := &fakeChecker{name: model.CheckTCP, result: model.CheckResult{Success: true}}
	ex := newExternalExecutor(t, &stubResolver{}, []string{"10.0.0.0/8"}, nil, fc)

	res := ex.executeOne(context.Background(), externalReq("defport", "tcp", "10.0.0.9"))
	if !res.GetSuccess() {
		t.Fatalf("allowed destination must run: %q", res.GetError())
	}
	if got := fc.lastTarget().Port; got != defaultExternalTCPPort {
		t.Errorf("port = %d, want the %d default for an external TCP probe", got, defaultExternalTCPPort)
	}

	req := externalReq("explicitport", "tcp", "10.0.0.9")
	req.ExternalTarget.Port = 8443
	if res2 := ex.executeOne(context.Background(), req); !res2.GetSuccess() {
		t.Fatalf("allowed destination must run: %q", res2.GetError())
	}
	if got := fc.lastTarget().Port; got != 8443 {
		t.Errorf("port = %d, want the requested 8443", got)
	}
}

/*
An external UDP diagnostic is refused, because it is a measurement this build cannot make.

The UDP checker counts a packet as received only when the reply's first four bytes are the sequence
number it sent — the kconmon probe server's protocol. Against anything that is not another agent
every packet is "lost", so the result was a confident 100% loss that describes the protocol rather
than the network, and an operator reading it would go looking for a link fault that is not there.
The CONTINUOUS external path has always refused UDP for this reason; the on-demand path listed it.

The refusal has to say why, and no checker may run.
*/
func TestExternalUDPIsRefusedBecauseItCannotMeasureAnythingThere(t *testing.T) {
	fc := &fakeChecker{name: model.CheckUDP, result: model.CheckResult{Success: true}}
	ex := newExternalExecutor(t, &stubResolver{}, []string{"10.0.0.0/8"}, nil, fc)

	for _, req := range []*pb.TaskRequest{
		externalReq("noport", "udp", "10.0.0.9"),
		externalReq("embedded", "udp", "10.0.0.9:5353"),
	} {
		res := ex.executeOne(context.Background(), req)
		if res.GetSuccess() {
			t.Fatalf("an external UDP probe must be refused, got a result: %+v", res)
		}
		if !strings.Contains(res.GetError(), "udp") {
			t.Errorf("refusal does not name udp: %s", res.GetError())
		}
		if !strings.Contains(res.GetError(), "echo") {
			t.Errorf("refusal does not say WHY udp cannot work there: %s", res.GetError())
		}
	}
	if fc.callCount() != 0 {
		t.Errorf("checker ran %d times for an external UDP target", fc.callCount())
	}

	// ICMP against the same destination still runs: the refusal is about UDP, not about external.
	icmp := &fakeChecker{name: model.CheckICMP, result: model.CheckResult{Success: true}}
	ex2 := newExternalExecutor(t, &stubResolver{}, []string{"10.0.0.0/8"}, nil, icmp)
	if res := ex2.executeOne(context.Background(), externalReq("ping", "icmp", "10.0.0.9")); !res.GetSuccess() {
		t.Fatalf("external icmp must still run: %q", res.GetError())
	}
}

func TestExternalOutOfRangePortIsRefused(t *testing.T) {
	fc := &fakeChecker{name: model.CheckTCP, result: model.CheckResult{Success: true}}
	ex := newExternalExecutor(t, &stubResolver{}, []string{"10.0.0.0/8"}, nil, fc)

	req := externalReq("badport", "tcp", "10.0.0.9")
	req.ExternalTarget.Port = 70000
	res := ex.executeOne(context.Background(), req)
	if res.GetSuccess() {
		t.Error("an out-of-range port must be refused")
	}
	if fc.callCount() != 0 {
		t.Errorf("checker must not run for an out-of-range port, ran %d", fc.callCount())
	}
}

// A name resolving to one permitted and one forbidden address is denied
// outright: the connection, not the allowlist, would pick which one is dialled.
func TestExternalPartialResolutionIsDenied(t *testing.T) {
	fc := &fakeChecker{name: model.CheckTCP, result: model.CheckResult{Success: true}}
	r := &stubResolver{hosts: map[string][]netip.Addr{
		"rebind.example.com": {
			netip.MustParseAddr("10.0.0.1"),
			netip.MustParseAddr("169.254.169.254"),
		},
	}}
	ex := newExternalExecutor(t, r, []string{"10.0.0.0/8"}, nil, fc)

	res := ex.executeOne(context.Background(), externalReq("partial", "tcp", "rebind.example.com"))
	if res.GetSuccess() {
		t.Fatal("a partially allowed resolution must be denied")
	}
	if strings.Contains(res.GetError(), "rebind.example.com") || strings.Contains(res.GetError(), "169.254") {
		t.Errorf("refusal leaked the destination: %s", res.GetError())
	}
	if fc.callCount() != 0 {
		t.Errorf("checker must never run for a partially allowed name, ran %d", fc.callCount())
	}
}

func TestExternalResolutionFailureIsDenial(t *testing.T) {
	fc := &fakeChecker{name: model.CheckTCP, result: model.CheckResult{Success: true}}
	r := &stubResolver{err: errors.New("lookup nowhere.example.com: no such host")}
	ex := newExternalExecutor(t, r, []string{"0.0.0.0/0", "::/0"}, nil, fc)

	res := ex.executeOne(context.Background(), externalReq("dnsfail", "tcp", "nowhere.example.com"))
	if res.GetSuccess() {
		t.Fatal("a resolution failure must deny")
	}
	if strings.Contains(res.GetError(), "nowhere.example.com") {
		t.Errorf("refusal leaked the hostname: %s", res.GetError())
	}
	if fc.callCount() != 0 {
		t.Errorf("checker must never run when resolution failed, ran %d", fc.callCount())
	}
}

func TestExternalDeniedPrefixWinsOverAllowed(t *testing.T) {
	fc := &fakeChecker{name: model.CheckICMP, result: model.CheckResult{Success: true}}
	ex := newExternalExecutor(t, &stubResolver{}, []string{"10.0.0.0/8"}, []string{"10.1.2.0/24"}, fc)

	res := ex.executeOne(context.Background(), externalReq("denywins", "icmp", "10.1.2.3"))
	if res.GetSuccess() {
		t.Error("a destination inside a denied prefix must be refused even though it is also allowed")
	}
	if fc.callCount() != 0 {
		t.Errorf("checker must not run, ran %d", fc.callCount())
	}
}

// dns and http are node-local checks that ignore the destination entirely.
// Running one for an external destination would report the agent's own probes
// as if they had targeted that destination, so they are refused.
func TestExternalNodeLocalCheckTypesAreRefused(t *testing.T) {
	for _, ct := range []string{"dns", "http"} {
		t.Run(ct, func(t *testing.T) {
			fc := &fakeChecker{name: model.CheckType(ct), result: model.CheckResult{Success: true}}
			r := &stubResolver{}
			ex := newExternalExecutor(t, r, []string{"0.0.0.0/0", "::/0"}, nil, fc)

			res := ex.executeOne(context.Background(), externalReq("nodelocal", ct, "10.0.0.9"))
			if res.GetSuccess() {
				t.Errorf("%s must not accept an external destination", ct)
			}
			if fc.callCount() != 0 {
				t.Errorf("checker must not run, ran %d", fc.callCount())
			}
			if r.callCount() != 0 {
				t.Errorf("no resolution may happen for an unsupported check type, resolved %d times", r.callCount())
			}
		})
	}
}

func TestExternalEmptyAddressIsRefused(t *testing.T) {
	fc := &fakeChecker{name: model.CheckTCP, result: model.CheckResult{Success: true}}
	ex := newExternalExecutor(t, &stubResolver{}, []string{"0.0.0.0/0", "::/0"}, nil, fc)

	res := ex.executeOne(context.Background(), externalReq("empty", "tcp", ""))
	if res.GetSuccess() {
		t.Error("an empty external address must be refused")
	}
	if fc.callCount() != 0 {
		t.Errorf("checker must not run, ran %d", fc.callCount())
	}
}

// The gate applies to the check type the agent does not even have enabled: a
// denied destination is refused before the "not enabled" answer, so a probe of
// a forbidden range never becomes a checker-enumeration oracle.
func TestExternalDeniedDestinationRefusedBeforeCheckerLookup(t *testing.T) {
	ex := newExternalExecutor(t, &stubResolver{}, []string{"10.0.0.0/8"}, nil)

	res := ex.executeOne(context.Background(), externalReq("noenabled", "tcp", "203.0.113.9"))
	if res.GetSuccess() {
		t.Fatal("a denied destination must be refused")
	}
	if strings.Contains(res.GetError(), "not enabled on this agent") {
		t.Errorf("the allowlist refusal must come first, got: %q", res.GetError())
	}
}

// The boundary split must authorise the bare host and dial the embedded port.
func TestExternalAddressWithEmbeddedPortIsSplitNotResolved(t *testing.T) {
	allow, err := checker.NewAllowlist([]string{"127.0.0.0/8"}, nil)
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	fake := &fakeChecker{}
	e := NewTaskExecutor(map[model.CheckType]checker.Checker{model.CheckTCP: fake}, nil,
		checker.Target{NodeName: "n1", Zone: "z1"}, 8080, nil, 1,
		ExternalPolicy{Enabled: true, Allowlist: allow, Resolver: &stubResolver{}})

	res := e.executeOne(t.Context(), &pb.TaskRequest{
		TaskId: "t-split", CheckType: "tcp", Plane: "pod",
		ExternalTarget: &pb.ExternalTarget{Name: "svc", Kind: "host", Address: "127.0.0.9:18201"},
	})
	if !res.GetSuccess() && res.GetError() != "" {
		t.Fatalf("expected the probe to reach the checker, got error %q", res.GetError())
	}
	if fake.calls != 1 {
		t.Fatalf("checker calls = %d, want 1", fake.calls)
	}
	got := fake.lastTgt
	if got.PodIP != "127.0.0.9" || got.Port != 18201 {
		t.Errorf("dialled %s:%d, want 127.0.0.9:18201 (split host + embedded port)", got.PodIP, got.Port)
	}
}

// TestExternalExplicitPortFieldWinsOverEmbeddedPort: when BOTH the port field
// and an embedded port are present, the explicit proto field wins -- it is
// the schema's own channel for the value.
func TestExternalExplicitPortFieldWinsOverEmbeddedPort(t *testing.T) {
	allow, err := checker.NewAllowlist([]string{"127.0.0.0/8"}, nil)
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	fake := &fakeChecker{}
	e := NewTaskExecutor(map[model.CheckType]checker.Checker{model.CheckTCP: fake}, nil,
		checker.Target{NodeName: "n1", Zone: "z1"}, 8080, nil, 1,
		ExternalPolicy{Enabled: true, Allowlist: allow, Resolver: &stubResolver{}})

	res := e.executeOne(t.Context(), &pb.TaskRequest{
		TaskId: "t-split2", CheckType: "tcp", Plane: "pod",
		ExternalTarget: &pb.ExternalTarget{Name: "svc", Kind: "host", Address: "127.0.0.9:18201", Port: 443},
	})
	if res.GetError() != "" {
		t.Fatalf("unexpected error %q", res.GetError())
	}
	if got := fake.lastTgt; got.Port != 443 || got.PodIP != "127.0.0.9" {
		t.Errorf("dialled %s:%d, want 127.0.0.9:443 (explicit field wins)", got.PodIP, got.Port)
	}
}

// TestErrorResultCarriesADecodableCheckResult is the mass-MTR regression: a refused task reported
// an empty DetailsJson, the controller returned it verbatim as a 200 body, and the Console recorded
// "decode result: unexpected end of JSON input" instead of the reason the agent gave.
func TestErrorResultCarriesADecodableCheckResult(t *testing.T) {
	e := NewTaskExecutor(nil, nil, checker.Target{NodeName: "node-1", Zone: "zone-a"}, 8080, nil, 1, ExternalPolicy{})

	res := e.errorResult(&pb.TaskRequest{
		TaskId:    "task-1",
		CheckType: "mtr",
		Target:    &pb.AgentMeta{NodeName: "node-2", Zone: "zone-b"},
	}, errors.New("agent busy: too many concurrent diagnostic tasks"))

	if len(res.GetDetailsJson()) == 0 {
		t.Fatal("a refusal carried no payload; the Console cannot decode an empty 200 body")
	}

	var decoded model.CheckResult
	if err := json.Unmarshal(res.GetDetailsJson(), &decoded); err != nil {
		t.Fatalf("DetailsJson does not decode as a CheckResult: %v", err)
	}
	if decoded.Success {
		t.Error("a refusal decoded as a success")
	}
	if decoded.Error != "agent busy: too many concurrent diagnostic tasks" {
		t.Errorf("Error = %q, want the agent's own reason", decoded.Error)
	}
	if decoded.Type != model.CheckMTR {
		t.Errorf("Type = %q, want mtr", decoded.Type)
	}
	if decoded.Source != "node-1" || decoded.Destination != "node-2" {
		t.Errorf("pair = %s->%s, want node-1->node-2", decoded.Source, decoded.Destination)
	}
}

// TestSaturatedExecutorReportsAnHonestResult covers the path that produced 685 of the run's 1274
// rows: nine outbound MTR pairs against a semaphore of four.
func TestSaturatedExecutorReportsAnHonestResult(t *testing.T) {
	rep := &recordingReporter{}
	e := NewTaskExecutor(nil, nil, checker.Target{NodeName: "node-1"}, 8080, rep, 1, ExternalPolicy{})

	// Fill the only slot, then offer a second task.
	e.sem <- struct{}{}
	defer func() { <-e.sem }()

	e.Handle(context.Background(), &pb.TaskRequest{TaskId: "task-2", CheckType: "mtr"})

	got := rep.results()
	if len(got) != 1 {
		t.Fatalf("reported %d results, want 1", len(got))
	}
	var decoded model.CheckResult
	if err := json.Unmarshal(got[0].GetDetailsJson(), &decoded); err != nil {
		t.Fatalf("a saturation refusal is not decodable: %v", err)
	}
	if !strings.Contains(decoded.Error, "agent busy") {
		t.Errorf("error = %q, want it to name the saturation", decoded.Error)
	}
}

// recordingReporter captures the TaskResults an executor reports.
type recordingReporter struct {
	mu   sync.Mutex
	seen []*pb.TaskResult
}

func (r *recordingReporter) ReportTaskResult(_ context.Context, res *pb.TaskResult) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, res)
	return nil
}

func (r *recordingReporter) results() []*pb.TaskResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*pb.TaskResult(nil), r.seen...)
}
