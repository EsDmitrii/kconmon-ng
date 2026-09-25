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

// RuleSource resolves the two facts /api/v1/alerts cannot carry: the rendered PromQL expression and
// the console row's own name.
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
		now:         now,
		sleep:       realSleep,
		logs:        newWatcherLogLimiter(now),
	}, nil
}

// openWindows returns the maintenance windows open at now. A lookup failure fails OPEN: an edge a
// window would have held back is noise, an edge lost to a database blip is an outage nobody hears of.
func (w *AlertWatcher) openWindows(ctx context.Context, now time.Time) []store.MaintenanceWindow {
	if w.maintenance == nil {
		return nil
	}
	var open []store.MaintenanceWindow
	f := store.MaintenanceFilter{From: now, To: now.Add(time.Nanosecond), Limit: maintenanceWindowsPageLimit}
	for {
		page, err := w.maintenance.ListMaintenanceWindows(ctx, f)
		if err != nil {
			if w.logs.allow("maintenance") {
				slog.Warn("alert webhook watcher: reading maintenance windows failed, delivering "+
					"without suppression", "error", err)
			}
			return nil
		}
		for i := range page.Windows {
			if mw := &page.Windows[i]; !now.Before(mw.StartAt) && now.Before(mw.EndAt) {
				open = append(open, *mw)
			}
		}
		if page.NextCursor == "" {
			return open
		}
		f.Cursor = page.NextCursor
	}
}

// covered reports whether an open window's scope covers an alert: a global window, one of the
// alert's two nodes, its directed pair, or its external target.
func covered(windows []store.MaintenanceWindow, labels map[string]string) bool {
	src, dst, target := labels["source_node"], labels["destination_node"], labels["target"]
	for i := range windows {
		switch scope := windows[i].Scope; {
		case scope == "":
			return true
		case scope == src || scope == dst || scope == target:
			return true
		case src != "" && dst != "" && scope == src+pairArrow+dst:
			return true
		}
	}
	return false
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
		w.firing, w.baselined = observed, true
		slog.Info("alert webhook watcher: baseline taken, no notifications will be sent for alerts "+
			"that were already firing", "managedFiring", len(observed))
		return
	}

	w.enrich(ctx, observed)

	windows := w.openWindows(ctx, w.now())

	// Sorted so a fan-out is deterministic.
	for _, fp := range slices.Sorted(maps.Keys(observed)) {
		a := observed[fp]
		_, known := w.firing[fp]
		_, held := w.suppressed[fp]
		switch {
		case !known && covered(windows, a.Labels):
			w.suppressed[fp] = struct{}{}
			w.countSuppressed(store.WebhookEventAlertFired)
		case !known:
			w.notifier.NotifyAlert(ctx, store.WebhookEventAlertFired, a)
		case held && !covered(windows, a.Labels):
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

// enrich fills RuleName and Expr from the alert_rules rows, for the alerts that
// have a row. A failure here is logged and IGNORED: the transition is the news
// and the expression is decoration, so a degraded payload beats a dropped one.
func (w *AlertWatcher) enrich(ctx context.Context, observed map[string]Alert) {
	if w.rules == nil || len(observed) == 0 {
		return
	}
	// Every rule, not just enabled ones: an alert can still be firing in Prometheus for a rule an
	// operator disabled seconds ago (the reconciler has not re-applied yet).
	rows, err := w.rules.ListAlertRules(ctx, false)
	if err != nil {
		if w.logs.allow("rules") {
			slog.Warn("alert webhook watcher: reading alert rules for payload enrichment failed — "+
				"deliveries continue with the label set and an empty expr", "error", err)
		}
		return
	}
	byID := make(map[string]*store.AlertRule, len(rows))
	for i := range rows {
		byID[rows[i].ID] = &rows[i]
	}
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
