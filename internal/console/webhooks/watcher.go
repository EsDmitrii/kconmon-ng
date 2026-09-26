package webhooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/alerting"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// DefaultAlertPollInterval is the cadence AlertWatcher polls Prometheus' alert state at.
const DefaultAlertPollInterval = 30 * time.Second

// alertPollTimeout bounds ONE poll; deliberately shorter than the default interval.
const alertPollTimeout = 15 * time.Second

// watcherLogRateLimit is how often one CLASS of poll failure may be logged; a Prometheus that is
// down produces the same failure every interval forever.
const watcherLogRateLimit = time.Minute

// promAlertStateFiring is the only upstream state this watcher acts on.
const promAlertStateFiring = "firing"

// AlertSource is the read seam onto Prometheus' current alert set: exactly the signature
// promql.Client.Alerts already has.
type AlertSource interface {
	Alerts(ctx context.Context) (json.RawMessage, error)
}

// AlertNotifier is the delivery seam. *Dispatcher satisfies it; a test
// substitutes a recorder and never opens a socket.
type AlertNotifier interface {
	NotifyAlert(ctx context.Context, event string, a Alert)
}

var _ AlertNotifier = (*Dispatcher)(nil)

// RuleSource resolves what /api/v1/alerts cannot carry: the rendered PromQL expression, the console
// row's own name, and the rule's `for`, which the restart hold needs.
type RuleSource interface {
	ListAlertRules(ctx context.Context, enabledOnly bool) ([]store.AlertRule, error)
}

var _ RuleSource = (*store.DB)(nil)

// AlertWatcherDeps is the AlertWatcher construction payload.
type AlertWatcherDeps struct {
	Alerts   AlertSource
	Notifier AlertNotifier
	// Rules is optional -- see RuleSource.
	Rules RuleSource
	// Maintenance, when set, holds back alert edges whose labels an open window's scope covers.
	Maintenance MaintenanceSource
	// Metrics, when set, counts the held edges.
	Metrics *metrics.Metrics
	// Interval is the poll cadence; non-positive is repaired to DefaultAlertPollInterval.
	Interval time.Duration
}

// MaintenanceSource reads maintenance windows; *store.DB satisfies it. nil holds nothing back.
type MaintenanceSource interface {
	ListMaintenanceWindows(ctx context.Context, f store.MaintenanceFilter) (store.MaintenancePage, error)
}

// pairArrow is events.PairArrow, the annotations vocabulary a pair-scoped window is written in. A
// copy rather than an import keeps this package off the events graph; a test pins the two together.
const pairArrow = "→"

// maintenanceWindowsPageLimit bounds one page of the open-windows lookup; a console has a handful.
const maintenanceWindowsPageLimit = 200

// AlertWatcher turns Prometheus' alert STATE into alert.fired/alert.resolved webhook deliveries by
// polling it and diffing consecutive observations; it lives in this package, next to the
// dispatcher.
type AlertWatcher struct {
	alerts      AlertSource
	rules       RuleSource
	notifier    AlertNotifier
	maintenance MaintenanceSource
	metrics     *metrics.Metrics
	interval    time.Duration

	// firing is the last GOOD observation, keyed by fingerprint. It is only
	// ever replaced wholesale by a successful poll, which is what makes "freeze
	// on failure" a property of the code rather than of a comment.
	firing map[string]Alert
	// baselined is false until the first SUCCESSFUL poll. Not "the first poll":
	// a console that started while Prometheus was down must still baseline
	// rather than page when it comes back.
	baselined bool
	// suppressed holds the fingerprints whose fired edge a window held back: their resolved edge is
	// held too, and they are delivered as fired if the window closes while they still fire.
	suppressed map[string]struct{}
	// unchecked holds the baseline's fingerprints until a window read decides which of them a previous
	// process held; suppressed lives in memory, so a restart would otherwise drop those fired edges.
	unchecked map[string]struct{}
	// pendingResolve holds the resolves of unchecked alerts that ended before that read succeeded,
	// judged later by the windows open at baselineAt.
	pendingResolve map[string]Alert
	baselineAt     time.Time

	// now and sleep are indirected for the tests, the dispatcher's idiom: a
	// loop asserted against a real clock is a thirty-second test.
	now   func() time.Time
	sleep sleeper

	logs *watcherLogLimiter
}

// NewAlertWatcher builds a watcher. It never touches the network. The only
// errors are nil seams, which are programmer errors -- an operator cannot
// produce one.
func NewAlertWatcher(d AlertWatcherDeps) (*AlertWatcher, error) { //nolint:gocritic // hugeParam: Deps is a construction payload, matching promrules.Deps
	if d.Alerts == nil {
		return nil, errors.New("webhooks: alert watcher: an alert source is required")
	}
	if d.Notifier == nil {
		return nil, errors.New("webhooks: alert watcher: a notifier is required")
	}
	interval := d.Interval
	if interval <= 0 {
		interval = DefaultAlertPollInterval
	}
	now := time.Now
	return &AlertWatcher{
		alerts:      d.Alerts,
		rules:       d.Rules,
		notifier:    d.Notifier,
		maintenance: d.Maintenance,
		metrics:     d.Metrics,
		interval:    interval,
		firing:      map[string]Alert{},
		suppressed:  map[string]struct{}{},
		unchecked:   map[string]struct{}{},
		now:         now,
		sleep:       realSleep,
		logs:        newWatcherLogLimiter(now),

		pendingResolve: map[string]Alert{},
	}, nil
}

// openWindows returns the maintenance windows open at now, and false when they could not be read. A
// failed read fails OPEN for new edges only (a held-back edge is noise, a lost one is an outage nobody
// hears of); an edge already held stays held until a read shows its window closed.
func (w *AlertWatcher) openWindows(ctx context.Context, now time.Time) ([]store.MaintenanceWindow, bool) {
	if w.maintenance == nil {
		return nil, true
	}
	var open []store.MaintenanceWindow
	f := store.MaintenanceFilter{From: now, To: now.Add(time.Nanosecond), Limit: maintenanceWindowsPageLimit}
	for {
		page, err := w.maintenance.ListMaintenanceWindows(ctx, f)
		if err != nil {
			if w.metrics != nil {
				w.metrics.WebhookMaintenanceReadErrors.WithLabelValues().Inc()
			}
			if w.logs.allow("maintenance") {
				slog.Warn("alert webhook watcher: reading maintenance windows failed, new alerts are "+
					"delivered without suppression and held ones stay held", "error", err)
			}
			return nil, false
		}
		for i := range page.Windows {
			if mw := &page.Windows[i]; !now.Before(mw.StartAt) && now.Before(mw.EndAt) {
				open = append(open, *mw)
			}
		}
		if page.NextCursor == "" {
			return open, true
		}
		f.Cursor = page.NextCursor
	}
}

// windowsFor reads the open windows only when this poll has something they could decide: a new
// fingerprint or a held one. Baseline alerts not yet checked are judged by baselineWindows instead.
func (w *AlertWatcher) windowsFor(ctx context.Context, observed map[string]Alert) ([]store.MaintenanceWindow, bool) {
	need := len(w.suppressed) > 0
	for fp := range observed {
		if _, known := w.firing[fp]; !known {
			need = true
			break
		}
	}
	if !need {
		return nil, true
	}
	return w.openWindows(ctx, w.now())
}

// covered reports whether an open window's scope covers an alert: a global window, one of the
// alert's two nodes, its directed pair, or its external target.
func covered(windows []store.MaintenanceWindow, labels map[string]string) bool {
	for i := range windows {
		if scopeCovers(windows[i].Scope, labels) {
			return true
		}
	}
	return false
}

func scopeCovers(scope string, labels map[string]string) bool {
	src, dst, target := labels["source_node"], labels["destination_node"], labels["target"]
	switch {
	case scope == "":
		return true
	case scope == src || scope == dst || scope == target:
		return true
	case src != "" && dst != "" && scope == src+pairArrow+dst:
		return true
	}
	return false
}

// holdUnchecked holds the baseline alerts a previous process held: those that fired after a covering
// window open at the baseline was declared and started (pre-window alerts were delivered, so their
// resolve must go out). firedAt is when the alert went pending, hence the rule's `for`, plus one
// interval of poll lag. windows are the ones open at baselineAt, as for decidePendingResolves: the
// first good read may come after the window closed, and a held alert that still fires then must be
// delivered as fired, not forgotten.
func (w *AlertWatcher) holdUnchecked(windows []store.MaintenanceWindow, rules ruleLookup) {
	if len(w.unchecked) == 0 {
		return
	}
	defer clear(w.unchecked)
	var candidates []string
	for fp := range w.unchecked {
		if a, ok := w.firing[fp]; ok && covered(windows, a.Labels) {
			candidates = append(candidates, fp)
		}
	}
	if len(candidates) == 0 {
		return
	}
	byID := rules()
	for _, fp := range candidates {
		if a := w.firing[fp]; w.heldBefore(&a, windows, byID) {
			w.suppressed[fp] = struct{}{}
			w.countSuppressed(store.WebhookEventAlertFired)
		}
	}
}

// heldBefore reports whether a previous process held a's fired edge: a covering window among windows
// was declared and started before that process first saw a firing. A rule missing from rules (or an
// unreadable table) is judged by activeAt alone.
func (w *AlertWatcher) heldBefore(a *Alert, windows []store.MaintenanceWindow, rules map[string]*store.AlertRule) bool {
	seenBy := a.FiredAt.Add(w.interval)
	if row, ok := rules[a.RuleID]; ok {
		seenBy = seenBy.Add(time.Duration(row.ForNs))
	}
	for i := range windows {
		mw := &windows[i]
		declared := mw.StartAt
		if mw.CreatedAt.After(declared) {
			declared = mw.CreatedAt
		}
		if scopeCovers(mw.Scope, a.Labels) && !seenBy.Before(declared) {
			return true
		}
	}
	return false
}

// decidePendingResolves settles the resolves of baseline alerts that ended before any window read
// succeeded, judged by windows, the ones open when the baseline was taken.
func (w *AlertWatcher) decidePendingResolves(ctx context.Context, windows []store.MaintenanceWindow, rules ruleLookup) {
	var byID map[string]*store.AlertRule
	if len(windows) > 0 {
		byID = rules()
	}
	for _, fp := range slices.Sorted(maps.Keys(w.pendingResolve)) {
		if a := w.pendingResolve[fp]; w.heldBefore(&a, windows, byID) {
			w.countSuppressed(store.WebhookEventAlertResolved)
		} else {
			w.notifier.NotifyAlert(ctx, store.WebhookEventAlertResolved, a)
		}
	}
	clear(w.pendingResolve)
}

// baselineWindows reads the windows open at baselineAt while a baseline alert is still undecided, once
// per poll for holdUnchecked and decidePendingResolves both; false means nothing to decide or a failed
// read, which keeps them waiting.
func (w *AlertWatcher) baselineWindows(ctx context.Context) ([]store.MaintenanceWindow, bool) {
	if len(w.unchecked) == 0 && len(w.pendingResolve) == 0 {
		return nil, false
	}
	return w.openWindows(ctx, w.baselineAt)
}

// ruleLookup returns the alert_rules rows by id, read at most once per poll; nil means no rule
// source or a failed read.
type ruleLookup func() map[string]*store.AlertRule

// rulesOnce is the poll's ruleLookup. Every rule, not just enabled ones: an alert can still be firing
// in Prometheus for a rule an operator disabled seconds ago (the reconciler has not re-applied yet).
// A failed read is logged and degrades each user: payloads go out without expr, and the restart hold
// judges by activeAt alone.
func (w *AlertWatcher) rulesOnce(ctx context.Context) ruleLookup {
	return sync.OnceValue(func() map[string]*store.AlertRule {
		if w.rules == nil {
			return nil
		}
		rows, err := w.rules.ListAlertRules(ctx, false)
		if err != nil {
			if w.logs.allow("rules") {
				slog.Warn("alert webhook watcher: reading alert rules failed — deliveries continue "+
					"with the label set and an empty expr, and the restart hold judges by activeAt alone",
					"error", err)
			}
			return nil
		}
		byID := make(map[string]*store.AlertRule, len(rows))
		for i := range rows {
			byID[rows[i].ID] = &rows[i]
		}
		return byID
	})
}

func (w *AlertWatcher) countSuppressed(event string) {
	if w.metrics != nil {
		w.metrics.WebhookSuppressed.WithLabelValues(event).Inc()
	}
}

// Run polls immediately and then on every interval until ctx is cancelled; it polls FIRST and waits
// after, so a console that just started takes its baseline now rather than one interval from now.
func (w *AlertWatcher) Run(ctx context.Context) {
	for ctx.Err() == nil {
		w.poll(ctx)
		if !w.sleep(ctx, w.interval) {
			return
		}
	}
}

// poll performs one observation and dispatches the edges it implies. Every
// early return is a FREEZE: the firing set is untouched and nothing is
// delivered.
func (w *AlertWatcher) poll(ctx context.Context) {
	pollCtx, cancel := context.WithTimeout(ctx, alertPollTimeout)
	defer cancel()

	raw, err := w.alerts.Alerts(pollCtx)
	if err != nil {
		// The error carries the configured Prometheus URL, so it is logged
		// (never surfaced) and rate-limited by CLASS: a Prometheus that is down
		// produces this every interval forever.
		if w.logs.allow("poll") {
			slog.Warn("alert webhook watcher: reading prometheus alert state failed — "+
				"the last known firing set is KEPT and nothing is resolved",
				"error", err)
		}
		return
	}
	observed, err := w.decode(raw)
	if err != nil {
		if w.logs.allow("decode") {
			slog.Warn("alert webhook watcher: prometheus answered /api/v1/alerts with a body the "+
				"console could not read — the last known firing set is KEPT and nothing is resolved",
				"error", err)
		}
		return
	}

	if !w.baselined {
		w.firing, w.baselined, w.baselineAt = observed, true, w.now()
		if w.maintenance != nil {
			for fp := range observed {
				w.unchecked[fp] = struct{}{}
			}
		}
		if windows, ok := w.baselineWindows(pollCtx); ok {
			w.holdUnchecked(windows, w.rulesOnce(pollCtx))
		}
		slog.Info("alert webhook watcher: baseline taken, no notifications will be sent for alerts "+
			"that were already firing", "managedFiring", len(observed), "held", len(w.suppressed))
		return
	}

	rules := w.rulesOnce(pollCtx)
	enrich(observed, rules)

	// Before windowsFor: an alert holdUnchecked holds makes that read necessary, and its answer then
	// decides whether the held alert.fired goes out on this poll.
	baseline, baselineOK := w.baselineWindows(pollCtx)
	if baselineOK {
		w.holdUnchecked(baseline, rules)
	}
	windows, windowsOK := w.windowsFor(pollCtx, observed)
	if baselineOK && len(w.pendingResolve) > 0 {
		w.decidePendingResolves(ctx, baseline, rules)
	}

	// Sorted so a fan-out is deterministic.
	for _, fp := range slices.Sorted(maps.Keys(observed)) {
		a := observed[fp]
		// Firing again: a late resolve of its earlier run would close the new one.
		delete(w.pendingResolve, fp)
		_, known := w.firing[fp]
		_, held := w.suppressed[fp]
		switch {
		case !known && covered(windows, a.Labels):
			w.suppressed[fp] = struct{}{}
			w.countSuppressed(store.WebhookEventAlertFired)
		case !known:
			w.notifier.NotifyAlert(ctx, store.WebhookEventAlertFired, a)
		case held && windowsOK && !covered(windows, a.Labels):
			// The window closed on an alert that kept firing: that is news now.
			delete(w.suppressed, fp)
			w.notifier.NotifyAlert(ctx, store.WebhookEventAlertFired, a)
		}
	}
	resolvedAt := w.now().UTC()
	for _, fp := range slices.Sorted(maps.Keys(w.firing)) {
		if _, still := observed[fp]; still {
			continue
		}
		if _, undecided := w.unchecked[fp]; undecided {
			// Only a failed window read leaves a baseline alert unchecked; whether the previous
			// process held its fired edge is still unknown, so the resolve waits for a read.
			delete(w.unchecked, fp)
			a := w.firing[fp]
			at := resolvedAt
			a.ResolvedAt = &at
			w.pendingResolve[fp] = a
			continue
		}
		if _, held := w.suppressed[fp]; held {
			// Its fired edge never went out; a lone resolved would page about nothing.
			delete(w.suppressed, fp)
			w.countSuppressed(store.WebhookEventAlertResolved)
			continue
		}
		// The REMEMBERED alert, not a re-derived one: a resolution is about the alert that was firing.
		a := w.firing[fp]
		at := resolvedAt
		a.ResolvedAt = &at
		w.notifier.NotifyAlert(ctx, store.WebhookEventAlertResolved, a)
	}
	w.firing = observed
}

// decode maps the upstream envelope onto the MANAGED, FIRING subset, keyed by fingerprint; it
// decodes here rather than reusing httpapi's projection because the two want different things.
func (w *AlertWatcher) decode(raw json.RawMessage) (map[string]Alert, error) {
	var envelope struct {
		Status string `json:"status"`
		Data   struct {
			Alerts []struct {
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
				State       string            `json:"state"`
				ActiveAt    *time.Time        `json:"activeAt"`
			} `json:"alerts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if envelope.Status != "success" {
		return nil, errors.New("prometheus reported status " + strconv.Quote(envelope.Status))
	}

	observed := make(map[string]Alert, len(envelope.Data.Alerts))
	for i := range envelope.Data.Alerts {
		a := &envelope.Data.Alerts[i]
		if a.State != promAlertStateFiring {
			continue
		}
		ruleID := a.Labels[alerting.RuleIDLabel]
		if ruleID == "" {
			// Unmanaged. Somebody else's rule, somebody else's routing.
			continue
		}
		labels := a.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		annotations := a.Annotations
		if annotations == nil {
			annotations = map[string]string{}
		}
		// A missing activeAt still needs a firedAt -- the key is not optional.
		// The observing clock is the only honest fallback available.
		firedAt := w.now().UTC()
		if a.ActiveAt != nil {
			firedAt = a.ActiveAt.UTC()
		}
		observed[fingerprint(ruleID, labels)] = Alert{
			RuleID:      ruleID,
			RuleName:    labels["alertname"],
			Severity:    labels[alerting.SeverityLabel],
			Labels:      labels,
			Annotations: annotations,
			FiredAt:     firedAt,
		}
	}
	return observed, nil
}

// enrich fills RuleName and Expr from the alert_rules rows, for the alerts that have a row. An
// unreadable table leaves them as decoded: the transition is the news and the expression is
// decoration, so a degraded payload beats a dropped one.
func enrich(observed map[string]Alert, rules ruleLookup) {
	if len(observed) == 0 {
		return
	}
	byID := rules()
	for fp, a := range observed {
		row, ok := byID[a.RuleID]
		if !ok {
			continue
		}
		a.RuleName = row.Name
		a.Expr = row.RenderedExpr
		observed[fp] = a
	}
}

// fingerprint identifies ONE alert instance; one rule fires per series, so the rule id alone would
// collapse a per-zone alert into a single edge that flaps as any one zone clears.
func fingerprint(ruleID string, labels map[string]string) string {
	h := sha256.New()
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		v := labels[k]
		// hash.Hash.Write never returns an error (the interface says so
		// outright), so the count is written directly rather than through
		// Fprintf's error-returning path.
		h.Write([]byte(strconv.Itoa(len(k))))
		h.Write([]byte(k))
		h.Write([]byte(strconv.Itoa(len(v))))
		h.Write([]byte(v))
	}
	return ruleID + "/" + hex.EncodeToString(h.Sum(nil))
}

// watcherLogLimiter admits one line per key per watcherLogRateLimit; it is a second copy rather
// than an export because a shared one would be a package neither of them owns for fifteen lines of
// map.
type watcherLogLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
	now  func() time.Time
}

func newWatcherLogLimiter(now func() time.Time) *watcherLogLimiter {
	return &watcherLogLimiter{last: make(map[string]time.Time), now: now}
}

func (l *watcherLogLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if at, ok := l.last[key]; ok && now.Sub(at) < watcherLogRateLimit {
		return false
	}
	l.last[key] = now
	return true
}
