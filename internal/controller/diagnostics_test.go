package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/protobuf/proto"
)

// fakeDispatcher is a stand-in TaskDispatcher for handler tests.
type fakeDispatcher struct {
	result   *pb.TaskResult
	err      error
	gotAgent string
	gotReq   *pb.TaskRequest
}

func (f *fakeDispatcher) Dispatch(_ context.Context, agentID string, req *pb.TaskRequest) (*pb.TaskResult, error) {
	f.gotAgent = agentID
	f.gotReq = req
	return f.result, f.err
}

func newDiagTestHandler(t *testing.T, disp TaskDispatcher, leaderEnabled bool, isLeader bool) *DiagnosticsHandler {
	t.Helper()
	reg := NewRegistry(30 * time.Second)
	reg.Register(model.AgentInfo{ID: "agent-a", NodeName: "node-a", PodIP: "10.0.0.1", Zone: "z1"})
	reg.Register(model.AgentInfo{ID: "agent-b", NodeName: "node-b", PodIP: "10.0.0.2", Zone: "z2"})
	m := metrics.NewPrometheusMetrics("test", prometheus.NewRegistry())
	h := NewDiagnosticsHandler(reg, disp, m, leaderEnabled, func() bool { return isLeader }, nil)
	return h
}

func doDiag(h *DiagnosticsHandler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/diagnostics", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestDiagnosticsHappyPath(t *testing.T) {
	checkResult := model.CheckResult{Type: model.CheckICMP, Success: true, Source: "node-a", Destination: "node-b"}
	details, _ := json.Marshal(checkResult)
	disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: details}}
	h := newDiagTestHandler(t, disp, false, false)

	w := doDiag(h, `{"source":"node-a","destination":"node-b","type":"icmp","plane":"pod"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var got model.CheckResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not a CheckResult: %v", err)
	}
	if got.Type != model.CheckICMP || !got.Success {
		t.Errorf("unexpected check result: %+v", got)
	}
	// Source resolves to agent, destination becomes the target meta.
	if disp.gotAgent != "agent-a" {
		t.Errorf("expected dispatch to agent-a, got %q", disp.gotAgent)
	}
	if disp.gotReq.GetTarget().GetNodeName() != "node-b" {
		t.Errorf("expected target node-b, got %q", disp.gotReq.GetTarget().GetNodeName())
	}
	if disp.gotReq.GetPlane() != "pod" {
		t.Errorf("expected plane pod, got %q", disp.gotReq.GetPlane())
	}
	if disp.gotReq.GetCheckType() != "icmp" {
		t.Errorf("expected check type icmp, got %q", disp.gotReq.GetCheckType())
	}
}

func TestDiagnosticsPlaneDefaultsToPod(t *testing.T) {
	disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: []byte(`{}`)}}
	h := newDiagTestHandler(t, disp, false, false)

	w := doDiag(h, `{"source":"node-a","destination":"node-b","type":"tcp"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if disp.gotReq.GetPlane() != "pod" {
		t.Errorf("expected default plane pod, got %q", disp.gotReq.GetPlane())
	}
}

// Agents probe the pod plane only and never read the task's plane, so a host-plane request ran a pod
// probe and the console's event history recorded it as plane=host. It is refused before anything is
// dispatched or published.
func TestDiagnosticsRefusesAPlaneAgentsDoNotProbe(t *testing.T) {
	for _, plane := range []string{"host", "Pod", "pod\u0000"} {
		disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: []byte(`{}`)}}
		pub := &fakeEventPublisher{}
		h := newDiagTestHandlerWithEvents(t, disp, pub)

		w := doDiag(h, `{"source":"node-a","destination":"node-b","type":"icmp","plane":"`+plane+`"}`)
		if w.Code != http.StatusBadRequest {
			t.Errorf("plane %q: status %d, want 400 (%s)", plane, w.Code, w.Body.String())
		}
		if disp.gotReq != nil {
			t.Errorf("plane %q: dispatched %v", plane, disp.gotReq)
		}
		if evs := pub.events(); len(evs) != 0 {
			t.Errorf("plane %q: published %v", plane, evs)
		}
	}
}

func TestDiagnosticsInvalidType(t *testing.T) {
	disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: []byte(`{}`)}}
	h := newDiagTestHandler(t, disp, false, false)

	w := doDiag(h, `{"source":"node-a","destination":"node-b","type":"bogus"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid type, got %d", w.Code)
	}
}

func TestDiagnosticsMissingFields(t *testing.T) {
	disp := &fakeDispatcher{}
	h := newDiagTestHandler(t, disp, false, false)

	for _, body := range []string{
		`{"destination":"node-b","type":"icmp"}`,
		`{"source":"node-a","type":"icmp"}`,
		`{"source":"node-a","destination":"node-b"}`,
	} {
		w := doDiag(h, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for body %s, got %d", body, w.Code)
		}
	}
}

func TestDiagnosticsBadJSON(t *testing.T) {
	disp := &fakeDispatcher{}
	h := newDiagTestHandler(t, disp, false, false)

	w := doDiag(h, `{not-json`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for malformed JSON, got %d", w.Code)
	}
}

func TestDiagnosticsUnknownSourceNode(t *testing.T) {
	disp := &fakeDispatcher{}
	h := newDiagTestHandler(t, disp, false, false)

	w := doDiag(h, `{"source":"ghost","destination":"node-b","type":"icmp"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown source, got %d", w.Code)
	}
	if disp.gotReq != nil {
		t.Error("dispatch must not run when source node is unknown")
	}
}

func TestDiagnosticsUnknownDestinationNode(t *testing.T) {
	disp := &fakeDispatcher{}
	h := newDiagTestHandler(t, disp, false, false)

	w := doDiag(h, `{"source":"node-a","destination":"ghost","type":"icmp"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown destination, got %d", w.Code)
	}
	if disp.gotReq != nil {
		t.Error("dispatch must not run when destination node is unknown")
	}
}

func TestDiagnosticsTimeout(t *testing.T) {
	disp := &fakeDispatcher{err: context.DeadlineExceeded}
	h := newDiagTestHandler(t, disp, false, false)

	w := doDiag(h, `{"source":"node-a","destination":"node-b","type":"icmp"}`)
	if w.Code != http.StatusGatewayTimeout {
		t.Errorf("expected 504 on dispatch timeout, got %d", w.Code)
	}
}

func TestDiagnosticsDispatchError(t *testing.T) {
	disp := &fakeDispatcher{err: ErrAgentNotSubscribed}
	h := newDiagTestHandler(t, disp, false, false)

	w := doDiag(h, `{"source":"node-a","destination":"node-b","type":"icmp"}`)
	// Source agent is registered but not watching -> the source cannot run the
	// task; surface as 404 (no agent able to serve).
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when source agent not subscribed, got %d", w.Code)
	}
}

// A replica demoted while the task was in flight answers like a standby (503, retry elsewhere), not
// like a broken agent (502).
func TestDiagnosticsLeadershipLostMidDispatchIsALeadershipError(t *testing.T) {
	disp := &fakeDispatcher{err: fmt.Errorf("dispatch: %w", ErrLeadershipLost)}
	h := newDiagTestHandler(t, disp, true, true)

	w := doDiag(h, `{"source":"node-a","destination":"node-b","type":"icmp"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d (%s), want 503 when leadership is lost mid-dispatch", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if !strings.Contains(w.Body.String(), "leadership lost") {
		t.Errorf("body = %q, want it to say leadership was lost", w.Body.String())
	}
}

func TestDiagnosticsNotLeader(t *testing.T) {
	disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: []byte(`{}`)}}
	h := newDiagTestHandler(t, disp, true, false)

	w := doDiag(h, `{"source":"node-a","destination":"node-b","type":"icmp"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when not leader, got %d", w.Code)
	}
	if disp.gotReq != nil {
		t.Error("dispatch must not run on a non-leader replica")
	}
}

func TestDiagnosticsLeaderServes(t *testing.T) {
	disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: []byte(`{}`)}}
	h := newDiagTestHandler(t, disp, true, true)

	w := doDiag(h, `{"source":"node-a","destination":"node-b","type":"icmp"}`)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 when leader, got %d", w.Code)
	}
}

func TestDiagnosticsTimeoutQueryCap(t *testing.T) {
	// Capture the deadline the handler put on the dispatch context.
	var haveDeadline bool
	var remaining time.Duration
	disp := dispatcherFunc(func(ctx context.Context, _ string, _ *pb.TaskRequest) (*pb.TaskResult, error) {
		dl, ok := ctx.Deadline()
		haveDeadline = ok
		remaining = time.Until(dl)
		return &pb.TaskResult{Success: true, DetailsJson: []byte(`{}`)}, nil
	})
	h := newDiagTestHandler(t, disp, false, false)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/v1/diagnostics?timeout=999", strings.NewReader(`{"source":"node-a","destination":"node-b","type":"icmp"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !haveDeadline {
		t.Fatal("expected dispatch context to carry a deadline")
	}
	// 999s must be capped to <= 120s.
	if remaining > 121*time.Second {
		t.Errorf("expected timeout capped near 120s, got %v", remaining)
	}
}

// fakeEventPublisher records every event the handler publishes.
type fakeEventPublisher struct {
	mu   sync.Mutex
	sent []*pb.Event
}

func (f *fakeEventPublisher) PublishEvent(ev *pb.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, ev)
}

func (f *fakeEventPublisher) events() []*pb.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*pb.Event, len(f.sent))
	copy(out, f.sent)
	return out
}

// newDiagTestHandlerWithEvents mirrors newDiagTestHandler but injects an
// EventPublisher seam.
func newDiagTestHandlerWithEvents(t *testing.T, disp TaskDispatcher, events EventPublisher) *DiagnosticsHandler {
	t.Helper()
	reg := NewRegistry(30 * time.Second)
	reg.Register(model.AgentInfo{ID: "src-agent", NodeName: "node-a"})
	reg.Register(model.AgentInfo{ID: "dst-agent", NodeName: "node-b"})
	m := metrics.NewPrometheusMetrics("test", prometheus.NewRegistry())
	return NewDiagnosticsHandler(reg, disp, m, false, nil, events)
}

func TestDiagnosticsHandlerPublishesCheckObserved(t *testing.T) {
	result := model.CheckResult{Type: model.CheckTCP, Success: true, Duration: 5 * time.Millisecond}
	body, _ := json.Marshal(result)
	dispatcher := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: body}}
	pub := &fakeEventPublisher{}
	h := newDiagTestHandlerWithEvents(t, dispatcher, pub)

	w := doDiag(h, `{"source":"node-a","destination":"node-b","type":"tcp"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	events := pub.events()
	if len(events) != 2 {
		t.Fatalf("expected 2 events (dispatched progress + check_observed), got %d: %+v", len(events), events)
	}
	if events[0].GetDiagnosticProgress().GetState() != "dispatched" {
		t.Errorf("expected first event DiagnosticProgress(dispatched), got %+v", events[0])
	}
	co := events[1].GetCheckObserved()
	if co == nil || co.GetCheckType() != "tcp" || !co.GetSuccess() ||
		co.GetSourceNode() != "node-a" || co.GetDestinationNode() != "node-b" {
		t.Errorf("unexpected CheckObserved: %+v", co)
	}
}

func TestDiagnosticsHandlerPublishesMTRTriggeredAndCompleted(t *testing.T) {
	result := model.CheckResult{
		Type: model.CheckMTR, Success: true,
		Details: model.MTRDetails{Target: "node-b", Hops: []model.MTRHop{{Number: 1, IP: "10.0.0.1", RTT: time.Millisecond}}},
	}
	body, _ := json.Marshal(result)
	dispatcher := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: body}}
	pub := &fakeEventPublisher{}
	h := newDiagTestHandlerWithEvents(t, dispatcher, pub)

	w := doDiag(h, `{"source":"node-a","destination":"node-b","type":"mtr"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	events := pub.events()
	if len(events) != 2 {
		t.Fatalf("expected 2 events (mtr_triggered + mtr_completed), got %d: %+v", len(events), events)
	}
	if events[0].GetMtrTriggered() == nil {
		t.Errorf("expected first event MTRTriggered, got %+v", events[0])
	}
	mc := events[1].GetMtrCompleted()
	if mc == nil || !mc.GetSuccess() || len(mc.GetHops()) != 1 || mc.GetHops()[0].GetIp() != "10.0.0.1" {
		t.Errorf("unexpected MTRCompleted: %+v", mc)
	}
}

func TestDiagnosticsHandlerPublishesDiagnosticProgressOnTimeout(t *testing.T) {
	dispatcher := &fakeDispatcher{err: context.DeadlineExceeded}
	pub := &fakeEventPublisher{}
	h := newDiagTestHandlerWithEvents(t, dispatcher, pub)

	doDiag(h, `{"source":"node-a","destination":"node-b","type":"tcp"}`)

	events := pub.events()
	if len(events) != 2 {
		t.Fatalf("expected dispatched + timeout progress events, got %d: %+v", len(events), events)
	}
	if events[1].GetDiagnosticProgress().GetState() != "timeout" {
		t.Errorf("expected second event state=timeout, got %+v", events[1])
	}
}

// Nanosecond values above 2^24 cannot survive a float32 round-trip, and the
// loss ratio is not representable in binary at all: exact assertions on these
// pin the numbers publishObserved carries from the agent's JSON into the events.
const (
	// ~1.23s, above 2^24.
	fixtureNs = int64(1234567891)
	// ~9.9s, above 2^32 as well.
	fixtureLargeNs = int64(9876543210)
	// Not representable in binary: a float32 hop would report 0.33329999...
	fixtureLossRatio = 0.3333
)

func TestDiagnosticsHandlerCheckObservedCarriesExactNumbers(t *testing.T) {
	// Raw fixture rather than a marshalled CheckResult, so the exact numbers
	// crossing the JSON boundary are visible in the test.
	details := []byte(`{"type":"tcp","success":true,"source":"node-a","destination":"node-b","duration":1234567891}`)
	dispatcher := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: details}}
	pub := &fakeEventPublisher{}
	h := newDiagTestHandlerWithEvents(t, dispatcher, pub)

	w := doDiag(h, `{"source":"node-a","destination":"node-b","type":"tcp"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	events := pub.events()
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(events), events)
	}
	co := events[1].GetCheckObserved()
	if co == nil {
		t.Fatalf("expected CheckObserved as the terminal event, got %+v", events[1])
	}
	if co.GetDurationNs() != fixtureNs {
		t.Errorf("duration_ns = %d, want %d", co.GetDurationNs(), fixtureNs)
	}
	if co.GetPlane() != "pod" || co.GetCheckType() != "tcp" {
		t.Errorf("unexpected CheckObserved metadata: %+v", co)
	}
}

func TestDiagnosticsHandlerMTRHopsCarryExactNumbers(t *testing.T) {
	details := []byte(`{"type":"mtr","success":true,"details":{"target":"node-b","hops":[
		{"number":1,"ip":"10.0.0.1","hostname":"hop-1","rtt":1234567891,"lossRatio":0.3333},
		{"number":30,"ip":"10.0.0.30","rtt":9876543210,"lossRatio":0}
	]}}`)
	dispatcher := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: details}}
	pub := &fakeEventPublisher{}
	h := newDiagTestHandlerWithEvents(t, dispatcher, pub)

	w := doDiag(h, `{"source":"node-a","destination":"node-b","type":"mtr"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	events := pub.events()
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(events), events)
	}
	mc := events[1].GetMtrCompleted()
	if mc == nil {
		t.Fatalf("expected MTRCompleted as the terminal event, got %+v", events[1])
	}
	hops := mc.GetHops()
	if len(hops) != 2 {
		t.Fatalf("expected 2 hops, got %d: %+v", len(hops), hops)
	}

	if hops[0].GetNumber() != 1 {
		t.Errorf("hop[0].number = %d, want 1", hops[0].GetNumber())
	}
	if hops[0].GetIp() != "10.0.0.1" || hops[0].GetHostname() != "hop-1" {
		t.Errorf("hop[0] address fields wrong: %+v", hops[0])
	}
	if hops[0].GetRttNs() != fixtureNs {
		t.Errorf("hop[0].rtt_ns = %d, want %d", hops[0].GetRttNs(), fixtureNs)
	}
	if hops[0].GetLossRatio() != fixtureLossRatio {
		t.Errorf("hop[0].loss_ratio = %v, want %v", hops[0].GetLossRatio(), fixtureLossRatio)
	}

	if hops[1].GetNumber() != 30 {
		t.Errorf("hop[1].number = %d, want 30", hops[1].GetNumber())
	}
	if hops[1].GetRttNs() != fixtureLargeNs {
		t.Errorf("hop[1].rtt_ns = %d, want %d", hops[1].GetRttNs(), fixtureLargeNs)
	}
	if hops[1].GetLossRatio() != 0 {
		t.Errorf("hop[1].loss_ratio = %v, want 0", hops[1].GetLossRatio())
	}
}

// TestDiagnosticsHandlerEventsShareTaskID pins the correlation contract: the
// dispatch-start event, the dispatched request and the terminal event all carry
// the same non-empty task ID.
func TestDiagnosticsHandlerEventsShareTaskID(t *testing.T) {
	details, _ := json.Marshal(model.CheckResult{Type: model.CheckTCP, Success: true})
	dispatcher := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: details}}
	pub := &fakeEventPublisher{}
	h := newDiagTestHandlerWithEvents(t, dispatcher, pub)

	w := doDiag(h, `{"source":"node-a","destination":"node-b","type":"tcp"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	events := pub.events()
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(events), events)
	}
	taskID := events[0].GetDiagnosticProgress().GetTaskId()
	if taskID == "" {
		t.Fatal("dispatch-start event carries no task_id")
	}
	if got := events[1].GetCheckObserved().GetTaskId(); got != taskID {
		t.Errorf("terminal event task_id = %q, want %q", got, taskID)
	}
	if got := dispatcher.gotReq.GetTaskId(); got != taskID {
		t.Errorf("dispatched request task_id = %q, want %q", got, taskID)
	}
}

func TestDiagnosticsHandlerMTREventsShareTaskID(t *testing.T) {
	details, _ := json.Marshal(model.CheckResult{Type: model.CheckMTR, Success: true})
	dispatcher := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: details}}
	pub := &fakeEventPublisher{}
	h := newDiagTestHandlerWithEvents(t, dispatcher, pub)

	w := doDiag(h, `{"source":"node-a","destination":"node-b","type":"mtr"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	events := pub.events()
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(events), events)
	}
	taskID := events[0].GetMtrTriggered().GetTaskId()
	if taskID == "" {
		t.Fatal("MTRTriggered carries no task_id")
	}
	if got := events[1].GetMtrCompleted().GetTaskId(); got != taskID {
		t.Errorf("MTRCompleted task_id = %q, want %q", got, taskID)
	}
}

// TestDiagnosticsHandlerFailurePathSharesTaskID covers the non-terminal-failure
// branch: the error progress event must be correlatable with the start event.
func TestDiagnosticsHandlerFailurePathSharesTaskID(t *testing.T) {
	dispatcher := &fakeDispatcher{err: ErrAgentNotSubscribed}
	pub := &fakeEventPublisher{}
	h := newDiagTestHandlerWithEvents(t, dispatcher, pub)

	doDiag(h, `{"source":"node-a","destination":"node-b","type":"tcp"}`)

	events := pub.events()
	if len(events) != 2 {
		t.Fatalf("expected dispatched + error progress events, got %d: %+v", len(events), events)
	}
	start := events[0].GetDiagnosticProgress()
	fail := events[1].GetDiagnosticProgress()
	if start.GetTaskId() == "" || fail.GetTaskId() != start.GetTaskId() {
		t.Errorf("task_id not shared: start=%q fail=%q", start.GetTaskId(), fail.GetTaskId())
	}
	if fail.GetState() != "error" {
		t.Errorf("expected state=error, got %q", fail.GetState())
	}
}

// --- M4: external destinations -------------------------------------------

// newDiagTestHandlerExternal builds a handler whose source agent advertises
// exactly srcCaps, so the external-destination capability gate can be exercised
// from both sides.
func newDiagTestHandlerExternal(t *testing.T, disp TaskDispatcher, events EventPublisher, srcCaps []string) *DiagnosticsHandler {
	t.Helper()
	reg := NewRegistry(30 * time.Second)
	reg.Register(model.AgentInfo{ID: "src-agent", NodeName: "node-a", Capabilities: srcCaps})
	reg.Register(model.AgentInfo{ID: "dst-agent", NodeName: "node-b"})
	m := metrics.NewPrometheusMetrics("test", prometheus.NewRegistry())
	return NewDiagnosticsHandler(reg, disp, m, false, nil, events)
}

func TestDiagnosticsM3RequestShapeUnchanged(t *testing.T) {
	details := []byte(`{"type":"icmp","success":true,"source":"node-a","destination":"node-b"}`)
	disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: details}}
	h := newDiagTestHandler(t, disp, false, false)

	w := doDiag(h, `{"source":"node-a","destination":"node-b","type":"icmp","plane":"pod"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if got := w.Body.Bytes(); !bytes.Equal(got, details) {
		t.Errorf("response body = %q, want the agent's details_json verbatim %q", got, details)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if nosniff := w.Header().Get("X-Content-Type-Options"); nosniff != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", nosniff)
	}

	if disp.gotAgent != "agent-a" {
		t.Errorf("dispatched to %q, want agent-a", disp.gotAgent)
	}
	if disp.gotReq.GetTaskId() == "" {
		t.Fatal("dispatched request carries no task_id")
	}
	want := &pb.TaskRequest{
		TaskId:    disp.gotReq.GetTaskId(),
		CheckType: "icmp",
		Plane:     "pod",
		Target:    &pb.AgentMeta{Id: "agent-b", NodeName: "node-b", PodIp: "10.0.0.2", Zone: "z2"},
	}
	if !proto.Equal(disp.gotReq, want) {
		t.Errorf("dispatched TaskRequest = %v, want %v", disp.gotReq, want)
	}
	if disp.gotReq.GetExternalTarget() != nil {
		t.Error("external_target must stay nil for a node destination")
	}
}

func TestDiagnosticsExternalDestinationDispatchesExternalTarget(t *testing.T) {
	disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: []byte(`{}`)}}
	h := newDiagTestHandlerExternal(t, disp, nil, []string{capabilityExternalChecks})

	w := doDiag(h, `{"source":"node-a","destination":"edge-gw","destinationKind":"external",`+
		`"destinationAddress":"10.10.0.1","type":"tcp"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if disp.gotAgent != "src-agent" {
		t.Errorf("dispatched to %q, want src-agent", disp.gotAgent)
	}
	// The proto forbids both being set; the target side must stay empty.
	if disp.gotReq.GetTarget() != nil {
		t.Errorf("target must be nil for an external destination, got %v", disp.gotReq.GetTarget())
	}
	et := disp.gotReq.GetExternalTarget()
	if et == nil {
		t.Fatal("external_target not set")
	}
	want := &pb.ExternalTarget{Name: "edge-gw", Kind: "host", Address: "10.10.0.1", Port: 0}
	if !proto.Equal(et, want) {
		t.Errorf("external_target = %v, want %v", et, want)
	}
	if disp.gotReq.GetPlane() != "pod" || disp.gotReq.GetCheckType() != "tcp" {
		t.Errorf("unexpected plane/check type: %v", disp.gotReq)
	}
}

// With no destination name supplied the address doubles as the name, so the
// event stream and any downstream identifier still have something to show.
func TestDiagnosticsExternalNameFallsBackToAddress(t *testing.T) {
	disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: []byte(`{}`)}}
	h := newDiagTestHandlerExternal(t, disp, nil, []string{capabilityExternalChecks})

	w := doDiag(h, `{"source":"node-a","destinationKind":"external","destinationAddress":"1.1.1.1","type":"icmp"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if got := disp.gotReq.GetExternalTarget().GetName(); got != "1.1.1.1" {
		t.Errorf("external_target.name = %q, want the address 1.1.1.1", got)
	}
}

// A source agent that never advertised external-checks gets a specific,
// actionable 501 naming its node instead of a task it would silently mishandle.
func TestDiagnosticsExternalWithoutCapabilityIsNotImplemented(t *testing.T) {
	disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: []byte(`{}`)}}
	h := newDiagTestHandlerExternal(t, disp, nil, nil)

	w := doDiag(h, `{"source":"node-a","destinationKind":"external","destinationAddress":"1.1.1.1","type":"icmp"}`)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "node-a") {
		t.Errorf("501 detail must name the agent's node, got %q", w.Body.String())
	}
	if disp.gotReq != nil {
		t.Error("dispatch must not run when the source agent lacks external-checks")
	}
}

// An unrelated capability must not open the gate.
func TestDiagnosticsExternalUnrelatedCapabilityIsNotImplemented(t *testing.T) {
	disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: []byte(`{}`)}}
	h := newDiagTestHandlerExternal(t, disp, nil, []string{"mtr"})

	w := doDiag(h, `{"source":"node-a","destinationKind":"external","destinationAddress":"1.1.1.1","type":"icmp"}`)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestDiagnosticsDestinationValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "destination and destinationAddress both empty",
			body: `{"source":"node-a","type":"icmp"}`,
		},
		{
			name: "external with empty address",
			body: `{"source":"node-a","destination":"edge-gw","destinationKind":"external","type":"icmp"}`,
		},
		{
			name: "external with both empty",
			body: `{"source":"node-a","destinationKind":"external","type":"icmp"}`,
		},
		{
			name: "unknown destinationKind",
			body: `{"source":"node-a","destination":"node-b","destinationKind":"pod","type":"icmp"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: []byte(`{}`)}}
			h := newDiagTestHandlerExternal(t, disp, nil, []string{capabilityExternalChecks})

			w := doDiag(h, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d (%s)", w.Code, w.Body.String())
			}
			if disp.gotReq != nil {
				t.Error("dispatch must not run for a rejected request")
			}
		})
	}
}

// destinationKind=node is the explicit spelling of the default and keeps the
// registry lookup, including its 404.
func TestDiagnosticsExplicitNodeKindResolvesRegistry(t *testing.T) {
	disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: []byte(`{}`)}}
	h := newDiagTestHandlerExternal(t, disp, nil, []string{capabilityExternalChecks})

	w := doDiag(h, `{"source":"node-a","destination":"node-b","destinationKind":"node","type":"icmp"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if disp.gotReq.GetTarget().GetNodeName() != "node-b" {
		t.Errorf("target = %v, want node-b", disp.gotReq.GetTarget())
	}
	if disp.gotReq.GetExternalTarget() != nil {
		t.Error("external_target must stay nil for destinationKind=node")
	}

	disp2 := &fakeDispatcher{}
	h2 := newDiagTestHandlerExternal(t, disp2, nil, []string{capabilityExternalChecks})
	w2 := doDiag(h2, `{"source":"node-a","destination":"ghost","destinationKind":"node","type":"icmp"}`)
	if w2.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown node destination, got %d", w2.Code)
	}
}

// The leader gate runs before anything external-specific.
func TestDiagnosticsExternalNotLeader(t *testing.T) {
	disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: []byte(`{}`)}}
	reg := NewRegistry(30 * time.Second)
	reg.Register(model.AgentInfo{ID: "src-agent", NodeName: "node-a", Capabilities: []string{capabilityExternalChecks}})
	m := metrics.NewPrometheusMetrics("test", prometheus.NewRegistry())
	h := NewDiagnosticsHandler(reg, disp, m, true, func() bool { return false }, nil)

	w := doDiag(h, `{"source":"node-a","destinationKind":"external","destinationAddress":"1.1.1.1","type":"icmp"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when not leader, got %d", w.Code)
	}
	if disp.gotReq != nil {
		t.Error("dispatch must not run on a non-leader replica")
	}
}

// The Live feed reads names, never addresses: destination_node must carry the
// external target's name on every published event of the run.
func TestDiagnosticsExternalEventsCarryTargetName(t *testing.T) {
	details, _ := json.Marshal(model.CheckResult{Type: model.CheckTCP, Success: true, Duration: 3 * time.Millisecond})
	disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: details}}
	pub := &fakeEventPublisher{}
	h := newDiagTestHandlerExternal(t, disp, pub, []string{capabilityExternalChecks})

	w := doDiag(h, `{"source":"node-a","destination":"edge-gw","destinationKind":"external",`+
		`"destinationAddress":"10.10.0.1","type":"tcp"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}

	events := pub.events()
	if len(events) != 2 {
		t.Fatalf("expected dispatched + check_observed, got %d: %+v", len(events), events)
	}
	if got := events[0].GetDiagnosticProgress().GetDestinationNode(); got != "edge-gw" {
		t.Errorf("dispatched event destination_node = %q, want edge-gw", got)
	}
	co := events[1].GetCheckObserved()
	if co == nil {
		t.Fatalf("expected CheckObserved as the terminal event, got %+v", events[1])
	}
	if co.GetDestinationNode() != "edge-gw" {
		t.Errorf("CheckObserved destination_node = %q, want the target name edge-gw", co.GetDestinationNode())
	}
	if strings.Contains(co.GetDestinationNode(), "10.10.0.1") {
		t.Error("the address must never leak into destination_node")
	}
}

// dispatcherFunc adapts a func to the TaskDispatcher interface.
type dispatcherFunc func(context.Context, string, *pb.TaskRequest) (*pb.TaskResult, error)

func (f dispatcherFunc) Dispatch(ctx context.Context, agentID string, req *pb.TaskRequest) (*pb.TaskResult, error) {
	return f(ctx, agentID, req)
}

// TestDiagnosticsBodyNeverEmpty is the other end of the mass-MTR regression: the handler returned
// the agent's DetailsJson verbatim, so an agent that refused a task (empty payload) produced a 200
// with a zero-byte body and every JSON client reported "unexpected end of JSON input" instead of
// the refusal's reason.
func TestDiagnosticsBodyNeverEmpty(t *testing.T) {
	tests := []struct {
		name string
		res  *pb.TaskResult
		want string
	}{
		{
			name: "agent payload is returned verbatim",
			res:  &pb.TaskResult{DetailsJson: []byte(`{"type":"mtr","success":true}`)},
			want: `{"type":"mtr","success":true}`,
		},
		{
			name: "a refusal without a payload still decodes",
			res:  &pb.TaskResult{Success: false, Error: "agent busy: too many concurrent diagnostic tasks"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := diagnosticsBody(tc.res, "mtr", "node-a", "node-b", time.Now())
			if len(body) == 0 {
				t.Fatal("diagnosticsBody returned no bytes; the Console cannot decode an empty 200")
			}
			if tc.want != "" && string(body) != tc.want {
				t.Fatalf("body = %s, want %s", body, tc.want)
			}

			var decoded model.CheckResult
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("body does not decode as a CheckResult: %v", err)
			}
			if tc.res.GetError() != "" && decoded.Error != tc.res.GetError() {
				t.Errorf("Error = %q, want the agent's reason %q", decoded.Error, tc.res.GetError())
			}
		})
	}
}

// An agent that reports no payload still ends the run: the caller gets a body naming the check, and
// the Console's timeline gets the terminal event that follows the dispatched one.
func TestDiagnosticsEmptyPayloadStillPublishesTheTerminalEvent(t *testing.T) {
	for _, typ := range []string{"icmp", "mtr"} {
		t.Run(typ, func(t *testing.T) {
			disp := &fakeDispatcher{result: &pb.TaskResult{Success: false, Error: "agent said no"}}
			pub := &fakeEventPublisher{}
			h := newDiagTestHandlerWithEvents(t, disp, pub)

			w := doDiag(h, `{"source":"node-a","destination":"node-b","type":"`+typ+`"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			var body model.CheckResult
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("body does not decode: %v (%s)", err, w.Body.String())
			}
			if string(body.Type) != typ || body.Source != "node-a" || body.Destination != "node-b" ||
				body.Error != "agent said no" || body.Timestamp.Year() < 2000 {
				t.Errorf("synthesized body = %s, want the check type, both nodes, the reason and a real timestamp", w.Body.String())
			}

			events := pub.events()
			if len(events) != 2 {
				t.Fatalf("expected the dispatched and the terminal event, got %d: %+v", len(events), events)
			}
			switch typ {
			case "mtr":
				if mc := events[1].GetMtrCompleted(); mc == nil || mc.GetSuccess() || mc.GetError() != "agent said no" {
					t.Errorf("terminal event = %+v, want a failed MTRCompleted carrying the reason", events[1])
				}
			default:
				if co := events[1].GetCheckObserved(); co == nil || co.GetSuccess() || co.GetError() != "agent said no" {
					t.Errorf("terminal event = %+v, want a failed CheckObserved carrying the reason", events[1])
				}
			}
		})
	}
}

func TestDiagnosticsAcceptsPMTU(t *testing.T) {
	disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: []byte(`{}`)}}
	h := newDiagTestHandler(t, disp, false, false)

	w := doDiag(h, `{"source":"node-a","destination":"node-b","type":"pmtu"}`)
	if w.Code == http.StatusBadRequest {
		t.Fatalf("pmtu rejected as a diagnostic type: %d %s", w.Code, w.Body.String())
	}
}

// No plane:* at all means an agent older than 2.4.0 whose planes are unknown: fail open and dispatch.
// Advertising plane:pmtu dispatches too.
func TestDiagnosticsPMTUPlaneGateFailsOpen(t *testing.T) {
	for name, caps := range map[string][]string{
		"no plane capabilities": nil,
		"external only":         {capabilityExternalChecks},
		"plane:pmtu":            {"plane:tcp", "plane:pmtu"},
	} {
		t.Run(name, func(t *testing.T) {
			disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: []byte(`{}`)}}
			h := newDiagTestHandlerExternal(t, disp, nil, caps)

			w := doDiag(h, `{"source":"node-a","destination":"node-b","type":"pmtu"}`)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
			}
			if disp.gotReq.GetCheckType() != "pmtu" {
				t.Errorf("dispatched %v, want a pmtu task", disp.gotReq)
			}
		})
	}
}

// The plane gate covers every type, not only pmtu: a source that advertises its planes without the
// requested one (checkers.<type>.enabled=false, http off by default) would refuse the task with
// success=false, which the CLI turns into exit 2 "the check ran and failed". The types it does
// advertise still dispatch.
func TestDiagnosticsPlaneGateCoversEveryType(t *testing.T) {
	// The default chart: http off, every other checker on.
	defaultCaps := []string{"plane:tcp", "plane:udp", "plane:icmp", "plane:pmtu", "plane:dns", "plane:mtr"}
	for checkType := range validCheckTypes {
		t.Run(checkType, func(t *testing.T) {
			var caps []string
			for _, c := range defaultCaps {
				if c != "plane:"+checkType {
					caps = append(caps, c)
				}
			}
			disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: []byte(`{}`)}}
			m := metrics.NewPrometheusMetrics("test", prometheus.NewRegistry())
			reg := NewRegistry(30 * time.Second)
			reg.Register(model.AgentInfo{ID: "src-agent", NodeName: "node-a", Capabilities: caps})
			reg.Register(model.AgentInfo{ID: "dst-agent", NodeName: "node-b"})
			h := NewDiagnosticsHandler(reg, disp, m, false, nil, nil)

			w := doDiag(h, `{"source":"node-a","destination":"node-b","type":"`+checkType+`"}`)

			if w.Code != http.StatusNotImplemented {
				t.Fatalf("expected 501, got %d (%s)", w.Code, w.Body.String())
			}
			if body := w.Body.String(); !strings.Contains(body, "node-a") || !strings.Contains(body, checkType) {
				t.Errorf("501 detail must name the node and %s, got %q", checkType, body)
			}
			if disp.gotReq != nil {
				t.Errorf("dispatch must not run when the source agent does not advertise plane:%s", checkType)
			}
			if got := testutil.ToFloat64(m.ControllerDiagnostics.WithLabelValues(checkType, "unsupported")); got != 1 {
				t.Errorf("result=unsupported = %v, want 1", got)
			}
		})
	}

	disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: []byte(`{}`)}}
	h := newDiagTestHandlerExternal(t, disp, nil, defaultCaps)
	for _, checkType := range []string{"tcp", "icmp", "dns", "mtr"} {
		if w := doDiag(h, `{"source":"node-a","destination":"node-b","type":"`+checkType+`"}`); w.Code != http.StatusOK {
			t.Errorf("%s on an agent advertising plane:%s = %d (%s), want 200", checkType, checkType, w.Code, w.Body.String())
		}
	}
}

// An external destination is probed by the source agent's own checker of that type, so the plane gate
// applies there too.
func TestDiagnosticsPlaneGateCoversExternalDestinations(t *testing.T) {
	disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: []byte(`{}`)}}
	h := newDiagTestHandlerExternal(t, disp, nil, []string{capabilityExternalChecks, "plane:icmp", "plane:mtr"})

	w := doDiag(h, `{"source":"node-a","destinationKind":"external","destinationAddress":"1.1.1.1","type":"tcp"}`)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d (%s)", w.Code, w.Body.String())
	}
	if disp.gotReq != nil {
		t.Error("dispatch must not run when the source agent does not advertise plane:tcp")
	}
}

// pmtu measures the path to another agent's echo listener; an external destination would only fail on
// the agent with a message about another check type.
func TestDiagnosticsPMTURefusesExternalDestination(t *testing.T) {
	disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: []byte(`{}`)}}
	h := newDiagTestHandlerExternal(t, disp, nil, []string{capabilityExternalChecks, "plane:pmtu"})

	w := doDiag(h, `{"source":"node-a","destinationKind":"external","destinationAddress":"1.1.1.1","type":"pmtu"}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "pmtu") {
		t.Errorf("400 detail must name pmtu, got %q", w.Body.String())
	}
	if disp.gotReq != nil {
		t.Error("dispatch must not run for pmtu towards an external destination")
	}
}

// A caller that went away (Ctrl-C in the CLI, a cancelled Console run) cancels the request context. That is
// not a dispatch failure: it is counted as cancelled and publishes no error progress event.
func TestDiagnosticsCancelledRequestIsNotAnError(t *testing.T) {
	dispatcher := &fakeDispatcher{err: context.Canceled}
	pub := &fakeEventPublisher{}
	reg := NewRegistry(30 * time.Second)
	reg.Register(model.AgentInfo{ID: "src-agent", NodeName: "node-a"})
	reg.Register(model.AgentInfo{ID: "dst-agent", NodeName: "node-b"})
	m := metrics.NewPrometheusMetrics("test", prometheus.NewRegistry())
	h := NewDiagnosticsHandler(reg, dispatcher, m, false, nil, pub)

	w := doDiag(h, `{"source":"node-a","destination":"node-b","type":"tcp"}`)

	if got := testutil.ToFloat64(m.ControllerDiagnostics.WithLabelValues("tcp", "error")); got != 0 {
		t.Errorf("result=error = %v, want 0 for a cancelled request", got)
	}
	if got := testutil.ToFloat64(m.ControllerDiagnostics.WithLabelValues("tcp", "cancelled")); got != 1 {
		t.Errorf("result=cancelled = %v, want 1", got)
	}
	for _, ev := range pub.events() {
		if st := ev.GetDiagnosticProgress().GetState(); st == "error" {
			t.Errorf("a cancelled request published a progress event with state=%q", st)
		}
	}
	if w.Code == http.StatusBadGateway {
		t.Errorf("a cancelled request must not be answered as a dispatch failure (502)")
	}
}

// The agent probes an external destination only with tcp, icmp and mtr (internal/agent/tasks.go
// externalCapableChecks) and refuses the rest with success=false. Dispatching them anyway recorded
// every such pair as a failed probe; the controller answers 400 instead, before any dispatch.
func TestDiagnosticsExternalRefusesTypesTheAgentCannotProbe(t *testing.T) {
	for checkType := range validCheckTypes {
		t.Run(checkType, func(t *testing.T) {
			disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: []byte(`{}`)}}
			h := newDiagTestHandlerExternal(t, disp, nil, []string{capabilityExternalChecks})

			w := doDiag(h, `{"source":"node-a","destinationKind":"external",`+
				`"destinationAddress":"10.0.0.9:53","type":"`+checkType+`"}`)

			_, capable := externalCapableDiagnosticTypes[checkType]
			switch {
			case capable && w.Code != http.StatusOK:
				t.Fatalf("%s to an external destination: got %d (%s), want 200", checkType, w.Code, w.Body.String())
			case !capable && w.Code != http.StatusBadRequest:
				t.Fatalf("%s to an external destination: got %d (%s), want 400", checkType, w.Code, w.Body.String())
			case !capable && disp.gotReq != nil:
				t.Errorf("%s to an external destination was dispatched", checkType)
			case !capable && checkType != string(model.CheckPMTU) &&
				!strings.Contains(w.Body.String(), "tcp, icmp and mtr"):
				t.Errorf("400 must name the types that work, got %q", w.Body.String())
			}
		})
	}
	for _, capable := range []string{"tcp", "icmp", "mtr"} {
		if _, ok := externalCapableDiagnosticTypes[capable]; !ok {
			t.Errorf("%s must stay external-capable, as it is on the agent", capable)
		}
	}
}

// POST /api/v1/diagnostics is a handful of short strings. A body past the cap is refused with 413
// before it is buffered: one 32 MiB string was enough to push a 128Mi controller past its limit.
func TestDiagnosticsRefusesAnOversizedBody(t *testing.T) {
	disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: []byte(`{}`)}}
	h := newDiagTestHandler(t, disp, false, false)

	huge := `{"source":"` + strings.Repeat("a", 1<<20) + `","destination":"node-b","type":"icmp"}`
	w := doDiag(h, huge)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("1 MiB body: got %d (%.200s), want 413", w.Code, w.Body.String())
	}
	if disp.gotReq != nil {
		t.Error("an oversized request was dispatched")
	}

	// A body that is merely long but under the cap is still an ordinary request.
	padded := `{"source":"node-a","destination":"node-b","type":"icmp","plane":"pod","pad":"` +
		strings.Repeat("p", 8<<10) + `"}`
	if w := doDiag(h, padded); w.Code != http.StatusOK {
		t.Fatalf("8 KiB body: got %d (%s), want 200", w.Code, w.Body.String())
	}
}

// The 400 for a type that is not one-off external-capable says why for that type: only udp needs
// another agent at the far end; dns and http need none, they are simply not one-off external checks.
func TestDiagnosticsExternalRefusalNamesTheRealReason(t *testing.T) {
	for typ, want := range map[string]string{
		"udp":  "kconmon probe server",
		"dns":  "continuous external check",
		"http": "continuous external check",
	} {
		disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: []byte(`{}`)}}
		h := newDiagTestHandlerExternal(t, disp, nil, []string{capabilityExternalChecks})
		w := doDiag(h, `{"source":"node-a","destinationKind":"external","destinationAddress":"1.1.1.1","type":"`+typ+`"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d (%s)", typ, w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, want) {
			t.Errorf("%s: 400 detail %q, want it to mention %q", typ, body, want)
		}
		if typ != "udp" && strings.Contains(body, "kconmon probe server") {
			t.Errorf("%s: 400 detail %q claims it needs an agent at the far end", typ, body)
		}
	}
}
