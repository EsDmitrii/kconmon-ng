// Package kubectx captures Kubernetes events into the Console's k8s_events
// table so the Investigate timeline can put "the kubelet evicted this pod" and
// "this node went NotReady" next to the loss and latency the fleet measured.
//
// It is the ONLY part of the Console that talks to the Kubernetes apiserver,
// and it is deliberately small: two list+watch streams, one filter, one
// idempotent INSERT. It owns no cache anything else reads, exposes no HTTP
// surface, and answers no question at read time -- httpapi reads the table.
//
// What it captures is bounded by M6 Decision 3, and the bound is the point. An
// unfiltered cluster event stream is a privacy problem (every message any
// controller writes about any object, in a database an operator can page
// through) and a volume problem (a busy cluster emits events far faster than a
// network monitor produces measurements). So:
//
//   - Pod events come from ONE namespace, the release namespace, where the
//     agents and the controller run.
//   - Node events are kept only for nodes the controller's topology vouches
//     for, and the check FAILS CLOSED: no topology, no node events.
//
// The reader must never be a reason the Console fails to start. Everything it
// can fail at -- an unavailable apiserver, an unreadable topology, a rejected
// row -- is a counted, logged, retried condition, never a fatal one.
package kubectx

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"

	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// K8sEvents{result} label values. Closed set, pinned in metrics_test.go.
const (
	resultStored    = "stored"
	resultDuplicate = "duplicate"
	resultFiltered  = "filtered"
	resultError     = "error"
)

const (
	// messageMaxLen caps a stored event message at 1 KiB, INCLUDING the
	// truncation marker. An event message is operator-visible free text
	// produced by anything with write access to events; the store's own
	// 8 KiB bound is the outer wall, and this is the one that keeps a
	// timeline row readable and a table row small.
	messageMaxLen = 1024
	// truncationMarker makes a cut message read as cut. Without it an
	// operator has no way to tell a truncated message from one that
	// genuinely ended there, which is exactly the kind of quiet lie an
	// investigation surface must not tell.
	truncationMarker = "..."

	// topologyRefreshInterval bounds how often the node-name set is re-read
	// from the controller. A read per event would turn an event storm into a
	// controller DoS, so the set is cached and a snapshot up to this old is
	// ACCEPTED: the cost of that staleness is bounded and self-correcting in
	// both directions -- a node that just joined has its events filtered for
	// up to one window, and a node that just left has them kept for up to
	// one. Neither is a correctness problem for a capture whose whole job is
	// to be a best-effort mirror of what the cluster said.
	topologyRefreshInterval = 30 * time.Second

	// Watch backoff. The floor exists so a stream that keeps closing cannot
	// become a hot relist loop against the apiserver; the ceiling exists so a
	// long apiserver outage does not turn into an arbitrarily long silence
	// after it ends.
	defaultMinBackoff = 1 * time.Second
	defaultMaxBackoff = 30 * time.Second

	// defaultResyncInterval backs up config validation. The reader is only
	// ever spawned with a validated config, so this is reached only by a
	// caller that built the struct by hand -- a zero here would be a relist
	// storm, so it is repaired rather than trusted.
	defaultResyncInterval = 10 * time.Minute

	// logRateLimit and logReasonsMax bound the reader's own logging. An event
	// storm must not become a log storm, and because the key is the k8s event
	// Reason -- a cluster-controlled string -- the keyspace itself is capped.
	logRateLimit  = time.Minute
	logReasonsMax = 256
)

// TopologySource is the live node snapshot a node event is checked against.
// Same narrow local interface scheduler, checks and httpapi each declare, for
// the same reason: a test pins a fixed topology with no controller and no HTTP
// server.
//
// A nil TopologySource is legal and means "no topology": every node event is
// then filtered (Decision 3's fail-closed rule), which is what the Console does
// when controller.url is unset.
type TopologySource interface {
	Topology(ctx context.Context) (*controllerclient.Topology, error)
}

var _ TopologySource = (*controllerclient.Client)(nil)

// stream is one list+watch. There are exactly TWO, and the split is forced by
// Kubernetes, not chosen:
//
//   - A pod's events live in the pod's namespace, so capturing them is a
//     namespace-scoped watch on the release namespace -- and keeping it
//     namespace-scoped is what stops the capture from becoming a cluster-wide
//     firehose.
//   - A Node is a cluster-scoped object, but the Event describing it is still
//     a namespaced object (the kubelet writes it into "default"). There is no
//     namespace that means "node events", so they can only be reached by an
//     all-namespaces watch narrowed with a field selector.
//
// One watch cannot be both: a single all-namespaces watch would drag in every
// pod event in the cluster, which is the exact thing the release-namespace
// bound exists to prevent.
//
// kind is checked client-side as well as being pushed to the apiserver as a
// field selector, and that redundancy is deliberate: the selector is an
// optimisation the server may or may not honour, while the kind check is the
// contract with the store, whose k8sEventKinds set rejects anything else.
type stream struct {
	name          string
	namespace     string
	fieldSelector string
	kind          string
}

// Reader is the capture loop. One per process; Run owns it.
type Reader struct {
	client kubernetes.Interface
	topo   TopologySource
	sink   store.K8sEventStore
	m      *metrics.Metrics

	// namespace is the RESOLVED pod-event namespace (config, then
	// POD_NAMESPACE, then "default"), computed once in New: resolving it per
	// event would read the environment in a hot path for a value that cannot
	// change while the process runs.
	namespace string
	resync    time.Duration

	// minBackoff/maxBackoff are fields rather than constants only so a unit
	// test can run the same schedule a thousand times faster; nothing outside
	// this package can change them.
	minBackoff time.Duration
	maxBackoff time.Duration

	// now is time.Now indirected for the tests that assert the refresh window
	// and the log rate limit, neither of which can be asserted against a real
	// clock without sleeping for the window.
	now func() time.Time

	// nodes is the cached topology node-name set and nodesAt is when it was
	// last ATTEMPTED (success or failure alike, so a controller outage cannot
	// turn one failed read into a per-event retry). A nil nodes with a
	// non-zero nodesAt is the fail-closed state: the topology could not be
	// read, so nothing vouches for any node.
	//
	// The mutex is held across the controller call. That serialises the two
	// stream goroutines, which costs nothing in practice because only the node
	// stream ever asks -- and the alternative, releasing the lock around the
	// read, would let an event storm start N concurrent topology reads, which
	// is the DoS the cache exists to prevent.
	mu      sync.Mutex
	nodes   map[string]struct{}
	nodesAt time.Time

	logs *logLimiter
}

// New builds a Reader. It never fails and never touches the network: the
// clientset is already built by the caller, and everything else is a field
// assignment, so a misconfigured reader shows up as counted, logged retries
// rather than as a boot failure.
//
// topo may be nil (no controller configured); sink and m may not.
func New(
	cfg config.KubernetesContextConfig,
	client kubernetes.Interface,
	topo TopologySource,
	sink store.K8sEventStore,
	m *metrics.Metrics,
) *Reader {
	resync := cfg.ResyncInterval
	if resync <= 0 {
		resync = defaultResyncInterval
	}
	now := time.Now
	return &Reader{
		client:     client,
		topo:       topo,
		sink:       sink,
		m:          m,
		namespace:  cfg.ResolveNamespace(),
		resync:     resync,
		minBackoff: defaultMinBackoff,
		maxBackoff: defaultMaxBackoff,
		now:        now,
		logs:       newLogLimiter(now),
	}
}

// Namespace reports the resolved pod-event namespace, so cmd/console can log
// what the reader actually settled on rather than what the config literally
// said (they differ whenever the namespace came from POD_NAMESPACE).
func (r *Reader) Namespace() string { return r.namespace }

// Run drives both streams until ctx is cancelled, then returns once both have
// stopped. It is spawned through cmd/console's `spawn` helper, whose wg.Wait
// blocks shutdown on this return.
func (r *Reader) Run(ctx context.Context) {
	streams := []stream{
		{
			name:          "pods",
			namespace:     r.namespace,
			fieldSelector: "involvedObject.kind=Pod",
			kind:          "Pod",
		},
		{
			name:          "nodes",
			namespace:     metav1.NamespaceAll,
			fieldSelector: "involvedObject.kind=Node",
			kind:          "Node",
		},
	}

	var wg sync.WaitGroup
	for _, s := range streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.watchLoop(ctx, s)
		}()
	}
	wg.Wait()
}

// watchLoop keeps one stream alive for the life of ctx. Every exit from
// listAndWatch is a reason to start over: a clean one (the watch expired, or
// the resync timer fired) waits the floor, a failed one backs off
// exponentially to the cap. Nothing here can end the loop except ctx.
func (r *Reader) watchLoop(ctx context.Context, s stream) {
	backoff := r.minBackoff
	for ctx.Err() == nil {
		err := r.listAndWatch(ctx, s)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if r.logs.allow("stream:" + s.name) {
				slog.Warn("kubernetes event stream failed, retrying",
					"stream", s.name, "backoff", backoff, "error", err)
			}
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = r.nextBackoff(backoff)
			continue
		}
		// Clean end of a watch. Still pause for the floor: an apiserver that
		// hangs up immediately must not become a relist loop against it.
		if !sleepCtx(ctx, r.minBackoff) {
			return
		}
		backoff = r.minBackoff
	}
}

// nextBackoff doubles d, capped at maxBackoff.
func (r *Reader) nextBackoff(d time.Duration) time.Duration {
	next := d * 2
	if next > r.maxBackoff || next <= 0 {
		return r.maxBackoff
	}
	return next
}

// listAndWatch performs one LIST, then WATCHes from the list's resourceVersion
// until the watch ends, ctx is cancelled, or the resync interval elapses. A nil
// return means "ordinary end, go around again"; a non-nil one means "back off
// first".
//
// The relist is not just recovery: it is how the capture stays correct across
// gaps. Every relisted event the database already holds costs one conflicting
// INSERT and one duplicate counter increment, never a duplicate row -- the
// (uid, resourceVersion) key is what makes replaying the whole list cheap
// enough to do routinely.
func (r *Reader) listAndWatch(ctx context.Context, s stream) error {
	events := r.client.CoreV1().Events(s.namespace)

	list, err := events.List(ctx, metav1.ListOptions{FieldSelector: s.fieldSelector})
	if err != nil {
		return fmt.Errorf("list events (%s): %w", s.name, err)
	}
	for i := range list.Items {
		r.handle(ctx, &list.Items[i], s)
	}

	w, err := events.Watch(ctx, metav1.ListOptions{
		FieldSelector:   s.fieldSelector,
		ResourceVersion: list.ResourceVersion,
	})
	if err != nil {
		return fmt.Errorf("watch events (%s): %w", s.name, err)
	}
	defer w.Stop()

	// The resync timer is the backstop against a watch that is connected and
	// silent -- wedged in a way no error path can observe, because from here
	// it is indistinguishable from a quiet cluster.
	resync := time.NewTimer(r.resync)
	defer resync.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-resync.C:
			return nil
		case ev, ok := <-w.ResultChan():
			if !ok {
				// The apiserver hung up (commonly "resourceVersion too old").
				// Ordinary, not an error: relist.
				return nil
			}
			switch ev.Type {
			case watch.Added, watch.Modified:
				obj, isEvent := ev.Object.(*corev1.Event)
				if !isEvent {
					continue
				}
				r.handle(ctx, obj, s)
			case watch.Error:
				return fmt.Errorf("watch events (%s): apiserver returned %T", s.name, ev.Object)
			case watch.Deleted, watch.Bookmark:
				// A deleted Event did not un-happen, and the capture is a
				// historical record: deletions are the apiserver's own TTL
				// doing its job, and the row stays until the Console's
				// retention sweep removes it. A bookmark carries no event.
			}
		}
	}
}

// handle applies the filter to one event and, when it survives, writes it.
//
// An event of the wrong kind for this stream is skipped SILENTLY and counted
// nowhere. It is not a filter decision: it means the apiserver ignored the
// field selector and the other stream owns this event, so counting it would
// double-count one event across two streams and make `filtered` unreadable.
func (r *Reader) handle(ctx context.Context, ev *corev1.Event, s stream) {
	if ev.InvolvedObject.Kind != s.kind {
		return
	}
	if !r.keep(ctx, ev, s) {
		r.m.K8sEvents.WithLabelValues(resultFiltered).Inc()
		return
	}

	in := store.K8sEventInput{
		UID:             string(ev.UID),
		ResourceVersion: ev.ResourceVersion,
		EventTime:       eventTimeOf(ev),
		Kind:            ev.InvolvedObject.Kind,
		Name:            ev.InvolvedObject.Name,
		Namespace:       involvedNamespace(ev),
		Reason:          ev.Reason,
		Type:            ev.Type,
		Message:         truncateMessage(ev.Message),
		Count:           ev.Count,
	}
	// A Node is cluster-scoped: the Event lives in "default", but the OBJECT
	// the row describes has no namespace, and writing "default" would make the
	// timeline claim otherwise.
	if in.Kind == "Node" {
		in.Namespace = ""
	}

	inserted, err := r.sink.InsertK8sEvent(ctx, in)
	switch {
	case err != nil:
		r.m.K8sEvents.WithLabelValues(resultError).Inc()
		// Keyed by reason, not by object: one broken emitter produces one log
		// line a minute instead of one per event. No message is logged -- it
		// is the unbounded operator-visible text this package already truncates
		// before storing.
		if r.logs.allow("insert:" + ev.Reason) {
			slog.Warn("failed to store a kubernetes event", //nolint:gosec // G706: kind/reason are cluster-controlled but bounded identifiers, structured slog fields
				"kind", in.Kind, "reason", in.Reason, "error", err)
		}
	case inserted:
		r.m.K8sEvents.WithLabelValues(resultStored).Inc()
	default:
		// Already stored. The expected steady-state outcome of every relist,
		// which is why it is a metric value and not a log line.
		r.m.K8sEvents.WithLabelValues(resultDuplicate).Inc()
	}
}

// keep is the whole filter (Decision 3).
func (r *Reader) keep(ctx context.Context, ev *corev1.Event, s stream) bool {
	if s.kind == "Node" {
		return r.nodeInTopology(ctx, ev.InvolvedObject.Name)
	}
	// Pod events are kept unconditionally WITHIN the namespace. The check is
	// defensive: the watch is already namespace-scoped, so this only fires if
	// the apiserver hands over something from elsewhere -- in which case the
	// namespace bound, not the watch, is the thing that must hold.
	return involvedNamespace(ev) == r.namespace
}

// nodeInTopology reports whether the controller's topology currently vouches
// for name, refreshing the cached node-name set at most once per
// topologyRefreshInterval.
//
// Every "no" here is a DROPPED node event, including the "no" produced by a
// topology that could not be read at all. That is the fail-closed choice
// Decision 3 makes, and it is the conservative direction: keeping unverified
// node events would let anything in the cluster write rows into an operator's
// investigation surface by naming a node.
func (r *Reader) nodeInTopology(ctx context.Context, name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.nodesAt.IsZero() || r.now().Sub(r.nodesAt) >= topologyRefreshInterval {
		r.refreshNodesLocked(ctx)
	}
	_, ok := r.nodes[name]
	return ok
}

// refreshNodesLocked re-reads the node-name set. Caller must hold r.mu.
func (r *Reader) refreshNodesLocked(ctx context.Context) {
	// nodesAt is stamped FIRST and unconditionally: a failed read must open
	// the same quiet window a successful one does, or a controller outage
	// becomes one topology request per event.
	r.nodesAt = r.now()

	if r.topo == nil {
		r.nodes = nil
		return
	}
	snap, err := r.topo.Topology(ctx)
	if err != nil {
		r.nodes = nil
		if r.logs.allow("topology") {
			slog.Warn("topology unavailable — node events are dropped until it can be read "+
				"(pod events are unaffected)", "error", err)
		}
		return
	}
	set := make(map[string]struct{}, len(snap.Nodes))
	for i := range snap.Nodes {
		set[snap.Nodes[i].Name] = struct{}{}
	}
	r.nodes = set
}

// involvedNamespace returns the namespace of the object the event is ABOUT,
// falling back to the Event's own namespace. They agree for every emitter that
// follows the convention; the fallback covers the ones that leave
// involvedObject.namespace empty.
func involvedNamespace(ev *corev1.Event) string {
	if ev.InvolvedObject.Namespace != "" {
		return ev.InvolvedObject.Namespace
	}
	return ev.Namespace
}

// eventTimeOf picks the timestamp a timeline row should carry.
//
// core/v1 Events carry up to three, and which ones are populated depends on
// which API the emitter used and how old it is: the legacy path fills
// FirstTimestamp/LastTimestamp, the events.k8s.io path fills EventTime (a
// MicroTime), and plenty of emitters fill some subset of the three. The order
// below is by meaning, not by convenience:
//
//  1. LastTimestamp -- "when did this last happen", which is what a row about a
//     recurring event means, and the field that moves when Count increments.
//  2. EventTime -- the modern API's single timestamp, present when the legacy
//     pair is not.
//  3. FirstTimestamp -- the last resort: earlier than the truth for a
//     recurring event, but a real observation rather than a guess.
//
// When all three are empty the zero time is returned deliberately: the store's
// Validate rejects it, so a timestamp-less event is counted as an error instead
// of being silently stamped with time.Now() -- a capture that invents times is
// worse than one that admits it dropped a row.
func eventTimeOf(ev *corev1.Event) time.Time {
	if !ev.LastTimestamp.IsZero() {
		return ev.LastTimestamp.Time
	}
	if !ev.EventTime.IsZero() {
		return ev.EventTime.Time
	}
	if !ev.FirstTimestamp.IsZero() {
		return ev.FirstTimestamp.Time
	}
	return time.Time{}
}

// truncateMessage bounds an event message at messageMaxLen bytes INCLUDING the
// marker, cutting on a rune boundary so a multibyte message never becomes
// invalid UTF-8 in a text column.
func truncateMessage(msg string) string {
	if len(msg) <= messageMaxLen {
		return msg
	}
	cut := messageMaxLen - len(truncationMarker)
	// Walk back to the last rune boundary at or before cut. Ranging over a
	// string yields byte offsets of rune starts, so the last one that still
	// fits is the boundary to cut at.
	end := 0
	for i := range msg {
		if i > cut {
			break
		}
		end = i
	}
	return msg[:end] + truncationMarker
}

// sleepCtx waits d or until ctx ends. It reports false when ctx ended, which
// is every caller's signal to stop.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// logLimiter admits one log line per key per logRateLimit. The keys are
// derived from cluster-controlled strings (a k8s event Reason), so the map is
// capped: an emitter inventing a fresh reason per event would otherwise grow
// this without bound, which is the same cardinality problem the metric labels
// are designed to avoid, moved into the heap.
type logLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
	now  func() time.Time
}

func newLogLimiter(now func() time.Time) *logLimiter {
	return &logLimiter{last: make(map[string]time.Time), now: now}
}

// allow reports whether key may be logged now, and records it when it may.
func (l *logLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if at, ok := l.last[key]; ok && now.Sub(at) < logRateLimit {
		return false
	}
	if len(l.last) >= logReasonsMax {
		// Drop everything rather than evict cleverly: the map is a rate
		// limiter, not a cache, so the worst case of resetting it is one extra
		// log line per key, and the cost of an LRU here would exceed what it
		// protects.
		clear(l.last)
	}
	l.last[key] = now
	return true
}

// size reports how many keys the limiter is tracking (its bound is asserted in
// the tests).
func (l *logLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.last)
}
