// Package promrules is the Console's PrometheusRule sync; Prometheus evaluates alert rules.
package promrules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	mathrand "math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/EsDmitrii/kconmon-ng/internal/console/alerting"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// GVR is the ONE resource this package touches. Written out rather than
// derived from a discovery client: discovery would be a second API surface to
// grant, and the group/version is pinned by the operator's CRD, not negotiated.
var GVR = schema.GroupVersionResource{
	Group:    "monitoring.coreos.com",
	Version:  "v1",
	Resource: "prometheusrules",
}

const (
	// FieldManager is the server-side-apply field manager the console owns; every apply uses it AND
	// force=true, which is correct precisely because the object is ours end to end.
	FieldManager = "kconmon-ng-console"

	// DefaultInterval backs up config validation; the reconciler is only ever built from a validated
	// config.
	DefaultInterval = 60 * time.Second

	// jitterFraction is the +/-20% spread on the interval; same constant and same idiom as the webhook
	// dispatcher's retry spread, for the same reason.
	jitterFraction = 0.2

	// syncMessageMaxLen mirrors store's alertRuleSyncMessageMaxLen; the store would reject a longer
	// message and the reconciler would then log a write failure instead of recording the outcome it
	// just observed.
	syncMessageMaxLen = 1024
	truncationMarker  = "..."

	// logRateLimit bounds this package's own logging.
	logRateLimit = time.Minute
)

// The cause classes a failed apply is reported as. They are a CLOSED set and
// they are the first token of the sync message, so an operator (and a future
// UI) can branch on the cause without parsing a Kubernetes error string.
const (
	// CauseCRDMissing: the resource itself is not served. The Prometheus
	// Operator is not installed, or its CRDs were removed.
	CauseCRDMissing = "crd-missing"
	// CauseForbidden: the resource is served but this ServiceAccount may not
	// write it. The Role/RoleBinding was not applied, or points elsewhere.
	CauseForbidden = "forbidden"
	// CauseOther: anything else -- a conflict, a webhook rejection, an
	// apiserver outage. Deliberately one bucket: the classes exist to tell
	// apart the two failures an operator can FIX by applying a manifest.
	CauseOther = "other"
)

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

// Client is the namespace-scoped PrometheusRule surface. It is namespaced ONLY
// -- there is no cluster-scoped path in this type at all -- so a bug cannot
// widen the blast radius past what the Role grants.
type Client struct {
	ri        dynamic.ResourceInterface
	namespace string
}

// NewClient binds dyn to one namespace; the dynamic.Interface is built by the caller (cmd/console,
// from the in-cluster REST config) for kubectx's reason.
func NewClient(dyn dynamic.Interface, namespace string) (*Client, error) {
	if dyn == nil {
		return nil, errors.New("promrules: dynamic client must not be nil")
	}
	ns := strings.TrimSpace(namespace)
	if ns == "" {
		return nil, errors.New("promrules: namespace must not be empty")
	}
	return &Client{ri: dyn.Resource(GVR).Namespace(ns), namespace: ns}, nil
}

// Namespace reports the namespace this client is bound to.
func (c *Client) Namespace() string { return c.namespace }

// Apply server-side-applies obj; one call, no read-modify-write and no create-then-update fallback.
func (c *Client) Apply(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return c.ri.Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{
		FieldManager: FieldManager,
		Force:        true,
	})
}

// Get reads one object by name. A missing object and a missing CRD are both
// NotFound here and are NOT told apart: see Classify for why that ambiguity is
// resolved at the apply, not the read.
func (c *Client) Get(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	return c.ri.Get(ctx, name, metav1.GetOptions{})
}

// ForeignRule is one PrometheusRule in the namespace that the console does NOT own; it is listed
// read-only.
type ForeignRule struct {
	// Name is the object's name.
	Name string
	// Groups is how many entries spec.groups holds.
	Groups int
	// Rules is the total number of rule entries across all groups -- alerting
	// and recording alike, because a recording rule is still something an
	// import would have to carry.
	Rules int
	// ManagedBy is the value of app.kubernetes.io/managed-by, or "" when the object carries no such
	// label.
	ManagedBy string
	// Object is the raw object, handed straight to the API layer; carried rather than projected
	// because an import has to read the actual groups.
	Object *unstructured.Unstructured
}

// ListForeign returns every PrometheusRule in the namespace that is not ours, sorted by name; the
// server-side alternative (`app.kubernetes.io/managed-by!=kconmon-ng-console`) is subtly
// wrong-adjacent.
func (c *Client) ListForeign(ctx context.Context) ([]ForeignRule, error) {
	list, err := c.ri.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list prometheusrules in %s: %w", c.namespace, err)
	}
	out := make([]ForeignRule, 0, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]
		managedBy := item.GetLabels()[alerting.ManagedByLabel]
		if managedBy == alerting.ManagedByValue {
			continue
		}
		groups, rules := countGroups(item)
		out = append(out, ForeignRule{
			Name:      item.GetName(),
			Groups:    groups,
			Rules:     rules,
			ManagedBy: managedBy,
			Object:    item.DeepCopy(),
		})
	}
	slices.SortFunc(out, func(a, b ForeignRule) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// countGroups counts spec.groups and the rule entries inside them; every shape that is not what the
// CRD promises counts as zero rather than erroring.
func countGroups(obj *unstructured.Unstructured) (groups, rules int) {
	raw, found, err := unstructured.NestedSlice(obj.Object, "spec", "groups")
	if !found || err != nil {
		return 0, 0
	}
	for _, g := range raw {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		groups++
		entries, ok := gm["rules"].([]any)
		if !ok {
			continue
		}
		rules += len(entries)
	}
	return groups, rules
}

// ---------------------------------------------------------------------------
// Drift
// ---------------------------------------------------------------------------

// Compare reports whether the live object diverges from the desired one on the fields the console
// RENDERS; only the rendered fields are compared, and that scope is the whole design.
func Compare(desired, live *unstructured.Unstructured) (drift bool, diff string) {
	want := renderRelevantJSON(desired)
	got := renderRelevantJSON(live)
	if want == got {
		return false, ""
	}
	return true, lineDiff(want, got)
}

// renderRelevantJSON projects an object down to the fields we own and renders them as stable.
func renderRelevantJSON(obj *unstructured.Unstructured) string {
	if obj == nil {
		return "<absent>"
	}
	spec, found, err := unstructured.NestedFieldNoCopy(obj.Object, "spec")
	if !found || err != nil {
		spec = nil
	}
	relevant := map[string]any{
		"labels": map[string]any{
			alerting.ManagedByLabel: obj.GetLabels()[alerting.ManagedByLabel],
		},
		"annotations": map[string]any{
			alerting.RuleIDsAnnotation: obj.GetAnnotations()[alerting.RuleIDsAnnotation],
		},
		"spec": spec,
	}
	out, err := json.MarshalIndent(relevant, "", "  ")
	if err != nil {
		// Unreachable for an unstructured object, which is JSON by
		// construction. Reported rather than swallowed: a marshal failure that
		// silently became "" would read as "no drift".
		return fmt.Sprintf("<unrenderable: %v>", err)
	}
	return string(out)
}

// lineDiff renders a compact unified-ish diff of two texts; an LCS would be O(n*m) per reconcile to
// produce a better rendering of a string that is then truncated to 1 KiB anyway.
func lineDiff(want, got string) string {
	a := strings.Split(want, "\n")
	b := strings.Split(got, "\n")

	head := 0
	for head < len(a) && head < len(b) && a[head] == b[head] {
		head++
	}
	tail := 0
	for tail < len(a)-head && tail < len(b)-head && a[len(a)-1-tail] == b[len(b)-1-tail] {
		tail++
	}

	var sb strings.Builder
	sb.WriteString("--- rendered (console)\n+++ live (cluster)\n")
	if head > 0 {
		fmt.Fprintf(&sb, "@@ %d identical leading line(s) elided @@\n", head)
	}
	for _, line := range a[head : len(a)-tail] {
		sb.WriteString("-" + line + "\n")
	}
	for _, line := range b[head : len(b)-tail] {
		sb.WriteString("+" + line + "\n")
	}
	if tail > 0 {
		fmt.Fprintf(&sb, "@@ %d identical trailing line(s) elided @@\n", tail)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// truncate bounds s at syncMessageMaxLen bytes INCLUDING the marker, cutting on a rune boundary.
func truncate(s string) string {
	if len(s) <= syncMessageMaxLen {
		return s
	}
	cut := syncMessageMaxLen - len(truncationMarker)
	end := 0
	for i := range s {
		if i > cut {
			break
		}
		end = i
	}
	return s[:end] + truncationMarker
}

// ---------------------------------------------------------------------------
// Error classification
// ---------------------------------------------------------------------------

// Classify names the cause class of a failed API call; that ambiguity is harmless HERE and only
// here: Apply creates the object when it is absent.
func Classify(err error) string {
	switch {
	case err == nil:
		return ""
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return CauseForbidden
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return CauseCRDMissing
	default:
		return CauseOther
	}
}

// causeMessage builds the operator-facing sync message: the cause class first
// so it can be branched on, then a sentence naming what to fix, then the API's
// own words. Bounded, because the column is.
func causeMessage(cause, namespace string, err error) string {
	var explain string
	switch cause {
	case CauseCRDMissing:
		explain = "the " + alerting.BundleKind + " CRD (" + alerting.BundleAPIVersion +
			") is not served by this cluster — install the Prometheus Operator CRDs, " +
			"or set console.alerting.enabled=false"
	case CauseForbidden:
		explain = "the console ServiceAccount may not write " + alerting.BundleKind +
			" objects in namespace " + namespace + " — apply the alerting Role and RoleBinding"
	default:
		explain = "the Kubernetes API rejected the apply"
	}
	return truncate(cause + ": " + explain + ": " + err.Error())
}

// ---------------------------------------------------------------------------
// Reconciler
// ---------------------------------------------------------------------------

// Store is the narrow store seam: the two methods this package calls.
type Store interface {
	// ListAlertRules(ctx, true) is the only call made: the reconciler renders
	// ENABLED rules and nothing else.
	ListAlertRules(ctx context.Context, enabledOnly bool) ([]store.AlertRule, error)
	UpdateAlertRuleSyncStatus(
		ctx context.Context, id, status, message string, lastSyncedAt *time.Time,
	) (store.AlertRule, error)
}

var _ Store = (*store.DB)(nil)

// Deps is the Reconciler's construction payload.
type Deps struct {
	Client   *Client
	Store    Store
	Renderer alerting.Renderer
	// BundleName is the name of the single object we own.
	BundleName string
	// Interval is the base reconcile cadence; non-positive is repaired to
	// DefaultInterval.
	Interval time.Duration
}

// Reconciler is the convergence loop. One per process; Run owns it.
type Reconciler struct {
	client     *Client
	store      Store
	renderer   alerting.Renderer
	bundleName string
	interval   time.Duration

	// kick is capacity 1, which IS the coalescing rule; anything larger would let a burst of CRUD
	// writes schedule a burst of identical applies.
	kick chan struct{}

	// now is time.Now indirected so a test can assert the lastSyncedAt that
	// reaches the store without comparing against a live clock.
	now func() time.Time

	logs *logLimiter
}

// New builds a Reconciler; it never touches the network and never fails on anything an operator can
// misconfigure.
func New(d Deps) (*Reconciler, error) { //nolint:gocritic // hugeParam: Deps is a construction payload, value semantics match ReconcilerDeps
	if d.Client == nil {
		return nil, errors.New("promrules: client must not be nil")
	}
	if d.Store == nil {
		return nil, errors.New("promrules: store must not be nil")
	}
	if strings.TrimSpace(d.BundleName) == "" {
		return nil, errors.New("promrules: bundle name must not be empty")
	}
	interval := d.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	now := time.Now
	return &Reconciler{
		client:     d.Client,
		store:      d.Store,
		renderer:   d.Renderer,
		bundleName: d.BundleName,
		interval:   interval,
		kick:       make(chan struct{}, 1),
		now:        now,
		logs:       newLogLimiter(now),
	}, nil
}

// Kick asks for a reconcile as soon as the loop is free; non-blocking and coalescing: it is called
// from HTTP handlers after every alert-rule write.
func (r *Reconciler) Kick() {
	select {
	case r.kick <- struct{}{}:
	default:
		// A pass is already queued. It will read the same table this kick
		// would have, so dropping this one loses nothing.
	}
}

// ListForeign delegates to the client, so callers that hold the reconciler do not additionally have
// to be handed the client.
func (r *Reconciler) ListForeign(ctx context.Context) ([]ForeignRule, error) {
	return r.client.ListForeign(ctx)
}

// Namespace reports the namespace the bundle is applied into.
func (r *Reconciler) Namespace() string { return r.client.Namespace() }

// Run reconciles immediately, then on every jittered interval and on every kick; it reconciles
// FIRST and waits after, so a console that just started applies the operator's rules now rather
// than a minute from now.
func (r *Reconciler) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := r.Reconcile(ctx); err != nil && ctx.Err() == nil {
			// Keyed by cause class, not by error text: a cluster with no CRD
			// produces the same failure forever, and the point of the limiter
			// is to say it once a minute instead of once a reconcile.
			if r.logs.allow("reconcile:" + Classify(err)) {
				slog.Warn("prometheus rule sync failed — alert rules stay in the database and "+
					"every enabled rule now carries the reason in its sync status",
					"namespace", r.client.Namespace(), "bundle", r.bundleName, "error", err)
			}
		}
		t := time.NewTimer(jitter(r.interval))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-r.kick:
			t.Stop()
		}
	}
}

// Reconcile is ONE pass: read the enabled rules, render, observe, apply, write the outcome back
// onto every rule.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	rows, err := r.store.ListAlertRules(ctx, true)
	if err != nil {
		// Nothing to write a status onto: the rules could not be read, so
		// there is no list of ids to mark. The loop logs and retries.
		return fmt.Errorf("list enabled alert rules: %w", err)
	}

	rules := make([]alerting.Rule, 0, len(rows))
	ids := make([]string, 0, len(rows))
	for i := range rows {
		rule, rerr := r.renderable(&rows[i])
		if rerr != nil {
			// One unrenderable row must not cost the other rules their sync.
			r.setStatus(ctx, rows[i].ID, store.AlertSyncStatusError, truncate("render: "+rerr.Error()), nil)
			continue
		}
		rules = append(rules, rule)
		ids = append(ids, rows[i].ID)
	}

	desired, err := r.renderer.RenderBundle(rules, r.client.Namespace(), r.bundleName)
	if err != nil {
		// A bundle-level failure is a relationship BETWEEN rules (an alert
		// name collision), so it cannot be attributed to one of them: every
		// surviving rule is unappliable and every one of them says so.
		msg := truncate("render bundle: " + err.Error())
		for _, id := range ids {
			r.setStatus(ctx, id, store.AlertSyncStatusError, msg, nil)
		}
		return fmt.Errorf("render bundle: %w", err)
	}

	// Observe BEFORE re-asserting. This read is the ONLY reason the loop does
	// a GET at all -- the apply needs no prior state.
	drift, diff := false, ""
	live, gerr := r.client.Get(ctx, r.bundleName)
	switch {
	case gerr == nil:
		drift, diff = Compare(desired, live)
	case apierrors.IsNotFound(gerr):
		// Either the object does not exist yet or the CRD does not. Neither is
		// drift, and the apply below is what tells them apart.
	default:
		// A failed read is not a reason to skip the write: converging matters
		// more than reporting. The drift observation is simply lost for this
		// pass.
		if r.logs.allow("get:" + Classify(gerr)) {
			slog.Warn("could not read the live PrometheusRule before applying — "+
				"drift will not be reported this pass, the apply still happens",
				"namespace", r.client.Namespace(), "bundle", r.bundleName, "error", gerr)
		}
	}

	if _, aerr := r.client.Apply(ctx, desired); aerr != nil {
		cause := Classify(aerr)
		msg := causeMessage(cause, r.client.Namespace(), aerr)
		for _, id := range ids {
			r.setStatus(ctx, id, store.AlertSyncStatusError, msg, nil)
		}
		return fmt.Errorf("apply %s %s/%s: %w", alerting.BundleKind, r.client.Namespace(), r.bundleName, aerr)
	}

	// The apply succeeded, so lastSyncedAt moves -- including for a rule reported as drift.
	now := r.now()
	status, message := store.AlertSyncStatusSynced, ""
	if drift {
		status, message = store.AlertSyncStatusDrift, truncate(diff)
	}
	for _, id := range ids {
		r.setStatus(ctx, id, status, message, &now)
	}
	return nil
}

// renderable turns one stored row into the renderer's input and proves it renders; the proof is a
// single-rule bundle rather than a bare Render.
func (r *Reconciler) renderable(row *store.AlertRule) (alerting.Rule, error) {
	params, err := decodeObject("params", row.Params)
	if err != nil {
		return alerting.Rule{}, err
	}
	labels, err := decodeStringMap("labels", row.Labels)
	if err != nil {
		return alerting.Rule{}, err
	}
	annotations, err := decodeStringMap("annotations", row.Annotations)
	if err != nil {
		return alerting.Rule{}, err
	}
	rule := alerting.Rule{
		ID:          row.ID,
		Name:        row.Name,
		Kind:        row.Kind,
		Params:      params,
		Severity:    row.Severity,
		ForNS:       row.ForNs,
		Labels:      labels,
		Annotations: annotations,
		Enabled:     row.Enabled,
	}
	if _, err := r.renderer.RenderBundle([]alerting.Rule{rule}, r.client.Namespace(), r.bundleName); err != nil {
		return alerting.Rule{}, err
	}
	return rule, nil
}

// setStatus records one rule's outcome; a failed write is logged and swallowed on purpose.
func (r *Reconciler) setStatus(ctx context.Context, id, status, message string, at *time.Time) {
	if _, err := r.store.UpdateAlertRuleSyncStatus(ctx, id, status, message, at); err != nil {
		if r.logs.allow("status:" + status) {
			slog.Warn("could not record an alert rule sync outcome",
				"ruleID", id, "status", status, "error", err)
		}
	}
}

// decodeObject turns a JSONB column into the renderer's map. An empty or null
// payload is an empty map, not an error: the store folds those into {} on the
// way in, and agent-missing genuinely has no params.
func decodeObject(field string, raw json.RawMessage) (map[string]any, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s is not a JSON object: %w", field, err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// decodeStringMap is decodeObject for the two columns whose values must all be
// strings. A non-string value NAMES ITS KEY rather than reporting "cannot
// unmarshal": the operator has to find it in a JSON blob they typed.
func decodeStringMap(field string, raw json.RawMessage) (map[string]string, error) {
	obj, err := decodeObject(field, raw)
	if err != nil {
		return nil, err
	}
	if len(obj) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(obj))
	// Sorted, so the same bad input always fails on the same key.
	for _, key := range slices.Sorted(maps.Keys(obj)) {
		s, ok := obj[key].(string)
		if !ok {
			return nil, fmt.Errorf("%s[%q] must be a string, got %T", field, key, obj[key])
		}
		out[key] = s
	}
	return out, nil
}

// jitter spreads d by +/-jitterFraction. math/rand is correct here and
// crypto/rand would not be: this is a scheduling decision, not a secret. Same
// helper, same reasoning, as the webhook dispatcher's.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	spread := 1 + jitterFraction*(2*mathrand.Float64()-1) //nolint:gosec // G404: reconcile spread, not a security decision
	return time.Duration(float64(d) * spread)
}

// logLimiter admits one log line per key per logRateLimit. Unlike kubectx's,
// the keyspace here is CLOSED by construction -- the keys are cause classes
// and sync statuses, both fixed sets -- so it needs no cap.
type logLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
	now  func() time.Time
}

func newLogLimiter(now func() time.Time) *logLimiter {
	return &logLimiter{last: make(map[string]time.Time), now: now}
}

func (l *logLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if at, ok := l.last[key]; ok && now.Sub(at) < logRateLimit {
		return false
	}
	l.last[key] = now
	return true
}
