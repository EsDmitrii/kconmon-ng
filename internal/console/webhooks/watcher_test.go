package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/EsDmitrii/kconmon-ng/internal/console/events"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// --- doubles ----------------------------------------------------------------

// fakeAlertSource answers a scripted sequence of polls. The LAST entry repeats,
// so a test that only cares about the first two polls does not have to script
// every later one.
type fakeAlertSource struct {
	mu      sync.Mutex
	replies []alertReply
	calls   int
	// polled is announced on every call, so a Run test waits on progress
	// instead of sleeping.
	polled chan struct{}
}

type alertReply struct {
	body json.RawMessage
	err  error
}

func newFakeAlertSource(replies ...alertReply) *fakeAlertSource {
	return &fakeAlertSource{replies: replies, polled: make(chan struct{}, 64)}
}

func (f *fakeAlertSource) Alerts(context.Context) (json.RawMessage, error) {
	f.mu.Lock()
	i := f.calls
	if i >= len(f.replies) {
		i = len(f.replies) - 1
	}
	r := f.replies[i]
	f.calls++
	f.mu.Unlock()
	select {
	case f.polled <- struct{}{}:
	default:
	}
	return r.body, r.err
}

func (f *fakeAlertSource) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeAlertNotifier records what the watcher decided to deliver. It records the
// whole Alert, not just the event, because the payload's honesty about firedAt
// and resolvedAt is half of what these tests are for.
type fakeAlertNotifier struct {
	mu   sync.Mutex
	sent []sentAlert
}

type sentAlert struct {
	event string
	alert Alert
}

func (f *fakeAlertNotifier) NotifyAlert(_ context.Context, event string, a Alert) { //nolint:gocritic // hugeParam: mirrors the seam
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentAlert{event: event, alert: a})
}

func (f *fakeAlertNotifier) recorded() []sentAlert {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentAlert(nil), f.sent...)
}

// fakeRuleSource is the alert_rules read seam: the two facts /api/v1/alerts
// cannot carry, expr and the row's own name.
type fakeRuleSource struct {
	rules []store.AlertRule
	err   error
}

func (f *fakeRuleSource) ListAlertRules(context.Context, bool) ([]store.AlertRule, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rules, nil
}

// --- fixtures ---------------------------------------------------------------

const (
	testRuleID      = "9c5a1d20-0000-4000-8000-0000000000ab"
	testOtherRuleID = "9c5a1d20-0000-4000-8000-0000000000cd"
)

var testActiveAt = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

// promAlert is one entry of the upstream envelope, spelled the way Prometheus
// spells it.
type promAlert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	State       string            `json:"state"`
	ActiveAt    *time.Time        `json:"activeAt,omitempty"`
	Value       string            `json:"value"`
}

func promBody(t *testing.T, alerts ...promAlert) json.RawMessage {
	t.Helper()
	if alerts == nil {
		alerts = []promAlert{}
	}
	raw, err := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"alerts": alerts},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// managedAlert is a firing alert this console owns: it carries the rule-id
// label the renderer stamps.
func managedAlert(ruleID, name string, extra map[string]string) promAlert {
	labels := map[string]string{
		"alertname":          name,
		"severity":           "critical",
		"kconmon_ng_rule_id": ruleID,
	}
	maps.Copy(labels, extra)
	at := testActiveAt
	return promAlert{
		Labels:      labels,
		Annotations: map[string]string{"summary": "loss between racks"},
		State:       "firing",
		ActiveAt:    &at,
		Value:       "7e+00",
	}
}

func newTestWatcher(t *testing.T, src AlertSource, rules RuleSource) (*AlertWatcher, *fakeAlertNotifier) {
	t.Helper()
	n := &fakeAlertNotifier{}
	w, err := NewAlertWatcher(AlertWatcherDeps{
		Alerts: src, Notifier: n, Rules: rules, Interval: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewAlertWatcher: %v", err)
	}
	w.now = func() time.Time { return testNow }
	return w, n
}

var testNow = time.Date(2026, 8, 8, 13, 0, 0, 0, time.UTC)

// --- construction -----------------------------------------------------------

func TestNewAlertWatcherRequiresItsSeams(t *testing.T) {
	src := newFakeAlertSource(alertReply{body: promBody(t)})
	n := &fakeAlertNotifier{}
	for _, tc := range []struct {
		name string
		deps AlertWatcherDeps
	}{
		{"no alert source", AlertWatcherDeps{Notifier: n, Interval: time.Second}},
		{"no notifier", AlertWatcherDeps{Alerts: src, Interval: time.Second}},
	} {
		if _, err := NewAlertWatcher(tc.deps); err == nil {
			t.Errorf("%s: NewAlertWatcher = nil error, want one", tc.name)
		}
	}
	// A rule source is OPTIONAL -- the watcher degrades to the label set.
	if _, err := NewAlertWatcher(AlertWatcherDeps{Alerts: src, Notifier: n, Interval: time.Second}); err != nil {
		t.Errorf("a nil rule source must be allowed, got: %v", err)
	}
}

func TestNewAlertWatcherRepairsANonPositiveInterval(t *testing.T) {
	src := newFakeAlertSource(alertReply{body: promBody(t)})
	w, err := NewAlertWatcher(AlertWatcherDeps{Alerts: src, Notifier: &fakeAlertNotifier{}})
	if err != nil {
		t.Fatal(err)
	}
	if w.interval != DefaultAlertPollInterval {
		t.Errorf("interval = %v, want the default %v", w.interval, DefaultAlertPollInterval)
	}
}

// --- baseline ---------------------------------------------------------------

// The first successful poll after boot is a BASELINE: whatever is firing was
// already firing, and a restart that paged the fleet about it would make every
// rolling update a false alarm.
func TestAlertWatcherBaselineDispatchesNothing(t *testing.T) {
	src := newFakeAlertSource(alertReply{body: promBody(t,
		managedAlert(testRuleID, "PairLossHigh", nil),
		managedAlert(testOtherRuleID, "DNSFailures", nil),
	)})
	w, n := newTestWatcher(t, src, nil)

	w.poll(context.Background())

	if got := n.recorded(); len(got) != 0 {
		t.Errorf("baseline poll dispatched %d notifications, want 0: %+v", len(got), got)
	}
	if !w.baselined {
		t.Error("the watcher did not take a baseline on its first successful poll")
	}
}

// A baseline is taken on the first SUCCESSFUL poll, not on the first attempt. A
// console that started while Prometheus was down must still not page the fleet
// when it comes back.
func TestAlertWatcherBaselineWaitsForASuccessfulPoll(t *testing.T) {
	body := promBody(t, managedAlert(testRuleID, "PairLossHigh", nil))
	src := newFakeAlertSource(
		alertReply{err: errors.New("connection refused")},
		alertReply{body: body},
	)
	w, n := newTestWatcher(t, src, nil)

	w.poll(context.Background())
	if w.baselined {
		t.Fatal("a FAILED poll took a baseline")
	}
	w.poll(context.Background())

	if got := n.recorded(); len(got) != 0 {
		t.Errorf("the first successful poll dispatched %d notifications, want 0: %+v", len(got), got)
	}
	if !w.baselined {
		t.Error("the first successful poll did not take a baseline")
	}
}

// --- edges ------------------------------------------------------------------

func TestAlertWatcherFiresOnANewFingerprint(t *testing.T) {
	src := newFakeAlertSource(
		alertReply{body: promBody(t)},
		alertReply{body: promBody(t, managedAlert(testRuleID, "PairLossHigh", nil))},
	)
	w, n := newTestWatcher(t, src, nil)

	w.poll(context.Background()) // baseline: nothing firing
	w.poll(context.Background())

	got := n.recorded()
	if len(got) != 1 {
		t.Fatalf("got %d notifications, want 1: %+v", len(got), got)
	}
	if got[0].event != store.WebhookEventAlertFired {
		t.Errorf("event = %q, want %q", got[0].event, store.WebhookEventAlertFired)
	}
	a := got[0].alert
	if a.RuleID != testRuleID {
		t.Errorf("ruleId = %q, want %q", a.RuleID, testRuleID)
	}
	if a.RuleName != "PairLossHigh" {
		t.Errorf("ruleName = %q, want the alertname label with no rule source wired", a.RuleName)
	}
	if a.Severity != "critical" {
		t.Errorf("severity = %q, want critical", a.Severity)
	}
	// firedAt is PROMETHEUS' activeAt, not the console's clock: it is the one
	// timestamp that is identical on every replica, which is what makes it a
	// usable deduplication component.
	if !a.FiredAt.Equal(testActiveAt) {
		t.Errorf("firedAt = %v, want Prometheus' activeAt %v", a.FiredAt, testActiveAt)
	}
	if a.ResolvedAt != nil {
		t.Errorf("resolvedAt = %v on a fired edge, want nil", a.ResolvedAt)
	}
	if a.Labels["kconmon_ng_rule_id"] != testRuleID || a.Annotations["summary"] != "loss between racks" {
		t.Errorf("labels/annotations were not carried verbatim: %+v / %+v", a.Labels, a.Annotations)
	}
}

func TestAlertWatcherResolvesOnADisappearedFingerprint(t *testing.T) {
	src := newFakeAlertSource(
		alertReply{body: promBody(t, managedAlert(testRuleID, "PairLossHigh", nil))},
		alertReply{body: promBody(t)},
	)
	w, n := newTestWatcher(t, src, nil)

	w.poll(context.Background()) // baseline: it was already firing
	w.poll(context.Background())

	got := n.recorded()
	if len(got) != 1 {
		t.Fatalf("got %d notifications, want 1: %+v", len(got), got)
	}
	if got[0].event != store.WebhookEventAlertResolved {
		t.Errorf("event = %q, want %q", got[0].event, store.WebhookEventAlertResolved)
	}
	a := got[0].alert
	// firedAt is the REMEMBERED one: a resolution is about the alert that was
	// firing, and re-deriving it from an absent alert is impossible anyway.
	if !a.FiredAt.Equal(testActiveAt) {
		t.Errorf("firedAt = %v, want the remembered %v", a.FiredAt, testActiveAt)
	}
	// resolvedAt is WHEN THE POLL NOTICED. That IS the granularity -- the
	// absence has no timestamp of its own -- and the payload doc says so.
	if a.ResolvedAt == nil || !a.ResolvedAt.Equal(testNow) {
		t.Errorf("resolvedAt = %v, want the observing poll's clock %v", a.ResolvedAt, testNow)
	}
}

// One rule can fire per SERIES, so the fingerprint is the rule id AND the
// alert's label set. Two label sets under one rule are two alerts, and
// resolving one must not resolve the other.
func TestAlertWatcherFingerprintsPerSeries(t *testing.T) {
	zoneA := managedAlert(testRuleID, "PairLossHigh", map[string]string{"zone": "a"})
	zoneB := managedAlert(testRuleID, "PairLossHigh", map[string]string{"zone": "b"})
	src := newFakeAlertSource(
		alertReply{body: promBody(t)},
		alertReply{body: promBody(t, zoneA, zoneB)},
		alertReply{body: promBody(t, zoneA)},
	)
	w, n := newTestWatcher(t, src, nil)

	w.poll(context.Background()) // baseline
	w.poll(context.Background()) // both series appear
	firstRound := n.recorded()
	if len(firstRound) != 2 {
		t.Fatalf("two series of one rule produced %d notifications, want 2: %+v", len(firstRound), firstRound)
	}
	zones := map[string]bool{}
	for _, s := range firstRound {
		if s.event != store.WebhookEventAlertFired {
			t.Errorf("event = %q, want alert.fired", s.event)
		}
		zones[s.alert.Labels["zone"]] = true
	}
	if !zones["a"] || !zones["b"] {
		t.Errorf("the two fired alerts were not the two zones: %+v", firstRound)
	}

	w.poll(context.Background()) // zone b clears
	got := n.recorded()
	if len(got) != 3 {
		t.Fatalf("got %d notifications after zone b cleared, want 3: %+v", len(got), got)
	}
	last := got[2]
	if last.event != store.WebhookEventAlertResolved || last.alert.Labels["zone"] != "b" {
		t.Errorf("last notification = %s %v, want alert.resolved for zone b", last.event, last.alert.Labels)
	}
}

// A repeated poll with the same firing set is not an edge. Nothing.
func TestAlertWatcherIsSilentWhileTheSetIsUnchanged(t *testing.T) {
	body := promBody(t, managedAlert(testRuleID, "PairLossHigh", nil))
	src := newFakeAlertSource(alertReply{body: body})
	w, n := newTestWatcher(t, src, nil)

	for range 4 {
		w.poll(context.Background())
	}
	if got := n.recorded(); len(got) != 0 {
		t.Errorf("an unchanged firing set produced %d notifications, want 0: %+v", len(got), got)
	}
}

// --- freeze on failure ------------------------------------------------------

// A dead Prometheus must NOT "resolve" the fleet.
func TestAlertWatcherPollErrorFreezesTheFiringSet(t *testing.T) {
	body := promBody(t,
		managedAlert(testRuleID, "PairLossHigh", nil),
		managedAlert(testOtherRuleID, "DNSFailures", nil),
	)
	src := newFakeAlertSource(
		alertReply{body: body},
		alertReply{err: errors.New("connection refused")},
		alertReply{body: body},
	)
	w, n := newTestWatcher(t, src, nil)

	w.poll(context.Background()) // baseline: two firing
	w.poll(context.Background()) // prometheus is down
	if got := n.recorded(); len(got) != 0 {
		t.Fatalf("a failed poll dispatched %d notifications, want 0 -- a dead prometheus must not "+
			"resolve the fleet: %+v", len(got), got)
	}
	w.poll(context.Background()) // prometheus is back, same alerts
	if got := n.recorded(); len(got) != 0 {
		t.Errorf("the recovery poll dispatched %d notifications, want 0: %+v", len(got), got)
	}
}

// A body the console cannot read is the same class of failure as no body at
// all: freeze, do not resolve. Prometheus answering status:"error" included.
func TestAlertWatcherUndecodableBodyFreezesTheFiringSet(t *testing.T) {
	firing := promBody(t, managedAlert(testRuleID, "PairLossHigh", nil))
	src := newFakeAlertSource(
		alertReply{body: firing},
		alertReply{body: json.RawMessage(`{"status":"error","errorType":"internal"}`)},
		alertReply{body: json.RawMessage(`not json at all`)},
		alertReply{body: firing},
	)
	w, n := newTestWatcher(t, src, nil)

	for range 4 {
		w.poll(context.Background())
	}
	if got := n.recorded(); len(got) != 0 {
		t.Errorf("undecodable polls dispatched %d notifications, want 0: %+v", len(got), got)
	}
}

// --- what is and is not webhook material ------------------------------------

// A firing alert with no kconmon_ng_rule_id label belongs to whoever wrote that
// rule. This console did not create it, does not render it, cannot resolve its
// expression, and has no business paging an operator's endpoints about it.
func TestAlertWatcherIgnoresUnmanagedAlerts(t *testing.T) {
	foreign := promAlert{
		Labels:      map[string]string{"alertname": "SomeoneElsesAlert", "severity": "critical"},
		Annotations: map[string]string{},
		State:       "firing",
		ActiveAt:    &testActiveAt,
	}
	src := newFakeAlertSource(
		alertReply{body: promBody(t)},
		alertReply{body: promBody(t, foreign)},
		alertReply{body: promBody(t)},
	)
	w, n := newTestWatcher(t, src, nil)

	for range 3 {
		w.poll(context.Background())
	}
	if got := n.recorded(); len(got) != 0 {
		t.Errorf("an unmanaged alert produced %d notifications, want 0: %+v", len(got), got)
	}
}

// PENDING is not FIRING. An alert inside its `for` window has not fired yet,
// and delivering on it would make every rule's `for` a lie.
func TestAlertWatcherIgnoresPendingAlerts(t *testing.T) {
	pending := managedAlert(testRuleID, "PairLossHigh", nil)
	pending.State = "pending"
	firing := managedAlert(testRuleID, "PairLossHigh", nil)

	src := newFakeAlertSource(
		alertReply{body: promBody(t)},
		alertReply{body: promBody(t, pending)},
		alertReply{body: promBody(t, firing)},
	)
	w, n := newTestWatcher(t, src, nil)

	w.poll(context.Background()) // baseline
	w.poll(context.Background()) // pending -- not an edge
	if got := n.recorded(); len(got) != 0 {
		t.Fatalf("a pending alert produced %d notifications, want 0: %+v", len(got), got)
	}
	w.poll(context.Background()) // it fires for real
	got := n.recorded()
	if len(got) != 1 || got[0].event != store.WebhookEventAlertFired {
		t.Errorf("pending->firing produced %+v, want exactly one alert.fired", got)
	}
}

// An alert with no activeAt still has to have a firedAt: the key is not
// optional. The console's own clock is the honest fallback, and it says so.
func TestAlertWatcherFallsBackToTheObservingClockWithoutActiveAt(t *testing.T) {
	noActive := managedAlert(testRuleID, "PairLossHigh", nil)
	noActive.ActiveAt = nil
	src := newFakeAlertSource(
		alertReply{body: promBody(t)},
		alertReply{body: promBody(t, noActive)},
	)
	w, n := newTestWatcher(t, src, nil)

	w.poll(context.Background())
	w.poll(context.Background())

	got := n.recorded()
	if len(got) != 1 {
		t.Fatalf("got %d notifications, want 1", len(got))
	}
	if !got[0].alert.FiredAt.Equal(testNow) {
		t.Errorf("firedAt = %v, want the observing poll's clock %v", got[0].alert.FiredAt, testNow)
	}
}

// --- rule enrichment --------------------------------------------------------

// expr and the row's own name are the two facts /api/v1/alerts cannot carry, so
// they come from the alert_rules table the rule was rendered from.
func TestAlertWatcherEnrichesFromTheRuleSource(t *testing.T) {
	rules := &fakeRuleSource{rules: []store.AlertRule{{
		ID:           testRuleID,
		Name:         "pair-loss-high",
		RenderedExpr: `kconmon_ng_pair_loss_ratio > 0.2`,
	}}}
	src := newFakeAlertSource(
		alertReply{body: promBody(t)},
		alertReply{body: promBody(t,
			managedAlert(testRuleID, "PairLossHigh", nil),
			managedAlert(testOtherRuleID, "DNSFailures", nil),
		)},
	)
	w, n := newTestWatcher(t, src, rules)

	w.poll(context.Background())
	w.poll(context.Background())

	byRule := map[string]Alert{}
	for _, s := range n.recorded() {
		byRule[s.alert.RuleID] = s.alert
	}
	if len(byRule) != 2 {
		t.Fatalf("got %d distinct alerts, want 2: %+v", len(byRule), byRule)
	}
	known := byRule[testRuleID]
	if known.RuleName != "pair-loss-high" {
		t.Errorf("ruleName = %q, want the row's own name", known.RuleName)
	}
	if known.Expr != `kconmon_ng_pair_loss_ratio > 0.2` {
		t.Errorf("expr = %q, want the row's rendered expression", known.Expr)
	}
	// The rule the source does not know about (deleted between the poll and the
	// lookup, say) degrades to the label set and an EMPTY expr -- never a guess.
	unknown := byRule[testOtherRuleID]
	if unknown.RuleName != "DNSFailures" {
		t.Errorf("ruleName = %q, want the alertname label as the fallback", unknown.RuleName)
	}
	if unknown.Expr != "" {
		t.Errorf("expr = %q, want \"\" for a rule the source does not know", unknown.Expr)
	}
}

// A rule source that fails must not stop the delivery: the transition is the
// news, and expr is enrichment. Degrade, do not drop.
func TestAlertWatcherRuleSourceFailureStillDelivers(t *testing.T) {
	rules := &fakeRuleSource{err: errors.New("connection refused")}
	src := newFakeAlertSource(
		alertReply{body: promBody(t)},
		alertReply{body: promBody(t, managedAlert(testRuleID, "PairLossHigh", nil))},
	)
	w, n := newTestWatcher(t, src, rules)

	w.poll(context.Background())
	w.poll(context.Background())

	got := n.recorded()
	if len(got) != 1 {
		t.Fatalf("got %d notifications, want 1 -- a failed enrichment must not swallow the edge", len(got))
	}
	if got[0].alert.Expr != "" || got[0].alert.RuleName != "PairLossHigh" {
		t.Errorf("degraded alert = %+v, want the label-set fallback", got[0].alert)
	}
}

// --- the loop ---------------------------------------------------------------

// The interval is asserted through the injected sleeper, the dispatcher's
// idiom: a loop asserted against a real clock is a 30-second test.
func TestAlertWatcherRunPollsOnItsInterval(t *testing.T) {
	src := newFakeAlertSource(alertReply{body: promBody(t)})
	w, _ := newTestWatcher(t, src, nil)

	var (
		mu    sync.Mutex
		waits []time.Duration
	)
	stop := make(chan struct{})
	w.sleep = func(ctx context.Context, d time.Duration) bool {
		mu.Lock()
		waits = append(waits, d)
		n := len(waits)
		mu.Unlock()
		if n >= 3 {
			close(stop)
			<-ctx.Done()
			return false
		}
		return ctx.Err() == nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	select {
	case <-stop:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher did not reach three intervals")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	for i, d := range waits {
		if d != 30*time.Second {
			t.Errorf("wait %d = %v, want the configured 30s", i, d)
		}
	}
	// It polls FIRST and waits after, so a console that just started takes its
	// baseline now rather than one interval from now.
	if n := src.count(); n < len(waits) {
		t.Errorf("polls=%d waits=%d, want a poll ahead of every wait", n, len(waits))
	}
}

func TestAlertWatcherRunReturnsOnContextCancellation(t *testing.T) {
	src := newFakeAlertSource(alertReply{body: promBody(t)})
	w, _ := newTestWatcher(t, src, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	select {
	case <-src.polled:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher never polled")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// --- fingerprints -----------------------------------------------------------

// The fingerprint has to be stable across polls (or every poll is an edge) and
// distinct per label set (or two series of one rule collapse into one alert).
func TestFingerprintIsStableAndPerLabelSet(t *testing.T) {
	base := map[string]string{"alertname": "A", "zone": "a", "pair": "n1->n2"}
	same := map[string]string{"pair": "n1->n2", "zone": "a", "alertname": "A"}
	other := map[string]string{"alertname": "A", "zone": "b", "pair": "n1->n2"}

	if fingerprint(testRuleID, base) != fingerprint(testRuleID, same) {
		t.Error("the same label set in a different map order produced two fingerprints")
	}
	if fingerprint(testRuleID, base) == fingerprint(testRuleID, other) {
		t.Error("two label sets produced one fingerprint")
	}
	if fingerprint(testRuleID, base) == fingerprint(testOtherRuleID, base) {
		t.Error("two rules with the same label set produced one fingerprint")
	}
	// Values that could collide if the pairs were concatenated naively.
	if fingerprint(testRuleID, map[string]string{"a": "b=c"}) ==
		fingerprint(testRuleID, map[string]string{"a=b": "c"}) {
		t.Error("the fingerprint is ambiguous across the key/value boundary")
	}
}

type fakeMaintenance struct {
	windows []store.MaintenanceWindow
	err     error
	// pages, when set, replaces windows: page i is answered for cursor "page-i", the first for "".
	pages [][]store.MaintenanceWindow
	calls int
}

func (f *fakeMaintenance) ListMaintenanceWindows(_ context.Context, mf store.MaintenanceFilter) (store.MaintenancePage, error) {
	f.calls++
	if f.err != nil {
		return store.MaintenancePage{}, f.err
	}
	if f.pages == nil {
		return store.MaintenancePage{Windows: f.windows}, nil
	}
	i := 0
	if mf.Cursor != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(mf.Cursor, "page-"))
		if err != nil {
			return store.MaintenancePage{}, err
		}
		i = n
	}
	page := store.MaintenancePage{Windows: f.pages[i]}
	if i+1 < len(f.pages) {
		page.NextCursor = "page-" + strconv.Itoa(i+1)
	}
	return page, nil
}

func windowAround(scope string) store.MaintenanceWindow {
	return store.MaintenanceWindow{ID: "w-" + scope, Scope: scope,
		StartAt: testNow.Add(-time.Hour), EndAt: testNow.Add(time.Hour)}
}

func newMaintenanceWatcher(t *testing.T, src AlertSource, mt MaintenanceSource) (*AlertWatcher, *fakeAlertNotifier) {
	t.Helper()
	n := &fakeAlertNotifier{}
	w, err := NewAlertWatcher(AlertWatcherDeps{Alerts: src, Notifier: n, Maintenance: mt, Interval: 30 * time.Second})
	if err != nil {
		t.Fatalf("NewAlertWatcher: %v", err)
	}
	w.now = func() time.Time { return testNow }
	return w, n
}

func pairAlert() promAlert {
	return managedAlert(testRuleID, "PairLossHigh", map[string]string{"source_node": "n1", "destination_node": "n2"})
}

// Inside a window neither edge is delivered: the fired one is held, the resolved one has nothing to
// resolve.
func TestAlertWatcherHoldsBothEdgesInsideAWindow(t *testing.T) {
	src := newFakeAlertSource(
		alertReply{body: promBody(t)},
		alertReply{body: promBody(t, pairAlert())},
		alertReply{body: promBody(t)},
	)
	w, n := newMaintenanceWatcher(t, src, &fakeMaintenance{windows: []store.MaintenanceWindow{windowAround("")}})
	for range 3 {
		w.poll(context.Background())
	}
	if got := n.recorded(); len(got) != 0 {
		t.Fatalf("delivered %d notifications inside a global window, want 0: %+v", len(got), got)
	}
}

// A window that closes on an alert still firing delivers its fired edge then, once, with the
// original firedAt so replicas still dedupe on it.
func TestAlertWatcherDeliversWhenTheWindowClosesOnAFiringAlert(t *testing.T) {
	src := newFakeAlertSource(
		alertReply{body: promBody(t)},
		alertReply{body: promBody(t, pairAlert())},
	)
	w, n := newMaintenanceWatcher(t, src, &fakeMaintenance{windows: []store.MaintenanceWindow{windowAround("n1")}})
	w.poll(context.Background())
	w.poll(context.Background())
	if got := n.recorded(); len(got) != 0 {
		t.Fatalf("delivered inside the window: %+v", got)
	}

	w.now = func() time.Time { return testNow.Add(2 * time.Hour) }
	w.poll(context.Background())
	w.poll(context.Background())
	got := n.recorded()
	if len(got) != 1 || got[0].event != store.WebhookEventAlertFired {
		t.Fatalf("after the window: %+v, want exactly one alert.fired", got)
	}
	if !got[0].alert.FiredAt.Equal(testActiveAt) {
		t.Errorf("firedAt = %v, want Prometheus' activeAt %v", got[0].alert.FiredAt, testActiveAt)
	}
}

func TestAlertWatcherMatchesWindowScopes(t *testing.T) {
	tests := []struct {
		scope string
		held  bool
	}{
		{"", true},
		{"n1", true},
		{"n2", true},
		{"n1" + events.PairArrow + "n2", true},
		{"n2" + events.PairArrow + "n1", false},
		{"n3", false},
	}
	for _, tc := range tests {
		t.Run(tc.scope, func(t *testing.T) {
			src := newFakeAlertSource(alertReply{body: promBody(t)}, alertReply{body: promBody(t, pairAlert())})
			w, n := newMaintenanceWatcher(t, src, &fakeMaintenance{windows: []store.MaintenanceWindow{windowAround(tc.scope)}})
			w.poll(context.Background())
			w.poll(context.Background())
			if held := len(n.recorded()) == 0; held != tc.held {
				t.Fatalf("scope %q: held=%v, want %v", tc.scope, held, tc.held)
			}
		})
	}
}

func TestAlertWatcherHoldsAnExternalTargetAlert(t *testing.T) {
	alert := managedAlert(testRuleID, "ExternalDown", map[string]string{"target": "dns-edge"})
	src := newFakeAlertSource(alertReply{body: promBody(t)}, alertReply{body: promBody(t, alert)})
	w, n := newMaintenanceWatcher(t, src, &fakeMaintenance{windows: []store.MaintenanceWindow{windowAround("dns-edge")}})
	w.poll(context.Background())
	w.poll(context.Background())
	if got := n.recorded(); len(got) != 0 {
		t.Fatalf("an alert on the window's target was delivered: %+v", got)
	}
}

// Windows that have not started or have already ended hold nothing, whatever the store returned.
func TestAlertWatcherIgnoresWindowsOutsideNow(t *testing.T) {
	future := store.MaintenanceWindow{Scope: "", StartAt: testNow.Add(time.Minute), EndAt: testNow.Add(time.Hour)}
	past := store.MaintenanceWindow{Scope: "", StartAt: testNow.Add(-time.Hour), EndAt: testNow}
	src := newFakeAlertSource(alertReply{body: promBody(t)}, alertReply{body: promBody(t, pairAlert())})
	w, n := newMaintenanceWatcher(t, src, &fakeMaintenance{windows: []store.MaintenanceWindow{future, past}})
	w.poll(context.Background())
	w.poll(context.Background())
	if got := n.recorded(); len(got) != 1 {
		t.Fatalf("got %d notifications, want the one fired edge", len(got))
	}
}

// An unreadable maintenance table must not swallow pages: fail open.
func TestAlertWatcherFailsOpenWhenWindowsCannotBeRead(t *testing.T) {
	src := newFakeAlertSource(alertReply{body: promBody(t)}, alertReply{body: promBody(t, pairAlert())})
	w, n := newMaintenanceWatcher(t, src, &fakeMaintenance{err: errors.New("db down")})
	w.poll(context.Background())
	w.poll(context.Background())
	if got := n.recorded(); len(got) != 1 {
		t.Fatalf("got %d notifications with the store down, want the fired edge delivered", len(got))
	}
}

// newMaintenanceWatcherWith wires rules and metrics too, for the tests that count held edges or need a
// rule's `for`.
func newMaintenanceWatcherWith(t *testing.T, src AlertSource, mt MaintenanceSource, rules RuleSource) (*AlertWatcher, *fakeAlertNotifier, *metrics.Metrics) {
	t.Helper()
	n := &fakeAlertNotifier{}
	m := metrics.New("kconmon_ng", prometheus.NewRegistry())
	w, err := NewAlertWatcher(AlertWatcherDeps{Alerts: src, Notifier: n, Rules: rules, Maintenance: mt,
		Metrics: m, Interval: 30 * time.Second})
	if err != nil {
		t.Fatalf("NewAlertWatcher: %v", err)
	}
	w.now = func() time.Time { return testNow }
	return w, n, m
}

// pairAlertActiveAt is pairAlert with Prometheus' activeAt moved to at.
func pairAlertActiveAt(at time.Time) promAlert {
	a := pairAlert()
	a.ActiveAt = &at
	return a
}

func suppressedCount(m *metrics.Metrics, event string) float64 {
	return testutil.ToFloat64(m.WebhookSuppressed.WithLabelValues(event))
}

func assertOneFired(t *testing.T, got []sentAlert, firedAt time.Time) {
	t.Helper()
	if len(got) != 1 || got[0].event != store.WebhookEventAlertFired {
		t.Fatalf("delivered %+v, want exactly one alert.fired", got)
	}
	if !got[0].alert.FiredAt.Equal(firedAt) {
		t.Errorf("firedAt = %v, want the original activeAt %v", got[0].alert.FiredAt, firedAt)
	}
}

// suppressed lives in memory, so a console restarted inside a window (a drain or an upgrade, which is
// what windows are declared for) must hold again what the previous process held: an alert that began
// inside the window and outlives it still gets its alert.fired when the window closes.
func TestAlertWatcherRestartInsideAWindowStillDeliversWhenItCloses(t *testing.T) {
	activeAt := testNow.Add(-30 * time.Minute) // the window opened an hour before testNow
	firing := promBody(t, pairAlertActiveAt(activeAt))
	src := newFakeAlertSource(
		alertReply{body: firing},
		alertReply{body: firing},
		alertReply{body: firing},
		alertReply{body: promBody(t)},
	)
	w, n, m := newMaintenanceWatcherWith(t, src, &fakeMaintenance{windows: []store.MaintenanceWindow{windowAround("")}}, nil)

	w.poll(context.Background()) // the restarted process takes its baseline inside the window
	w.poll(context.Background())
	if got := n.recorded(); len(got) != 0 {
		t.Fatalf("delivered inside the window: %+v", got)
	}
	if got := suppressedCount(m, store.WebhookEventAlertFired); got != 1 {
		t.Errorf("WebhookSuppressed(alert.fired) = %v, want 1 for the edge the baseline held again", got)
	}

	w.now = func() time.Time { return testNow.Add(2 * time.Hour) }
	w.poll(context.Background()) // the window has closed, the alert still fires
	assertOneFired(t, n.recorded(), activeAt)

	w.poll(context.Background()) // it resolves after the window
	got := n.recorded()
	if len(got) != 2 || got[1].event != store.WebhookEventAlertResolved {
		t.Fatalf("after the resolve: %+v, want alert.fired then alert.resolved", got)
	}
}

// The other half of a restart inside a window: an alert held again at baseline that resolves inside
// the window sends nothing, not a lone alert.resolved.
func TestAlertWatcherRestartInsideAWindowHoldsTheResolveToo(t *testing.T) {
	src := newFakeAlertSource(
		alertReply{body: promBody(t, pairAlertActiveAt(testNow.Add(-30*time.Minute)))},
		alertReply{body: promBody(t)},
	)
	w, n, m := newMaintenanceWatcherWith(t, src, &fakeMaintenance{windows: []store.MaintenanceWindow{windowAround("n1")}}, nil)
	w.poll(context.Background())
	w.poll(context.Background())
	if got := n.recorded(); len(got) != 0 {
		t.Fatalf("delivered %+v, want nothing for an alert that began and ended inside the window", got)
	}
	if got := suppressedCount(m, store.WebhookEventAlertResolved); got != 1 {
		t.Errorf("WebhookSuppressed(alert.resolved) = %v, want 1", got)
	}
}

// An alert that fired before the window opened was delivered then, so a restart inside the window must
// not hold it: its resolve inside the window still goes out.
func TestAlertWatcherRestartInsideAWindowKeepsPreWindowAlertsDelivered(t *testing.T) {
	src := newFakeAlertSource(
		alertReply{body: promBody(t, pairAlertActiveAt(testNow.Add(-3*time.Hour)))},
		alertReply{body: promBody(t)},
	)
	w, n, _ := newMaintenanceWatcherWith(t, src, &fakeMaintenance{windows: []store.MaintenanceWindow{windowAround("")}}, nil)
	w.poll(context.Background())
	w.poll(context.Background())
	got := n.recorded()
	if len(got) != 1 || got[0].event != store.WebhookEventAlertResolved {
		t.Fatalf("delivered %+v, want the alert.resolved of an alert that fired before the window", got)
	}
}

// activeAt is when the alert went PENDING; it fires `for` later. An alert pending when the window opened
// and firing inside it was held by the previous process, so the baseline counts the rule's `for`.
func TestAlertWatcherRestartInsideAWindowCountsTheRulesFor(t *testing.T) {
	activeAt := testNow.Add(-time.Hour - 2*time.Minute) // two minutes before the window opened
	firing := promBody(t, pairAlertActiveAt(activeAt))
	src := newFakeAlertSource(alertReply{body: firing})
	rules := &fakeRuleSource{rules: []store.AlertRule{{ID: testRuleID, Name: "pair-loss-high",
		ForNs: int64(5 * time.Minute)}}}
	w, n, _ := newMaintenanceWatcherWith(t, src, &fakeMaintenance{windows: []store.MaintenanceWindow{windowAround("")}}, rules)
	w.poll(context.Background())
	w.poll(context.Background())
	if got := n.recorded(); len(got) != 0 {
		t.Fatalf("delivered inside the window: %+v", got)
	}
	w.now = func() time.Time { return testNow.Add(2 * time.Hour) }
	w.poll(context.Background())
	assertOneFired(t, n.recorded(), activeAt)
}

// A window declared after the alert fired did not hold it in the previous process either.
func TestAlertWatcherRestartDoesNotHoldAlertsOlderThanTheWindowsCreation(t *testing.T) {
	retro := windowAround("")
	retro.StartAt = testNow.Add(-2 * time.Hour)
	retro.CreatedAt = testNow.Add(-10 * time.Minute)
	src := newFakeAlertSource(
		alertReply{body: promBody(t, pairAlertActiveAt(testNow.Add(-time.Hour)))},
		alertReply{body: promBody(t)},
	)
	w, n, _ := newMaintenanceWatcherWith(t, src, &fakeMaintenance{windows: []store.MaintenanceWindow{retro}}, nil)
	w.poll(context.Background())
	w.poll(context.Background())
	got := n.recorded()
	if len(got) != 1 || got[0].event != store.WebhookEventAlertResolved {
		t.Fatalf("delivered %+v, want the alert.resolved of an alert older than the window's creation", got)
	}
}

// A restart inside a window while the database is still coming back: the baseline cannot read the
// windows, so the hold is decided on the first poll that can.
func TestAlertWatcherRestartDecidesTheHoldOnceWindowsAreReadable(t *testing.T) {
	activeAt := testNow.Add(-30 * time.Minute)
	firing := promBody(t, pairAlertActiveAt(activeAt))
	src := newFakeAlertSource(alertReply{body: firing})
	mt := &fakeMaintenance{windows: []store.MaintenanceWindow{windowAround("")}, err: errors.New("db starting")}
	w, n, _ := newMaintenanceWatcherWith(t, src, mt, nil)
	w.poll(context.Background()) // baseline, windows unreadable
	w.poll(context.Background()) // still unreadable
	mt.err = nil
	w.poll(context.Background()) // readable: the hold is decided now
	if got := n.recorded(); len(got) != 0 {
		t.Fatalf("delivered inside the window: %+v", got)
	}
	w.now = func() time.Time { return testNow.Add(2 * time.Hour) }
	w.poll(context.Background())
	assertOneFired(t, n.recorded(), activeAt)
}

// A read failure while an edge is held must not release it mid-window: the held edge stays held and
// goes out once, when a read shows its window closed.
func TestAlertWatcherKeepsAHeldEdgeHeldWhenWindowsCannotBeRead(t *testing.T) {
	src := newFakeAlertSource(
		alertReply{body: promBody(t)},
		alertReply{body: promBody(t, pairAlert())},
	)
	mt := &fakeMaintenance{windows: []store.MaintenanceWindow{windowAround("")}}
	w, n, m := newMaintenanceWatcherWith(t, src, mt, nil)
	w.poll(context.Background()) // baseline
	w.poll(context.Background()) // fires inside the window: held

	mt.err = errors.New("db down")
	w.poll(context.Background())
	if got := n.recorded(); len(got) != 0 {
		t.Fatalf("a failed window read released the held edge mid-window: %+v", got)
	}
	if got := testutil.ToFloat64(m.WebhookMaintenanceReadErrors.WithLabelValues()); got != 1 {
		t.Errorf("WebhookMaintenanceReadErrors = %v, want 1", got)
	}

	mt.err = nil
	w.poll(context.Background()) // readable again, the window still open
	if got := n.recorded(); len(got) != 0 {
		t.Fatalf("delivered while the window is still open: %+v", got)
	}
	w.now = func() time.Time { return testNow.Add(2 * time.Hour) }
	w.poll(context.Background())
	w.poll(context.Background())
	assertOneFired(t, n.recorded(), testActiveAt)
	if got := suppressedCount(m, store.WebhookEventAlertFired); got != 1 {
		t.Errorf("WebhookSuppressed(alert.fired) = %v, want 1: the release is not a second hold", got)
	}
}

// A new edge during a read failure still fails open, and the failure is counted.
func TestAlertWatcherCountsWindowReadErrors(t *testing.T) {
	src := newFakeAlertSource(alertReply{body: promBody(t)}, alertReply{body: promBody(t, pairAlert())})
	w, n, m := newMaintenanceWatcherWith(t, src, &fakeMaintenance{err: errors.New("db down")}, nil)
	w.poll(context.Background())
	w.poll(context.Background())
	if got := n.recorded(); len(got) != 1 {
		t.Fatalf("got %d notifications with the store down, want the fired edge delivered", len(got))
	}
	if got := testutil.ToFloat64(m.WebhookMaintenanceReadErrors.WithLabelValues()); got != 1 {
		t.Errorf("WebhookMaintenanceReadErrors = %v, want 1", got)
	}
}

// A poll with nothing new and nothing held has nothing a window could decide, so it reads none.
func TestAlertWatcherSkipsTheWindowReadWhenNothingCanBeHeld(t *testing.T) {
	src := newFakeAlertSource(alertReply{body: promBody(t)})
	mt := &fakeMaintenance{windows: []store.MaintenanceWindow{windowAround("")}}
	w, _, _ := newMaintenanceWatcherWith(t, src, mt, nil)
	for range 3 {
		w.poll(context.Background())
	}
	if mt.calls != 0 {
		t.Errorf("read the windows %d times with nothing firing, want 0", mt.calls)
	}
}

// The covering window can sit on any page of the open-windows lookup.
func TestAlertWatcherReadsEveryPageOfWindows(t *testing.T) {
	src := newFakeAlertSource(alertReply{body: promBody(t)}, alertReply{body: promBody(t, pairAlert())})
	mt := &fakeMaintenance{pages: [][]store.MaintenanceWindow{{windowAround("n9")}, {windowAround("n1")}}}
	w, n, m := newMaintenanceWatcherWith(t, src, mt, nil)
	w.poll(context.Background())
	w.poll(context.Background())
	if got := n.recorded(); len(got) != 0 {
		t.Fatalf("an alert covered by a window on the second page was delivered: %+v", got)
	}
	if mt.calls != 2 {
		t.Errorf("window pages read = %d, want 2", mt.calls)
	}
	if got := suppressedCount(m, store.WebhookEventAlertFired); got != 1 {
		t.Errorf("WebhookSuppressed(alert.fired) = %v, want 1", got)
	}
}

// An alert delivered before a window opened still gets its alert.resolved inside it: the window holds
// only edges it saw begin.
func TestAlertWatcherResolvesAPreWindowAlertInsideAWindow(t *testing.T) {
	src := newFakeAlertSource(
		alertReply{body: promBody(t)},
		alertReply{body: promBody(t, pairAlert())},
		alertReply{body: promBody(t)},
	)
	mt := &fakeMaintenance{}
	w, n, m := newMaintenanceWatcherWith(t, src, mt, nil)
	w.poll(context.Background())
	w.poll(context.Background()) // fires, no window: delivered
	mt.windows = []store.MaintenanceWindow{windowAround("")}
	w.poll(context.Background()) // resolves inside the window
	got := n.recorded()
	if len(got) != 2 || got[0].event != store.WebhookEventAlertFired || got[1].event != store.WebhookEventAlertResolved {
		t.Fatalf("delivered %+v, want alert.fired then alert.resolved", got)
	}
	if got := suppressedCount(m, store.WebhookEventAlertResolved); got != 0 {
		t.Errorf("WebhookSuppressed(alert.resolved) = %v, want 0", got)
	}
}

// deadlineSource records whether each database read carried a deadline.
type deadlineSource struct {
	windowsBounded, rulesBounded []bool
}

func (d *deadlineSource) ListMaintenanceWindows(ctx context.Context, _ store.MaintenanceFilter) (store.MaintenancePage, error) {
	_, ok := ctx.Deadline()
	d.windowsBounded = append(d.windowsBounded, ok)
	return store.MaintenancePage{}, nil
}

func (d *deadlineSource) ListAlertRules(ctx context.Context, _ bool) ([]store.AlertRule, error) {
	_, ok := ctx.Deadline()
	d.rulesBounded = append(d.rulesBounded, ok)
	return nil, nil
}

// A hung database must not stall the watcher: every read in a poll runs under the poll's deadline.
func TestAlertWatcherBoundsItsDatabaseReadsByThePollTimeout(t *testing.T) {
	src := newFakeAlertSource(alertReply{body: promBody(t)}, alertReply{body: promBody(t, pairAlert())})
	db := &deadlineSource{}
	w, _, _ := newMaintenanceWatcherWith(t, src, db, db)
	w.poll(context.Background())
	w.poll(context.Background())
	if len(db.windowsBounded) == 0 || len(db.rulesBounded) == 0 {
		t.Fatalf("reads: windows=%v rules=%v, want both read", db.windowsBounded, db.rulesBounded)
	}
	for i, ok := range append(db.windowsBounded, db.rulesBounded...) {
		if !ok {
			t.Errorf("read %d ran without a deadline", i)
		}
	}
}

// countingRuleSource counts alert_rules reads.
type countingRuleSource struct {
	fakeRuleSource
	calls int
}

func (c *countingRuleSource) ListAlertRules(ctx context.Context, enabledOnly bool) ([]store.AlertRule, error) {
	c.calls++
	return c.fakeRuleSource.ListAlertRules(ctx, enabledOnly)
}

// Enrichment, the restart hold and the pending resolves all want the rule table; one poll reads it
// once. The poll measured is the first good window read after a restart, where all three run.
func TestAlertWatcherReadsTheRulesOncePerPoll(t *testing.T) {
	a := pairAlertActiveAt(testNow.Add(-30 * time.Minute))
	b := managedAlert(testOtherRuleID, "Other", map[string]string{"source_node": "n1", "destination_node": "n2"})
	src := newFakeAlertSource(
		alertReply{body: promBody(t, a, b)}, // baseline, windows unreadable
		alertReply{body: promBody(t, a)},    // b resolves while undecided
		alertReply{body: promBody(t, a)},    // windows readable again
	)
	mt := &fakeMaintenance{windows: []store.MaintenanceWindow{windowAround("")}, err: errors.New("db starting")}
	rules := &countingRuleSource{fakeRuleSource: fakeRuleSource{rules: []store.AlertRule{
		{ID: testRuleID, Name: "pair-loss-high", ForNs: int64(5 * time.Minute)},
	}}}
	w, _, _ := newMaintenanceWatcherWith(t, src, mt, rules)
	w.poll(context.Background())
	w.poll(context.Background())
	mt.err = nil
	before := rules.calls
	w.poll(context.Background())
	if got := rules.calls - before; got != 1 {
		t.Errorf("alert_rules read %d times in one poll, want 1", got)
	}
}

func TestPairArrowMatchesTheAnnotationsVocabulary(t *testing.T) {
	if pairArrow != events.PairArrow {
		t.Fatalf("pairArrow %q drifted from events.PairArrow %q", pairArrow, events.PairArrow)
	}
}
