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
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// DefaultAlertPollInterval is the cadence AlertWatcher polls Prometheus' alert
// state at, and therefore the granularity of every alert.resolved timestamp it
// produces. Repeated from config so this package can be constructed and tested
// without importing config at all -- keyLen's precedent.
const DefaultAlertPollInterval = 30 * time.Second

// alertPollTimeout bounds ONE poll. Deliberately shorter than the default
// interval: a poll that outlived its own cadence would queue behind itself, and
// the honest behaviour for a Prometheus that is not answering is to give up on
// this cycle and try the next one, freezing the firing set in the meantime.
const alertPollTimeout = 15 * time.Second

// watcherLogRateLimit is how often one CLASS of poll failure may be logged. A
// Prometheus that is down produces the same failure every interval forever, and
// the point of the limiter is to say that once a minute rather than twice a
// minute for the lifetime of the process.
const watcherLogRateLimit = time.Minute

// promAlertStateFiring is the only upstream state this watcher acts on.
// "pending" means the expression matches but the rule's `for` window has not
// elapsed -- the alert has NOT fired, and delivering on it would make every
// rule's `for` a lie.
const promAlertStateFiring = "firing"

// AlertSource is the read seam onto Prometheus' current alert set: exactly the
// signature promql.Client.Alerts already has, which is what makes it a seam
// rather than an adapter. The body is the upstream /api/v1/alerts envelope,
// unre-shaped -- promql never interprets what Prometheus said, so this package
// does the decoding it needs and nothing more.
type AlertSource interface {
	Alerts(ctx context.Context) (json.RawMessage, error)
}

// AlertNotifier is the delivery seam. *Dispatcher satisfies it; a test
// substitutes a recorder and never opens a socket.
type AlertNotifier interface {
	NotifyAlert(ctx context.Context, event string, a Alert)
}

var _ AlertNotifier = (*Dispatcher)(nil)

// RuleSource resolves the two facts /api/v1/alerts cannot carry: the rendered
// PromQL expression and the console row's own name. Prometheus reports labels,
// annotations, state, activeAt and a value, and nothing else -- the expression
// lives in the CRD it evaluates and in the alert_rules row it was rendered
// from, so a payload that promises `expr` has to read it from one of those.
//
// It is OPTIONAL. With no rule source the watcher still detects every edge and
// still delivers; the alert simply carries the alertname label as its name and
// an EMPTY expr. That is the honest degradation: a missing expression is worth
// less than a missed page, and a guessed one is worth less than nothing.
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
	// Interval is the poll cadence; non-positive is repaired to
	// DefaultAlertPollInterval, promrules.New's posture (the config layer has
	// already rejected a bad value, and this constructor is also reachable from
	// a test that built the struct by hand).
	Interval time.Duration
}

// AlertWatcher turns Prometheus' alert STATE into alert.fired/alert.resolved
// webhook deliveries by polling it and diffing consecutive observations (M7
// Decisions 6 and 7).
//
// It lives in this package, next to the dispatcher, for one reason: it exists
// ONLY to fire deliveries. It is not alerting's, which is a pure renderer with
// no clock and no I/O, and it is not promrules', which reconciles CRDs and has
// no idea an endpoint exists -- put it in either and that package acquires a
// webhook dependency it otherwise does not have.
//
// Four postures, each a deliberate trade:
//
//   - BASELINE ON BOOT. The first successful poll records the firing set and
//     dispatches NOTHING. Whatever was already firing was already somebody's
//     problem, and a console restart that paged the fleet about it would make
//     every rolling update a false alarm. The cost is that a genuine transition
//     during the restart window is missed, which is the right way round.
//
//   - FREEZE ON FAILURE. A poll that fails, or answers something the console
//     cannot decode, changes NOTHING: the last known firing set is kept and no
//     resolution is dispatched. A dead Prometheus must never "resolve" the
//     fleet -- that is the most dangerous lie this component could tell.
//
//   - MANAGED ONLY. An alert with no kconmon_ng_rule_id label was written by
//     somebody else, is rendered by somebody else and is somebody else's to
//     route. The console lists it (GET /api/v1/alerts serves it), but it is not
//     webhook material.
//
//   - PER REPLICA. There is no leader election here and no advisory lock: every
//     console replica polls and every one dispatches, so N replicas deliver N
//     copies of each edge. That is deliberate for M7 -- a lock would trade
//     duplicate deliveries for MISSED ones whenever the holder is the replica
//     that dies -- and it is why AlertPayload documents
//     (event, ruleId, labels, firedAt) as a deduplication key that is stable
//     across replicas. A receiver that dedupes on it sees one alert.
type AlertWatcher struct {
	alerts   AlertSource
	rules    RuleSource
	notifier AlertNotifier
	interval time.Duration

	// firing is the last GOOD observation, keyed by fingerprint. It is only
	// ever replaced wholesale by a successful poll, which is what makes "freeze
	// on failure" a property of the code rather than of a comment.
	firing map[string]Alert
	// baselined is false until the first SUCCESSFUL poll. Not "the first poll":
	// a console that started while Prometheus was down must still baseline
	// rather than page when it comes back.
	baselined bool

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
		alerts:   d.Alerts,
		rules:    d.Rules,
		notifier: d.Notifier,
		interval: interval,
		firing:   map[string]Alert{},
		now:      now,
		sleep:    realSleep,
		logs:     newWatcherLogLimiter(now),
	}, nil
}

// Run polls immediately and then on every interval until ctx is cancelled.
// Spawned through cmd/console's `spawn` helper, whose wg.Wait blocks shutdown
// on this return.
//
// It polls FIRST and waits after, so a console that just started takes its
// baseline now rather than one interval from now -- the earlier the baseline,
// the shorter the window in which a real transition is invisible.
//
// The interval is NOT jittered, unlike the reconciler's. Jitter exists there to
// stop N replicas server-side-applying in lockstep; here the call is a plain
// GET against a Prometheus this console already proxies for every open browser
// tab, so lockstep costs nothing -- and an unjittered cadence is one an
// operator can actually reason about when reading an alert.resolved timestamp,
// whose honest error bar is exactly this interval.
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

	// Sorted so a fan-out is deterministic. Nothing downstream depends on the
	// order, but a test that asserts two edges should not have to sort them,
	// and an operator reading two adjacent log lines should see them in the
	// same order every time.
	for _, fp := range slices.Sorted(maps.Keys(observed)) {
		if _, known := w.firing[fp]; !known {
			a := observed[fp]
			w.notifier.NotifyAlert(ctx, store.WebhookEventAlertFired, a)
		}
	}
	resolvedAt := w.now().UTC()
	for _, fp := range slices.Sorted(maps.Keys(w.firing)) {
		if _, still := observed[fp]; still {
			continue
		}
		// The REMEMBERED alert, not a re-derived one: a resolution is about the
		// alert that was firing, and there is nothing left upstream to derive
		// it from. resolvedAt is when this poll noticed the absence, which is
		// the whole granularity -- Alert.ResolvedAt says so in full.
		a := w.firing[fp]
		at := resolvedAt
		a.ResolvedAt = &at
		w.notifier.NotifyAlert(ctx, store.WebhookEventAlertResolved, a)
	}
	w.firing = observed
}

// decode maps the upstream envelope onto the MANAGED, FIRING subset, keyed by
// fingerprint.
//
// It decodes here rather than reusing httpapi's projection because the two want
// different things: httpapi serves every alert including foreign ones, with
// state and value, to a browser; this wants the firing managed ones with the
// fields a payload carries. Sharing one projection would mean one of them
// carrying fields it does not use, and the coupling would run the wrong way --
// httpapi already depends on nothing in this package.
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
	// Every rule, not just enabled ones: an alert can still be firing in
	// Prometheus for a rule an operator disabled seconds ago (the reconciler
	// has not re-applied yet), and resolving its name from the row is exactly
	// as correct then as it is now.
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

// fingerprint identifies ONE alert instance: the rule it came from and the
// exact series it fired for. One rule fires per series, so the rule id alone
// would collapse a per-zone alert into a single edge that flaps as any one zone
// clears.
//
// The label set is hashed rather than joined into the key because a label VALUE
// can be arbitrary text (a pair name, a target URL) and the resulting key would
// be unbounded. Pairs are length-prefixed before hashing so a key/value
// boundary cannot be forged across it: {"a":"b=c"} and {"a=b":"c"} are
// different alerts and must not share a fingerprint.
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

// watcherLogLimiter admits one line per key per watcherLogRateLimit. Its
// keyspace is CLOSED by construction -- three literal strings -- so it needs no
// cap, promrules' logLimiter's reasoning. It is a second copy rather than an
// export because a shared one would be a package neither of them owns for
// fifteen lines of map.
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
