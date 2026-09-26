package controller

import (
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
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
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
	reg     *Registry
}

func newExternalTestEnv(t *testing.T, leaderElection, isLeader bool) *externalTestEnv {
	t.Helper()

	reg := NewRegistry(30 * time.Second)
	reg.Register(model.AgentInfo{ID: "agent-a", NodeName: "node-a", PodIP: "10.0.0.1"})
	reg.Register(model.AgentInfo{ID: "agent-b", NodeName: "node-b", PodIP: "10.0.0.2"})

	m := metrics.NewPrometheusMetrics("test", prometheus.NewRegistry())
	srv := NewGRPCServer(reg, m, false, nil, false)
	h := NewExternalChecksHandler(reg, srv.ExternalCheckManager(), m, leaderElection, func() bool { return isLeader })

	return &externalTestEnv{srv: srv, handler: h, mgr: srv.ExternalCheckManager(), metrics: m, reg: reg}
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

// A PUT that CHANGES a briefly unknown agent's specs (a definition added or deleted while it was
// evicted) stores the new specs, not the old ones: the Console records this body as pushed and does not
// re-PUT it for 2 minutes, so a kept stale list meant probing a deleted target, or never starting a new
// one, for that long. An open stream gets the change like any other.
func TestExternalPutStoresNewSpecsOfBrieflyUnknownAgent(t *testing.T) {
	env := newExternalTestEnv(t, false, true)

	stream, _ := env.watch(t, "agent-a")
	_ = recvAssignment(t, stream) // initial empty
	if w := env.put(oneSpecBody); w.Code != http.StatusOK {
		t.Fatalf("first PUT: expected 200, got %d", w.Code)
	}
	_ = recvAssignment(t, stream)

	env.reg.SetTTL(time.Nanosecond)
	time.Sleep(time.Millisecond)
	if env.reg.EvictStale() == 0 {
		t.Fatal("setup: nothing was evicted")
	}

	changed := `{"agents": {"agent-a": [
	  {"definitionId":"def-2","target":{"name":"t2","kind":"host","address":"1.1.1.1","port":443},"checkType":"tcp","intervalNs":30000000000,"timeoutNs":5000000000},
	  {"definitionId":"def-3","target":{"name":"t3","kind":"host","address":"9.9.9.9"},"checkType":"icmp","intervalNs":30000000000,"timeoutNs":5000000000}
	]}}`
	w := env.put(changed)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT naming the evicted agent: expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var resp externalChecksResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if len(resp.Unknown) != 1 || resp.Unknown[0] != "agent-a" {
		t.Errorf("expected agent-a reported as unknown, got %v", resp.Unknown)
	}
	ids := func(a *pb.ExternalCheckAssignment) []string {
		out := make([]string, 0, len(a.GetSpecs()))
		for _, s := range a.GetSpecs() {
			out = append(out, s.GetDefinitionId())
		}
		return out
	}
	if got := ids(env.mgr.Assignment("agent-a")); len(got) != 2 || got[0] != "def-2" || got[1] != "def-3" {
		t.Errorf("a re-subscribing agent-a gets %v, want [def-2 def-3] from the latest PUT", got)
	}
	if got := ids(recvAssignment(t, stream)); len(got) != 2 {
		t.Errorf("the open stream got %v, want the changed specs", got)
	}
}

// An agent the registry dropped between the Console's topology read and its PUT (a missed heartbeat)
// keeps its assignment: the Console's next ticks compute the same desired state and do not re-PUT
// until the 2-minute resync, so wiping it here left the agent probing nothing for that long.
func TestExternalPutKeepsAssignmentOfBrieflyUnknownAgent(t *testing.T) {
	env := newExternalTestEnv(t, false, true)

	stream, _ := env.watch(t, "agent-a")
	_ = recvAssignment(t, stream) // initial empty
	if w := env.put(oneSpecBody); w.Code != http.StatusOK {
		t.Fatalf("first PUT: expected 200, got %d", w.Code)
	}
	if got := recvAssignment(t, stream); len(got.GetSpecs()) != 1 {
		t.Fatalf("expected the assignment push, got %d specs", len(got.GetSpecs()))
	}

	env.reg.SetTTL(time.Nanosecond)
	time.Sleep(time.Millisecond)
	if env.reg.EvictStale() == 0 {
		t.Fatal("setup: nothing was evicted")
	}

	w := env.put(oneSpecBody)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT naming the evicted agent: expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var resp externalChecksResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if len(resp.Unknown) != 1 || resp.Unknown[0] != "agent-a" {
		t.Errorf("expected agent-a reported as unknown, got %v", resp.Unknown)
	}
	select {
	case a := <-stream.sent:
		t.Fatalf("the briefly unknown agent was pushed %d specs; its assignment must be kept", len(a.GetSpecs()))
	case <-time.After(200 * time.Millisecond):
	}
	if got := env.mgr.AssignedCount(); got != 1 {
		t.Errorf("AssignedCount = %d, want 1: the kept assignment still counts", got)
	}
	// A stream that broke with the heartbeat re-subscribes to the kept assignment, not an empty one.
	if got := env.mgr.Assignment("agent-a"); len(got.GetSpecs()) != 1 {
		t.Errorf("a re-subscribing agent-a gets %d specs, want 1", len(got.GetSpecs()))
	}

	// Leaving it out of the body still removes it: the Console's next topology no longer has it.
	if w := env.put(`{"agents": {}}`); w.Code != http.StatusOK {
		t.Fatalf("removal PUT: expected 200, got %d", w.Code)
	}
	if got := recvAssignment(t, stream); len(got.GetSpecs()) != 0 {
		t.Fatalf("expected an EMPTY assignment after removal, got %d specs", len(got.GetSpecs()))
	}
	if got := env.mgr.AssignedCount(); got != 0 {
		t.Errorf("AssignedCount = %d, want 0 after removal", got)
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

	for i := range externalSubscriberBuffer + 5 {
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

// oneTCPSpec is a single tcp spec whose address makes it distinct.
func oneTCPSpec(addr string) []*pb.ExternalCheckSpec {
	return []*pb.ExternalCheckSpec{{DefinitionId: "d", CheckType: "tcp",
		Target: &pb.ExternalTarget{Name: "t", Kind: "host", Address: addr}}}
}

// fillExternalBuffer queues externalSubscriberBuffer distinct assignments the subscriber has not
// read, so the next push to it cannot be queued.
func fillExternalBuffer(m *ExternalCheckManager, agentID string) {
	for i := range externalSubscriberBuffer {
		m.Apply(map[string][]*pb.ExternalCheckSpec{agentID: oneTCPSpec(string(rune('a' + i)))})
	}
}

// drainExternal reads n queued assignments and returns the last one.
func drainExternal(t *testing.T, ch <-chan *pb.ExternalCheckAssignment, n int) *pb.ExternalCheckAssignment {
	t.Helper()
	var last *pb.ExternalCheckAssignment
	for range n {
		select {
		case last = <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out draining queued assignments")
		}
	}
	return last
}

// A push that could not be queued is re-pushed by the next PUT even though the desired state has
// not changed since: the console's periodic re-PUT is the recovery path.
func TestExternalDroppedUpdateIsRePushed(t *testing.T) {
	m := NewExternalCheckManager()
	ch, cleanup := m.Subscribe("agent-a")
	defer cleanup()

	fillExternalBuffer(m, "agent-a")
	final := map[string][]*pb.ExternalCheckSpec{"agent-a": oneTCPSpec("final")}
	m.Apply(final)
	drainExternal(t, ch, externalSubscriberBuffer)

	if changed := m.Apply(final); changed != 1 {
		t.Errorf("changed = %d on the re-push, want 1", changed)
	}
	select {
	case a := <-ch:
		if got := a.GetSpecs()[0].GetTarget().GetAddress(); got != "final" {
			t.Fatalf("re-push carries address %q, want final", got)
		}
	default:
		t.Fatal("the dropped update was never re-pushed")
	}
	if got := m.AssignedCount(); got != 1 {
		t.Errorf("AssignedCount = %d while the drop was pending, want 1", got)
	}
}

// The same holds for a removal: the agent must get its empty assignment, or it keeps probing
// definitions the console deleted.
func TestExternalDroppedRemovalIsRePushed(t *testing.T) {
	m := NewExternalCheckManager()
	ch, cleanup := m.Subscribe("agent-a")
	defer cleanup()

	fillExternalBuffer(m, "agent-a")
	m.Apply(map[string][]*pb.ExternalCheckSpec{})
	if last := drainExternal(t, ch, externalSubscriberBuffer); len(last.GetSpecs()) == 0 {
		t.Fatal("setup: the last queued assignment is already empty")
	}

	changed := m.Apply(map[string][]*pb.ExternalCheckSpec{})
	select {
	case a := <-ch:
		if len(a.GetSpecs()) != 0 {
			t.Fatalf("re-push carries %d specs, want none", len(a.GetSpecs()))
		}
	default:
		t.Fatalf("the dropped removal was never re-pushed (changed=%d)", changed)
	}

	// Delivered once: the next identical PUT is a no-op again.
	if changed := m.Apply(map[string][]*pb.ExternalCheckSpec{}); changed != 0 {
		t.Errorf("changed = %d after the re-push was delivered, want 0", changed)
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

	for range workers {
		wg.Go(func() {
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
		})
	}

	for i := range rounds {
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

// PUT /api/v1/external-checks carries the whole desired state, so its cap is sized for a large fleet
// rather than for one request; past it the body is refused with 413 instead of being buffered whole.
func TestExternalPutRefusesAnOversizedBody(t *testing.T) {
	env := newExternalTestEnv(t, false, false)

	huge := `{"agents":{"agent-a":[{"definitionId":"` + strings.Repeat("d", maxExternalChecksBodyBytes) +
		`","target":{"name":"x","kind":"host","address":"1.1.1.1"},"checkType":"tcp","intervalNs":1,"timeoutNs":1}]}}`
	w := env.put(huge)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("body past the cap: got %d (%.200s), want 413", w.Code, w.Body.String())
	}
	if env.mgr.AssignedCount() != 0 {
		t.Error("an oversized PUT changed the assignment state")
	}

	// A desired state of a few thousand specs is well under the cap.
	var b strings.Builder
	b.WriteString(`{"agents":{"agent-a":[`)
	for i := range 4000 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"definitionId":"def-%06d","target":{"name":"t%d","kind":"host","address":"10.0.%d.%d","port":443},`+
			`"checkType":"tcp","intervalNs":30000000000,"timeoutNs":5000000000}`, i, i, i/250, i%250)
	}
	b.WriteString(`]}}`)
	if w := env.put(b.String()); w.Code != http.StatusOK {
		t.Fatalf("PUT of 4000 specs (%d bytes): got %d (%.200s), want 200", b.Len(), w.Code, w.Body.String())
	}
}

// The console sheds definitions until its PUT fits its own copy of the limit, so a smaller limit
// here would refuse every PUT the console believes it trimmed enough.
func TestExternalBodyLimitMatchesTheConsole(t *testing.T) {
	if maxExternalChecksBodyBytes != controllerclient.MaxExternalChecksBodyBytes {
		t.Fatalf("maxExternalChecksBodyBytes = %d, console controllerclient.MaxExternalChecksBodyBytes = %d",
			maxExternalChecksBodyBytes, controllerclient.MaxExternalChecksBodyBytes)
	}
}
