package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/prometheus/client_golang/prometheus"
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
	h := NewDiagnosticsHandler(reg, disp, m, leaderEnabled, func() bool { return isLeader })
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

func TestDiagnosticsHostPlaneForwarded(t *testing.T) {
	disp := &fakeDispatcher{result: &pb.TaskResult{Success: true, DetailsJson: []byte(`{}`)}}
	h := newDiagTestHandler(t, disp, false, false)

	w := doDiag(h, `{"source":"node-a","destination":"node-b","type":"icmp","plane":"host"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected host plane to be accepted, got %d", w.Code)
	}
	if disp.gotReq.GetPlane() != "host" {
		t.Errorf("expected plane host forwarded, got %q", disp.gotReq.GetPlane())
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

// dispatcherFunc adapts a func to the TaskDispatcher interface.
type dispatcherFunc func(context.Context, string, *pb.TaskRequest) (*pb.TaskResult, error)

func (f dispatcherFunc) Dispatch(ctx context.Context, agentID string, req *pb.TaskRequest) (*pb.TaskResult, error) {
	return f(ctx, agentID, req)
}
