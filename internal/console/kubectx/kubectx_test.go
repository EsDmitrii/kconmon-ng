package kubectx

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	apitypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

const testNamespace = "kconmon"

// fakeSink is the store seam. reply, when set, decides the (inserted, error)
// outcome per call, which is how the dedupe and store-error cases are driven
// without a database.
type fakeSink struct {
	mu    sync.Mutex
	got   []store.K8sEventInput
	reply func(in store.K8sEventInput) (bool, error)
}

func (s *fakeSink) InsertK8sEvent(_ context.Context, in store.K8sEventInput) (bool, error) {
	s.mu.Lock()
	s.got = append(s.got, in)
	reply := s.reply
	s.mu.Unlock()
	if reply == nil {
		return true, nil
	}
	return reply(in)
}

func (s *fakeSink) inputs() []store.K8sEventInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.K8sEventInput(nil), s.got...)
}

// fakeTopology is a TopologySource pinned to a fixed node set, with a call
// counter so the "refreshed at most every 30s" claim can be asserted rather
// than assumed.
type fakeTopology struct {
	mu    sync.Mutex
	nodes []string
	err   error
	calls int
}

func (f *fakeTopology) Topology(_ context.Context) (*controllerclient.Topology, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	snap := &controllerclient.Topology{Timestamp: time.Now()}
	for _, n := range f.nodes {
		snap.Nodes = append(snap.Nodes, controllerclient.Node{Name: n, Zone: "z", Ready: true})
	}
	return snap, nil
}

func (f *fakeTopology) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func podEvent(name, eventNS, podNS, pod, rv string) *corev1.Event {
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: eventNS, UID: apitypes.UID("uid-" + name), ResourceVersion: rv,
		},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: pod, Namespace: podNS},
		Reason:         "Killing",
		Type:           "Normal",
		Message:        "Stopping container agent",
		Count:          1,
		LastTimestamp:  metav1.NewTime(time.Now().Truncate(time.Second)),
	}
}

func nodeEvent(name, node, rv string) *corev1.Event {
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			// Node events are cluster-scoped objects, but the Event that
			// describes them is itself namespaced and lands in "default".
			Name: name, Namespace: "default", UID: apitypes.UID("uid-" + name), ResourceVersion: rv,
		},
		InvolvedObject: corev1.ObjectReference{Kind: "Node", Name: node},
		Reason:         "NodeNotReady",
		Type:           "Warning",
		Message:        "Node is not ready",
		Count:          2,
		LastTimestamp:  metav1.NewTime(time.Now().Truncate(time.Second)),
	}
}

func newReader(t *testing.T, cs *fake.Clientset, topo TopologySource, sink store.K8sEventStore) (*Reader, *metrics.Metrics) {
	t.Helper()
	m := metrics.New("kconmon_ng", prometheus.NewRegistry())
	cfg := config.KubernetesContextConfig{
		Enabled:        true,
		Namespace:      testNamespace,
		ResyncInterval: time.Hour, // no periodic relist inside a unit test
	}
	r := New(cfg, cs, topo, sink, m)
	// Production backoff is 1s..30s; a unit test cannot wait for it and must
	// not busy-loop either, so the same schedule runs a thousand times faster.
	r.minBackoff = time.Millisecond
	r.maxBackoff = 5 * time.Millisecond
	return r, m
}

func counts(m *metrics.Metrics) map[string]float64 {
	out := make(map[string]float64, 4)
	for _, result := range []string{resultStored, resultDuplicate, resultFiltered, resultError} {
		out[result] = testutil.ToFloat64(m.K8sEvents.WithLabelValues(result))
	}
	return out
}

func total(m *metrics.Metrics) float64 {
	var sum float64
	for _, v := range counts(m) {
		sum += v
	}
	return sum
}

// runUntil starts the reader, waits for want decisions to be counted, proves
// no further decision arrives, then cancels and requires Run to return.
func runUntil(t *testing.T, r *Reader, m *metrics.Metrics, want float64) map[string]float64 {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()

	deadline := time.Now().Add(10 * time.Second)
	for total(m) < want && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	// Settle: a reader that double-counts (or relists in a hot loop) shows up
	// as extra decisions here, and every assertion below would otherwise pass.
	time.Sleep(150 * time.Millisecond)
	got := counts(m)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s of context cancellation")
	}
	if sum := got[resultStored] + got[resultDuplicate] + got[resultFiltered] + got[resultError]; sum != want {
		t.Fatalf("decisions counted = %v, want exactly %v (%v)", sum, want, got)
	}
	return got
}

// TestFilterMatrix is Decision 3 as a table. The node half FAILS CLOSED: a node
// event is kept only when the topology vouches for the node, and no topology at
// all means no node events -- never "keep them, the controller is down".
func TestFilterMatrix(t *testing.T) {
	for _, tc := range []struct {
		name         string
		objects      []runtime.Object
		topo         TopologySource
		wantStored   float64
		wantFiltered float64
		wantNames    []string
	}{
		{
			name:         "node in topology is kept, pod in namespace is kept",
			objects:      []runtime.Object{nodeEvent("n1", "node-a", "10"), podEvent("p1", testNamespace, testNamespace, "agent-x", "11")},
			topo:         &fakeTopology{nodes: []string{"node-a", "node-b"}},
			wantStored:   2,
			wantFiltered: 0,
			wantNames:    []string{"agent-x", "node-a"},
		},
		{
			name:         "node outside the topology is filtered",
			objects:      []runtime.Object{nodeEvent("n1", "node-z", "10"), podEvent("p1", testNamespace, testNamespace, "agent-x", "11")},
			topo:         &fakeTopology{nodes: []string{"node-a"}},
			wantStored:   1,
			wantFiltered: 1,
			wantNames:    []string{"agent-x"},
		},
		{
			name:         "topology unreadable drops node events and keeps pod events",
			objects:      []runtime.Object{nodeEvent("n1", "node-a", "10"), podEvent("p1", testNamespace, testNamespace, "agent-x", "11")},
			topo:         &fakeTopology{err: errors.New("controller unreachable")},
			wantStored:   1,
			wantFiltered: 1,
			wantNames:    []string{"agent-x"},
		},
		{
			name:         "no topology source at all drops node events",
			objects:      []runtime.Object{nodeEvent("n1", "node-a", "10"), podEvent("p1", testNamespace, testNamespace, "agent-x", "11")},
			topo:         nil,
			wantStored:   1,
			wantFiltered: 1,
			wantNames:    []string{"agent-x"},
		},
		{
			name:         "pod event in another namespace is never seen",
			objects:      []runtime.Object{podEvent("p1", "other", "other", "stranger", "10"), podEvent("p2", testNamespace, testNamespace, "agent-x", "11")},
			topo:         &fakeTopology{nodes: []string{"node-a"}},
			wantStored:   1,
			wantFiltered: 0,
			wantNames:    []string{"agent-x"},
		},
		{
			name: "pod event whose involvedObject is in another namespace is filtered",
			objects: []runtime.Object{
				podEvent("p1", testNamespace, "other", "stranger", "10"),
				podEvent("p2", testNamespace, testNamespace, "agent-x", "11"),
			},
			topo:         &fakeTopology{nodes: []string{"node-a"}},
			wantStored:   1,
			wantFiltered: 1,
			wantNames:    []string{"agent-x"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &fakeSink{}
			r, m := newReader(t, fake.NewClientset(tc.objects...), tc.topo, sink)
			got := runUntil(t, r, m, tc.wantStored+tc.wantFiltered)

			if got[resultStored] != tc.wantStored {
				t.Errorf("stored = %v, want %v", got[resultStored], tc.wantStored)
			}
			if got[resultFiltered] != tc.wantFiltered {
				t.Errorf("filtered = %v, want %v", got[resultFiltered], tc.wantFiltered)
			}
			names := make(map[string]bool)
			for _, in := range sink.inputs() {
				names[in.Name] = true
			}
			if len(names) != len(tc.wantNames) {
				t.Fatalf("stored names = %v, want %v", names, tc.wantNames)
			}
			for _, want := range tc.wantNames {
				if !names[want] {
					t.Errorf("stored names = %v, missing %q", names, want)
				}
			}
		})
	}
}

// TestDuplicateRevisionIsCountedNotLogged: inserted=false with a nil error is
// the relist's normal outcome, and it must be its own metric value rather than
// an error or a silent success.
func TestDuplicateRevisionIsCountedNotLogged(t *testing.T) {
	sink := &fakeSink{reply: func(store.K8sEventInput) (bool, error) { return false, nil }}
	cs := fake.NewClientset(podEvent("p1", testNamespace, testNamespace, "agent-x", "10"))
	r, m := newReader(t, cs, &fakeTopology{}, sink)

	got := runUntil(t, r, m, 1)
	if got[resultDuplicate] != 1 {
		t.Errorf("duplicate = %v, want 1 (%v)", got[resultDuplicate], got)
	}
	if got[resultStored] != 0 || got[resultError] != 0 {
		t.Errorf("a duplicate must not count as stored or error, got %v", got)
	}
}

// TestStoreErrorIsCounted: a rejected row is neither stored nor filtered.
func TestStoreErrorIsCounted(t *testing.T) {
	sink := &fakeSink{reply: func(store.K8sEventInput) (bool, error) { return false, errors.New("boom") }}
	cs := fake.NewClientset(podEvent("p1", testNamespace, testNamespace, "agent-x", "10"))
	r, m := newReader(t, cs, &fakeTopology{}, sink)

	got := runUntil(t, r, m, 1)
	if got[resultError] != 1 {
		t.Errorf("error = %v, want 1 (%v)", got[resultError], got)
	}
}

// TestMessageTruncatedToOneKiB: an event message is operator-visible text of
// unbounded length written by anything in the cluster. It is cut to 1 KiB with
// a visible marker, so a truncated message reads as truncated rather than as a
// message that happened to end there.
func TestMessageTruncatedToOneKiB(t *testing.T) {
	ev := podEvent("p1", testNamespace, testNamespace, "agent-x", "10")
	ev.Message = strings.Repeat("a", 5000)
	sink := &fakeSink{}
	r, m := newReader(t, fake.NewClientset(ev), &fakeTopology{}, sink)

	runUntil(t, r, m, 1)

	in := sink.inputs()
	if len(in) != 1 {
		t.Fatalf("sink saw %d inputs, want 1", len(in))
	}
	if len(in[0].Message) > messageMaxLen {
		t.Errorf("message is %d bytes, want <= %d", len(in[0].Message), messageMaxLen)
	}
	if !strings.HasSuffix(in[0].Message, truncationMarker) {
		t.Errorf("truncated message must end with %q, got %q", truncationMarker, in[0].Message[len(in[0].Message)-10:])
	}
}

// TestTruncateMessageBoundaries pins the exact edges, including the one a byte
// slice gets wrong: cutting mid-rune would store invalid UTF-8 in a text
// column.
func TestTruncateMessageBoundaries(t *testing.T) {
	short := "kubelet restarted the container"
	if got := truncateMessage(short); got != short {
		t.Errorf("a short message must pass through unchanged, got %q", got)
	}
	exact := strings.Repeat("a", messageMaxLen)
	if got := truncateMessage(exact); got != exact {
		t.Errorf("a message of exactly %d bytes must pass through unchanged, got %d bytes", messageMaxLen, len(got))
	}
	multibyte := strings.Repeat("é", messageMaxLen) // 2 bytes per rune
	got := truncateMessage(multibyte)
	if len(got) > messageMaxLen {
		t.Errorf("truncated multibyte message is %d bytes, want <= %d", len(got), messageMaxLen)
	}
	if !strings.HasSuffix(got, truncationMarker) {
		t.Errorf("truncated message must end with %q", truncationMarker)
	}
	trimmed := strings.TrimSuffix(got, truncationMarker)
	for _, r := range trimmed {
		if r != 'é' {
			t.Fatalf("truncation cut mid-rune: found %q in %q", r, trimmed)
		}
	}
}

// TestEventTimeFallbackOrder pins the messy-timestamp order. core/v1 Events are
// written by many components across many versions: the modern events API fills
// EventTime, the legacy one fills First/LastTimestamp, and plenty of emitters
// fill some subset. LastTimestamp wins because "when did this last happen" is
// what a timeline row means.
func TestEventTimeFallbackOrder(t *testing.T) {
	last := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	micro := time.Date(2026, 8, 8, 11, 0, 0, 0, time.UTC)
	first := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name string
		ev   corev1.Event
		want time.Time
	}{
		{
			name: "lastTimestamp wins over everything",
			ev: corev1.Event{
				LastTimestamp:  metav1.NewTime(last),
				EventTime:      metav1.NewMicroTime(micro),
				FirstTimestamp: metav1.NewTime(first),
			},
			want: last,
		},
		{
			name: "eventTime is the second choice",
			ev: corev1.Event{
				EventTime:      metav1.NewMicroTime(micro),
				FirstTimestamp: metav1.NewTime(first),
			},
			want: micro,
		},
		{
			name: "firstTimestamp is the last resort",
			ev:   corev1.Event{FirstTimestamp: metav1.NewTime(first)},
			want: first,
		},
		{
			name: "nothing set stays zero so the store rejects it",
			ev:   corev1.Event{},
			want: time.Time{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := eventTimeOf(&tc.ev); !got.Equal(tc.want) {
				t.Errorf("eventTimeOf() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEventTimeFallbackReachesTheStore proves the order above is what the
// reader actually writes, not just what a helper computes.
func TestEventTimeFallbackReachesTheStore(t *testing.T) {
	micro := time.Date(2026, 8, 8, 11, 0, 0, 0, time.UTC)
	ev := podEvent("p1", testNamespace, testNamespace, "agent-x", "10")
	ev.LastTimestamp = metav1.Time{}
	ev.EventTime = metav1.NewMicroTime(micro)

	sink := &fakeSink{}
	r, m := newReader(t, fake.NewClientset(ev), &fakeTopology{}, sink)
	runUntil(t, r, m, 1)

	in := sink.inputs()
	if len(in) != 1 {
		t.Fatalf("sink saw %d inputs, want 1", len(in))
	}
	if !in[0].EventTime.Equal(micro) {
		t.Errorf("EventTime = %v, want %v", in[0].EventTime, micro)
	}
}

// TestMappedFieldsReachTheStore pins the projection itself: the dedupe key, the
// involved object's identity and the recurrence count.
func TestMappedFieldsReachTheStore(t *testing.T) {
	sink := &fakeSink{}
	cs := fake.NewClientset(nodeEvent("n1", "node-a", "42"))
	r, m := newReader(t, cs, &fakeTopology{nodes: []string{"node-a"}}, sink)
	runUntil(t, r, m, 1)

	in := sink.inputs()
	if len(in) != 1 {
		t.Fatalf("sink saw %d inputs, want 1", len(in))
	}
	got := in[0]
	if got.UID != "uid-n1" || got.ResourceVersion != "42" {
		t.Errorf("dedupe key = (%q, %q), want (uid-n1, 42)", got.UID, got.ResourceVersion)
	}
	if got.Kind != "Node" || got.Name != "node-a" {
		t.Errorf("involved object = %s/%s, want Node/node-a", got.Kind, got.Name)
	}
	if got.Namespace != "" {
		t.Errorf("a node event must carry an empty namespace, got %q", got.Namespace)
	}
	if got.Reason != "NodeNotReady" || got.Type != "Warning" {
		t.Errorf("reason/type = %q/%q, want NodeNotReady/Warning", got.Reason, got.Type)
	}
	if got.Count != 2 {
		t.Errorf("count = %d, want 2", got.Count)
	}
}

// TestTopologyIsRefreshedAtMostOncePerWindow: the node-name set is read from
// the controller, so a per-event read would turn an event storm into a
// controller DoS.
func TestTopologyIsRefreshedAtMostOncePerWindow(t *testing.T) {
	objs := make([]runtime.Object, 0, 6)
	for i := range 6 {
		objs = append(objs, nodeEvent(string(rune('a'+i)), "node-a", string(rune('1'+i))))
	}
	topo := &fakeTopology{nodes: []string{"node-a"}}
	sink := &fakeSink{}
	r, m := newReader(t, fake.NewClientset(objs...), topo, sink)

	got := runUntil(t, r, m, 6)
	if got[resultStored] != 6 {
		t.Errorf("stored = %v, want 6 (%v)", got[resultStored], got)
	}
	if calls := topo.callCount(); calls != 1 {
		t.Errorf("topology read %d times for 6 node events, want exactly 1 within the %v window",
			calls, topologyRefreshInterval)
	}
}

// TestNodeSetIsRefreshedAfterTheWindow: the flip side -- a node that joins the
// fleet must stop being filtered without a console restart.
func TestNodeSetIsRefreshedAfterTheWindow(t *testing.T) {
	topo := &fakeTopology{}
	sink := &fakeSink{}
	r, _ := newReader(t, fake.NewClientset(), topo, sink)

	clock := time.Now()
	var mu sync.Mutex
	r.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}

	ctx := context.Background()
	if r.nodeInTopology(ctx, "node-a") {
		t.Fatal("node-a must be filtered while the topology has no nodes")
	}
	topo.mu.Lock()
	topo.nodes = []string{"node-a"}
	topo.mu.Unlock()

	if r.nodeInTopology(ctx, "node-a") {
		t.Error("the cached node set must not be re-read inside the refresh window")
	}
	mu.Lock()
	clock = clock.Add(topologyRefreshInterval + time.Second)
	mu.Unlock()
	if !r.nodeInTopology(ctx, "node-a") {
		t.Error("a node that joined the fleet must be kept after the refresh window elapses")
	}
	if calls := topo.callCount(); calls != 2 {
		t.Errorf("topology read %d times, want 2 (one per window)", calls)
	}
}

// TestRelistOnWatchClose: a closed watch channel is the apiserver's ordinary
// "your resourceVersion is too old" signal. The reader must go back to a LIST,
// not sit on a dead channel -- the failure mode that makes a capture look alive
// while it silently stops capturing.
func TestRelistOnWatchClose(t *testing.T) {
	cs := fake.NewClientset(podEvent("p1", testNamespace, testNamespace, "agent-x", "10"))

	var mu sync.Mutex
	lists := 0
	watches := make([]*watch.RaceFreeFakeWatcher, 0, 4)

	cs.PrependReactor("list", "events", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == testNamespace {
			mu.Lock()
			lists++
			mu.Unlock()
		}
		return false, nil, nil // fall through to the tracker
	})
	cs.PrependWatchReactor("events", func(action k8stesting.Action) (bool, watch.Interface, error) {
		w := watch.NewRaceFreeFake()
		if action.GetNamespace() == testNamespace {
			mu.Lock()
			watches = append(watches, w)
			mu.Unlock()
		}
		return true, w, nil
	})

	r, m := newReader(t, cs, &fakeTopology{}, &fakeSink{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()

	waitFor(t, "the first list and watch", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return lists >= 1 && len(watches) >= 1
	})
	if got := total(m); got != 1 {
		t.Fatalf("decisions after the first list = %v, want 1", got)
	}

	mu.Lock()
	first := watches[0]
	mu.Unlock()
	first.Stop() // the apiserver hanging up

	waitFor(t, "the relist after the watch closed", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return lists >= 2 && len(watches) >= 2
	})

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s of context cancellation")
	}
	// The relisted event is the same revision, so it is a decision either way;
	// what must NOT happen is a second STORE of the same row.
	if got := counts(m); got[resultStored] > 2 {
		t.Errorf("stored = %v after one relist, want at most 2 (%v)", got[resultStored], got)
	}
}

// TestRunReturnsOnContextCancel: the reader is spawned through cmd/console's
// `spawn` helper, whose wg.Wait blocks shutdown until Run returns.
func TestRunReturnsOnContextCancel(t *testing.T) {
	r, _ := newReader(t, fake.NewClientset(), &fakeTopology{}, &fakeSink{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s of context cancellation")
	}
}

// TestRunSurvivesAListFailure: a controller-plane blip must produce a backoff
// and a retry, never a dead goroutine that stops capturing until the next
// restart.
func TestRunSurvivesAListFailure(t *testing.T) {
	cs := fake.NewClientset(podEvent("p1", testNamespace, testNamespace, "agent-x", "10"))
	var mu sync.Mutex
	failures := 0
	cs.PrependReactor("list", "events", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() != testNamespace {
			return false, nil, nil
		}
		mu.Lock()
		defer mu.Unlock()
		if failures < 2 {
			failures++
			return true, nil, errors.New("apiserver unavailable")
		}
		return false, nil, nil
	})

	r, m := newReader(t, cs, &fakeTopology{}, &fakeSink{})
	got := runUntil(t, r, m, 1)
	if got[resultStored] != 1 {
		t.Errorf("stored = %v after two failed lists, want 1 (%v)", got[resultStored], got)
	}
}

// TestBackoffIsCapped pins the schedule shape: exponential from min, never past
// max, and reset once a stream is healthy again.
func TestBackoffIsCapped(t *testing.T) {
	r, _ := newReader(t, fake.NewClientset(), nil, &fakeSink{})
	r.minBackoff, r.maxBackoff = time.Second, 30*time.Second

	d := r.minBackoff
	seen := make([]time.Duration, 0, 11)
	seen = append(seen, d)
	for range 10 {
		d = r.nextBackoff(d)
		seen = append(seen, d)
	}
	if seen[1] != 2*time.Second || seen[2] != 4*time.Second {
		t.Errorf("backoff schedule starts %v, want 1s 2s 4s", seen[:3])
	}
	for _, d := range seen {
		if d > r.maxBackoff {
			t.Fatalf("backoff %v exceeded the %v cap (schedule %v)", d, r.maxBackoff, seen)
		}
	}
	if seen[len(seen)-1] != r.maxBackoff {
		t.Errorf("backoff settled at %v, want the %v cap", seen[len(seen)-1], r.maxBackoff)
	}
}

// TestLogLimiterAllowsOncePerReasonPerWindow: an event storm must not become a
// log storm, and one noisy reason must not silence a different one.
func TestLogLimiterAllowsOncePerReasonPerWindow(t *testing.T) {
	clock := time.Now()
	l := newLogLimiter(func() time.Time { return clock })

	if !l.allow("Killing") {
		t.Fatal("the first occurrence of a reason must be logged")
	}
	if l.allow("Killing") {
		t.Error("the second occurrence inside the window must be suppressed")
	}
	if !l.allow("BackOff") {
		t.Error("a different reason must not be suppressed by another one's window")
	}
	clock = clock.Add(logRateLimit + time.Second)
	if !l.allow("Killing") {
		t.Error("the window must reopen after logRateLimit")
	}
}

// TestLogLimiterIsBounded: reason is a cluster-controlled string, so the
// limiter's own map is an unbounded-growth surface unless it is capped.
func TestLogLimiterIsBounded(t *testing.T) {
	clock := time.Now()
	l := newLogLimiter(func() time.Time { return clock })
	for i := range logReasonsMax * 3 {
		l.allow(string(rune(i)) + "-reason")
	}
	if got := l.size(); got > logReasonsMax {
		t.Errorf("limiter holds %d reasons, want at most %d", got, logReasonsMax)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
