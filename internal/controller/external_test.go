package controller

import (
	"context"
	"encoding/json"
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
	"google.golang.org/grpc"
)

// fakeExternalStream implements grpc.ServerStreamingServer[pb.ExternalCheckAssignment]
// for exercising WatchExternalChecks without a real gRPC transport. Mirrors
// fakeTaskStream in grpc_server_test.go.
type fakeExternalStream struct {
	grpc.ServerStream
	ctx  context.Context
	sent chan *pb.ExternalCheckAssignment
}

func newFakeExternalStream(ctx context.Context) *fakeExternalStream {
	return &fakeExternalStream{ctx: ctx, sent: make(chan *pb.ExternalCheckAssignment, 16)}
}

func (f *fakeExternalStream) Context() context.Context                 { return f.ctx }
func (f *fakeExternalStream) Send(a *pb.ExternalCheckAssignment) error { f.sent <- a; return nil }

// externalTestEnv bundles the pieces every external-checks test needs.
type externalTestEnv struct {
	srv     *GRPCServer
	handler *ExternalChecksHandler
	mgr     *ExternalCheckManager
	metrics *metrics.PrometheusMetrics
}

func newExternalTestEnv(t *testing.T, leaderElection, isLeader bool) *externalTestEnv {
	t.Helper()

	reg := NewRegistry(30 * time.Second)
	reg.Register(model.AgentInfo{ID: "agent-a", NodeName: "node-a", PodIP: "10.0.0.1"})
	reg.Register(model.AgentInfo{ID: "agent-b", NodeName: "node-b", PodIP: "10.0.0.2"})

	m := metrics.NewPrometheusMetrics("test", prometheus.NewRegistry())
	srv := NewGRPCServer(reg, m, false, nil, false)
	h := NewExternalChecksHandler(reg, srv.ExternalCheckManager(), m, leaderElection, func() bool { return isLeader })

	return &externalTestEnv{srv: srv, handler: h, mgr: srv.ExternalCheckManager(), metrics: m}
}

func (e *externalTestEnv) put(body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut,
		"/api/v1/external-checks", strings.NewReader(body))
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, req)
	return w
}

// watch starts WatchExternalChecks on a goroutine and returns the fake stream
// plus a cancel that ends the RPC. The handler's return is drained by the test
// helper's cleanup so a leaked goroutine fails -race runs loudly.
func (e *externalTestEnv) watch(t *testing.T, agentID string) (*fakeExternalStream, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	stream := newFakeExternalStream(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e.srv.WatchExternalChecks(&pb.WatchExternalChecksRequest{AgentId: agentID}, stream)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return stream, cancel
}

func recvAssignment(t *testing.T, stream *fakeExternalStream) *pb.ExternalCheckAssignment {
	t.Helper()
	select {
	case a := <-stream.sent:
		return a
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an ExternalCheckAssignment")
		return nil
	}
}

const oneSpecBody = `{
  "agents": {
    "agent-a": [
      {
        "definitionId": "def-1",
        "target": {"name": "dns-root", "kind": "host", "address": "8.8.8.8", "port": 53},
        "checkType": "dns",
        "intervalNs": 30000000000,
        "timeoutNs": 5000000000,
        "params": {"query": "example.com"}
      }
    ]
  }
}`

// 1. PUT then subscribe -> the initial send carries the stored assignment.
func TestExternalPutThenSubscribeInitialSend(t *testing.T) {
	env := newExternalTestEnv(t, false, true)

	if w := env.put(oneSpecBody); w.Code != http.StatusOK {
		t.Fatalf("expected 200 from PUT, got %d (%s)", w.Code, w.Body.String())
	}

	stream, _ := env.watch(t, "agent-a")
	got := recvAssignment(t, stream)

	if len(got.GetSpecs()) != 1 {
		t.Fatalf("expected 1 spec on the initial send, got %d", len(got.GetSpecs()))
	}
	spec := got.GetSpecs()[0]
	if spec.GetDefinitionId() != "def-1" {
		t.Errorf("definitionId = %q, want def-1", spec.GetDefinitionId())
	}
	if spec.GetCheckType() != "dns" {
		t.Errorf("checkType = %q, want dns", spec.GetCheckType())
	}
	if spec.GetTarget().GetAddress() != "8.8.8.8" || spec.GetTarget().GetPort() != 53 {
		t.Errorf("target = %+v, want 8.8.8.8:53", spec.GetTarget())
	}
	if spec.GetIntervalNs() != 30000000000 || spec.GetTimeoutNs() != 5000000000 {
		t.Errorf("interval/timeout = %d/%d", spec.GetIntervalNs(), spec.GetTimeoutNs())
	}
	var params map[string]any
	if err := json.Unmarshal(spec.GetParamsJson(), &params); err != nil {
		t.Fatalf("params_json is not valid JSON: %v", err)
	}
	if params["query"] != "example.com" {
		t.Errorf("params = %v, want query=example.com", params)
	}
	if got.GetTimestamp() == nil {
		t.Error("expected the assignment to be stamped with a timestamp")
	}
}

// An agent with no assignment still gets an initial (empty) send, so a
// restarting agent converges without waiting for a push.
func TestExternalSubscribeWithoutAssignmentSendsEmpty(t *testing.T) {
	env := newExternalTestEnv(t, false, true)

	stream, _ := env.watch(t, "agent-b")
	got := recvAssignment(t, stream)

	if len(got.GetSpecs()) != 0 {
		t.Fatalf("expected an empty initial assignment, got %d specs", len(got.GetSpecs()))
	}
}

// 2. Subscribe then PUT -> the push arrives on the open stream.
func TestExternalSubscribeThenPutPushes(t *testing.T) {
	env := newExternalTestEnv(t, false, true)

	stream, _ := env.watch(t, "agent-a")
	if initial := recvAssignment(t, stream); len(initial.GetSpecs()) != 0 {
		t.Fatalf("expected an empty initial assignment, got %d specs", len(initial.GetSpecs()))
	}

	if w := env.put(oneSpecBody); w.Code != http.StatusOK {
		t.Fatalf("expected 200 from PUT, got %d (%s)", w.Code, w.Body.String())
	}

	pushed := recvAssignment(t, stream)
	if len(pushed.GetSpecs()) != 1 || pushed.GetSpecs()[0].GetDefinitionId() != "def-1" {
		t.Fatalf("unexpected pushed assignment: %+v", pushed)
	}
}

// 3. A non-leader replica answers 503, the same branch as diagnostics.
func TestExternalPutNonLeader503(t *testing.T) {
	env := newExternalTestEnv(t, true, false)

	w := env.put(oneSpecBody)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 from a non-leader, got %d", w.Code)
	}
	if env.mgr.AssignedCount() != 0 {
		t.Error("a non-leader must not mutate assignment state")
	}
}

// 4. An unknown agent id is ignored with a warning, never a 400.
func TestExternalPutUnknownAgentIgnored(t *testing.T) {
	env := newExternalTestEnv(t, false, true)

	body := `{"agents": {"agent-ghost": [{"definitionId":"d","target":{"name":"t","kind":"host","address":"1.1.1.1"},"checkType":"tcp","intervalNs":1,"timeoutNs":1}]}}`
	w := env.put(body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for an unknown agent id, got %d (%s)", w.Code, w.Body.String())
	}

	var resp externalChecksResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if len(resp.Unknown) != 1 || resp.Unknown[0] != "agent-ghost" {
		t.Errorf("expected agent-ghost reported as unknown, got %v", resp.Unknown)
	}
	if env.mgr.AssignedCount() != 0 {
		t.Errorf("unknown agent must not be stored, AssignedCount = %d", env.mgr.AssignedCount())
	}
}

// 5. An agent dropped from a subsequent PUT gets an EMPTY assignment pushed:
// deletion has to converge, not just stop being re-sent.
func TestExternalRemovedAgentGetsEmptyAssignment(t *testing.T) {
	env := newExternalTestEnv(t, false, true)

	stream, _ := env.watch(t, "agent-a")
	_ = recvAssignment(t, stream) // initial empty

	if w := env.put(oneSpecBody); w.Code != http.StatusOK {
		t.Fatalf("first PUT: expected 200, got %d", w.Code)
	}
	if got := recvAssignment(t, stream); len(got.GetSpecs()) != 1 {
		t.Fatalf("expected the assignment push, got %d specs", len(got.GetSpecs()))
	}
	if env.mgr.AssignedCount() != 1 {
		t.Fatalf("AssignedCount = %d, want 1", env.mgr.AssignedCount())
	}

	// agent-a is gone from the desired state entirely.
	if w := env.put(`{"agents": {}}`); w.Code != http.StatusOK {
		t.Fatalf("second PUT: expected 200, got %d", w.Code)
	}

	empty := recvAssignment(t, stream)
	if len(empty.GetSpecs()) != 0 {
		t.Fatalf("expected an EMPTY assignment after removal, got %d specs", len(empty.GetSpecs()))
	}
	if env.mgr.AssignedCount() != 0 {
		t.Errorf("AssignedCount = %d, want 0 after removal", env.mgr.AssignedCount())
	}
	if got := testutil.ToFloat64(env.metrics.ControllerExternalAssignments.WithLabelValues()); got != 0 {
		t.Errorf("assignments gauge = %v, want 0", got)
	}
}

// An agent present in the body with an EMPTY spec list converges exactly like
// an absent one.
func TestExternalEmptySpecListEqualsRemoval(t *testing.T) {
	env := newExternalTestEnv(t, false, true)

	stream, _ := env.watch(t, "agent-a")
	_ = recvAssignment(t, stream)

	env.put(oneSpecBody)
	_ = recvAssignment(t, stream)

	if w := env.put(`{"agents": {"agent-a": []}}`); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := recvAssignment(t, stream); len(got.GetSpecs()) != 0 {
		t.Fatalf("expected an empty assignment, got %d specs", len(got.GetSpecs()))
	}
	if env.mgr.AssignedCount() != 0 {
		t.Errorf("AssignedCount = %d, want 0", env.mgr.AssignedCount())
	}
}

// 6. An identical PUT is a no-op: no second push. This is what lets
// controllerclient retry a 503 without disturbing agents.
func TestExternalIdenticalPutNoSecondPush(t *testing.T) {
	env := newExternalTestEnv(t, false, true)

	stream, _ := env.watch(t, "agent-a")
	_ = recvAssignment(t, stream) // initial empty

	env.put(oneSpecBody)
	_ = recvAssignment(t, stream) // the one real change

	w := env.put(oneSpecBody)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on the retried PUT, got %d", w.Code)
	}
	var resp externalChecksResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if resp.Changed != 0 {
		t.Errorf("changed = %d on an identical PUT, want 0", resp.Changed)
	}

	select {
	case extra := <-stream.sent:
		t.Fatalf("identical PUT pushed again: %+v", extra)
	case <-time.After(200 * time.Millisecond):
	}
}

// Key ordering inside params must not fake a change either: the manager
// canonicalizes params before comparing.
func TestExternalParamsKeyOrderIsNotAChange(t *testing.T) {
	env := newExternalTestEnv(t, false, true)

	stream, _ := env.watch(t, "agent-a")
	_ = recvAssignment(t, stream)

	first := `{"agents":{"agent-a":[{"definitionId":"d","target":{"name":"t","kind":"host","address":"1.1.1.1"},"checkType":"http","intervalNs":1,"timeoutNs":1,"params":{"a":1,"b":2}}]}}`
	second := `{"agents":{"agent-a":[{"definitionId":"d","target":{"name":"t","kind":"host","address":"1.1.1.1"},"checkType":"http","intervalNs":1,"timeoutNs":1,"params":{"b":2,"a":1}}]}}`

	env.put(first)
	_ = recvAssignment(t, stream)

	env.put(second)
	select {
	case extra := <-stream.sent:
		t.Fatalf("reordered params counted as a change: %+v", extra)
	case <-time.After(200 * time.Millisecond):
	}
}

// 7. A subscriber whose stream is gone must not block the fan-out to others.
func TestExternalStalledSubscriberDoesNotBlockOthers(t *testing.T) {
	env := newExternalTestEnv(t, false, true)

	// agent-a subscribes directly on the manager and never reads: its buffer
	// fills and the manager must drop rather than block.
	stalled, stalledCleanup := env.mgr.Subscribe("agent-a")
	defer stalledCleanup()
	_ = stalled

	live, _ := env.watch(t, "agent-b")
	_ = recvAssignment(t, live)

	body := `{"agents":{"agent-a":[{"definitionId":"a","target":{"name":"t","kind":"host","address":"1.1.1.1"},"checkType":"tcp","intervalNs":1,"timeoutNs":1}],` +
		`"agent-b":[{"definitionId":"b","target":{"name":"t","kind":"host","address":"1.1.1.2"},"checkType":"tcp","intervalNs":1,"timeoutNs":1}]}}`

	for i := 0; i < externalSubscriberBuffer+5; i++ {
		// Alternate the spec so every PUT is a real change for both agents.
		alt := strings.Replace(body, "1.1.1.2", "1.1.1."+string(rune('0'+i%10)), 1)
		done := make(chan int, 1)
		go func() {
			done <- env.put(alt).Code
		}()
		select {
		case code := <-done:
			if code != http.StatusOK {
				t.Fatalf("PUT %d returned %d", i, code)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("PUT %d blocked on the stalled subscriber", i)
		}
		// Keep the live subscriber draining so it never stalls itself.
		select {
		case <-live.sent:
		case <-time.After(2 * time.Second):
			t.Fatalf("live subscriber did not receive push %d", i)
		}
	}
}

// 8. Concurrent subscribe / cleanup / push. Under -race this is the tasks.go
// send-on-closed-channel race: the manager must never close a subscriber
// channel, only delete the map entry.
func TestExternalConcurrentSubscribeCleanupPush(t *testing.T) {
	env := newExternalTestEnv(t, false, true)

	const workers = 8
	const rounds = 200

	stop := make(chan struct{})
	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ch, cleanup := env.mgr.Subscribe("agent-a")
				select {
				case <-ch:
				default:
				}
				cleanup()
				cleanup() // idempotent
			}
		}()
	}

	for i := 0; i < rounds; i++ {
		specs := []*pb.ExternalCheckSpec{{
			DefinitionId: "d",
			CheckType:    "tcp",
			IntervalNs:   int64(i + 1),
			TimeoutNs:    1,
			Target:       &pb.ExternalTarget{Name: "t", Kind: "host", Address: "1.1.1.1"},
		}}
		env.mgr.Apply(map[string][]*pb.ExternalCheckSpec{"agent-a": specs})
	}

	close(stop)
	wg.Wait()
}

// 9. A check type outside tcp|icmp|dns|http is a 400 and changes nothing.
func TestExternalInvalidCheckType400(t *testing.T) {
	env := newExternalTestEnv(t, false, true)

	for _, ct := range []string{"udp", "mtr", "", "TCP"} {
		body := `{"agents":{"agent-a":[{"definitionId":"d","target":{"name":"t","kind":"host","address":"1.1.1.1"},"checkType":"` +
			ct + `","intervalNs":1,"timeoutNs":1}]}}`
		w := env.put(body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("checkType %q: expected 400, got %d (%s)", ct, w.Code, w.Body.String())
		}
	}
	if env.mgr.AssignedCount() != 0 {
		t.Errorf("a rejected PUT must not mutate state, AssignedCount = %d", env.mgr.AssignedCount())
	}
}

func TestExternalMalformedBody400(t *testing.T) {
	env := newExternalTestEnv(t, false, true)
	if w := env.put(`{"agents":`); w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for malformed JSON, got %d", w.Code)
	}
}

// The subscribers gauge tracks open WatchExternalChecks streams.
func TestExternalSubscribersGauge(t *testing.T) {
	env := newExternalTestEnv(t, false, true)

	stream, cancel := env.watch(t, "agent-a")
	_ = recvAssignment(t, stream)

	if got := testutil.ToFloat64(env.metrics.ControllerExternalSubscribers.WithLabelValues()); got != 1 {
		t.Errorf("subscribers gauge = %v, want 1", got)
	}

	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if testutil.ToFloat64(env.metrics.ControllerExternalSubscribers.WithLabelValues()) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("subscribers gauge did not return to 0 after the stream ended")
}

// The assignments gauge counts AGENTS with a non-empty assignment, never specs
// and never per-agent series.
func TestExternalAssignmentsGauge(t *testing.T) {
	env := newExternalTestEnv(t, false, true)

	body := `{"agents":{` +
		`"agent-a":[{"definitionId":"a","target":{"name":"t","kind":"host","address":"1.1.1.1"},"checkType":"tcp","intervalNs":1,"timeoutNs":1},` +
		`{"definitionId":"a2","target":{"name":"t2","kind":"host","address":"1.1.1.3"},"checkType":"icmp","intervalNs":1,"timeoutNs":1}],` +
		`"agent-b":[{"definitionId":"b","target":{"name":"t","kind":"host","address":"1.1.1.2"},"checkType":"http","intervalNs":1,"timeoutNs":1}]}}`

	if w := env.put(body); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if got := testutil.ToFloat64(env.metrics.ControllerExternalAssignments.WithLabelValues()); got != 2 {
		t.Errorf("assignments gauge = %v, want 2 (agents, not specs)", got)
	}
}

// The HTTP route is wired and hot-injection gates it before the handler exists.
func TestExternalChecksRouteWiring(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	srv := NewHTTPServer(reg, nil, prometheus.NewRegistry(), nil)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut,
		"/api/v1/external-checks", strings.NewReader(`{"agents":{}}`))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 before SetExternalChecksHandler, got %d", w.Code)
	}

	m := metrics.NewPrometheusMetrics("test", prometheus.NewRegistry())
	grpcSrv := NewGRPCServer(reg, m, false, nil, false)
	srv.SetExternalChecksHandler(NewExternalChecksHandler(
		reg, grpcSrv.ExternalCheckManager(), m, false, func() bool { return true }))

	req = httptest.NewRequestWithContext(context.Background(), http.MethodPut,
		"/api/v1/external-checks", strings.NewReader(`{"agents":{}}`))
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 once the handler is injected, got %d (%s)", w.Code, w.Body.String())
	}
}
