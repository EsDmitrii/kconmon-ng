// Package promrules is the Console's PrometheusRule sync; Prometheus evaluates alert rules.
package promrules

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// DeleteExact removes obj only while it is still the object the caller read: same UID, same
// resourceVersion. An object that is already gone is success.
func (c *Client) DeleteExact(ctx context.Context, obj *unstructured.Unstructured) error {
	uid, rv := obj.GetUID(), obj.GetResourceVersion()
	err := c.ri.Delete(ctx, obj.GetName(), metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv},
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// ownedByConsole reports whether obj carries the label the console's render stamps on its bundle;
// ListForeign draws the same line.
func ownedByConsole(obj *unstructured.Unstructured) bool {
	return obj.GetLabels()[alerting.ManagedByLabel] == alerting.ManagedByValue
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
	// AlertRules is how many of those entries are alerting rules, the ones an import copies; the
	// recording ones it skips.
	AlertRules int
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
		if ownedByConsole(item) {
			continue
		}
		groups, rules := countGroups(item)
		out = append(out, ForeignRule{
			Name:       item.GetName(),
			Groups:     groups,
			Rules:      rules,
			AlertRules: countAlertingRules(item),
			ManagedBy:  item.GetLabels()[alerting.ManagedByLabel],
			Object:     item.DeepCopy(),
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

// countAlertingRules counts the entries an import would try to copy: a non-empty alert and no
// record, the same split httpapi's adoptRuleEntry makes.
func countAlertingRules(obj *unstructured.Unstructured) int {
	raw, found, err := unstructured.NestedSlice(obj.Object, "spec", "groups")
	if !found || err != nil {
		return 0
	}
	n := 0
	for _, g := range raw {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		entries, _ := gm["rules"].([]any)
		for _, e := range entries {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if record, _ := em["record"].(string); strings.TrimSpace(record) != "" {
				continue
			}
			if alert, _ := em["alert"].(string); strings.TrimSpace(alert) != "" {
				n++
			}
		}
	}
	return n
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

// isContentRejection reports whether err is the cluster refusing the object's CONTENT: a 400 or 422,
// which is also how an admission webhook's denial arrives. A timeout, 429, 5xx or an unreachable
// webhook says nothing about any rule, so it must not send one to quarantine.
func isContentRejection(err error) bool {
	return apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) ||
		strings.Contains(err.Error(), "denied the request")
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
	// UpdateAlertRuleSyncStatusIfUnchanged writes an outcome only onto the version the pass rendered,
	// so a rule edited meanwhile keeps its 'unsynced'.
	UpdateAlertRuleSyncStatusIfUnchanged(
		ctx context.Context, id string, updatedAt time.Time, status, message string, lastSyncedAt *time.Time,
	) (bool, error)
}

var _ Store = (*store.DB)(nil)

// LockKey is the pg_try_advisory_lock key this loop serializes itself on across console replicas.
// It is deliberately NOT the scheduler's key: a rule Kick must not queue behind a schedule tick.
const LockKey int64 = 2111970503

// Locker is the cross-replica mutual-exclusion seam, satisfied by *store.DB. See
// store.DB.WithAdvisoryLock for the (false, nil) = "someone else has it" contract.
type Locker interface {
	WithAdvisoryLock(ctx context.Context, key int64, fn func(context.Context) error) (bool, error)
}

var _ Locker = (*store.DB)(nil)

// Deps is the Reconciler's construction payload.
type Deps struct {
	Client   *Client
	Store    Store
	Renderer alerting.Renderer
	// Lock makes the loop single-writer across replicas. Nil runs every pass unlocked, which is
	// what a single replica does anyway.
	Lock Locker
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
	lock       Locker
	bundleName string
	interval   time.Duration

	// kick is capacity 1, which IS the coalescing rule; anything larger would let a burst of CRUD
	// writes schedule a burst of identical applies.
	kick chan struct{}

	// now is time.Now indirected so a test can assert the lastSyncedAt that
	// reaches the store without comparing against a live clock.
	now func() time.Time

	/* lastApplied maps rule id -> FINGERPRINT OF THE RENDERED ENTRY in the last bundle the cluster
	   ACCEPTED, and it is the quarantine's whole basis: on a content rejection, the rules that are
	   absent from it OR whose rendered bytes have changed since are the ones that can have caused
	   it. nil until the first successful apply, which is why the quarantine path checks for it.

	   The fingerprint, and not just the id, is what makes an EDIT quarantinable. While this was an
	   id set, retrying "the last applied set" after an operator broke the expression of a rule that
	   was already deployed produced an identical object -- the id had not changed -- so nothing was
	   ever quarantined, every rule was stamped with the generic API-rejected message naming none of
	   them, and the bundle froze exactly the way the quarantine exists to prevent. Fingerprints come
	   off the rendered object, so the live bundle can seed them as precisely as an apply can.

	   Reconcile serializes every pass behind an advisory lock (or runs single-threaded when there is
	   none), so this needs no lock of its own. It is per-process and deliberately not persisted: a
	   restarted replica simply has no fallback until its first successful apply, which is the same
	   state as a fresh install. */
	lastApplied map[string]string

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
		lock:       d.Lock,
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

// Reconcile is ONE pass, taken only by the replica that holds the advisory lock; the others do
// nothing, which is not an error. An unset Locker runs the pass directly.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	if r.lock == nil {
		return r.reconcileLocked(ctx)
	}

	locked, err := r.lock.WithAdvisoryLock(ctx, LockKey, r.reconcileLocked)
	if err != nil {
		return err
	}
	if !locked {
		slog.Debug("another console replica holds the alert rule lock, skipping this pass",
			"bundle", r.bundleName)
	}
	return nil
}

// reconcileLocked is the pass itself: read the enabled rules, render, observe, apply, write the
// outcome back onto every rule.
func (r *Reconciler) reconcileLocked(ctx context.Context) error {
	rows, err := r.store.ListAlertRules(ctx, true)
	if err != nil {
		// Nothing to write a status onto: the rules could not be read, so
		// there is no list of ids to mark. The loop logs and retries.
		return fmt.Errorf("list enabled alert rules: %w", err)
	}

	// Read the live bundle before deciding anything: its entries carry their rule ids
	// (alerting.RuleIDLabel), so it is the last rule set the cluster accepted, whichever process
	// applied it. Seeding lastApplied from it on every pass lets the alert-name tie-break and the
	// quarantine survive a restart and see what the other replica applied since.
	live, gerr := r.client.Get(ctx, r.bundleName)
	if gerr == nil && ownedByConsole(live) {
		if seeded := ruleFingerprints(live); len(seeded) > 0 {
			if r.lastApplied == nil {
				slog.Info("seeded the alert-rule quarantine baseline from the live bundle",
					"bundle", r.bundleName, "rules", len(seeded))
			}
			r.lastApplied = seeded
		}
	}

	candidates := make([]renderedRow, 0, len(rows))
	versions := make(map[string]time.Time, len(rows))
	for i := range rows {
		versions[rows[i].ID] = rows[i].UpdatedAt
		rule, rerr := r.renderable(&rows[i])
		if rerr != nil {
			// One unrenderable row must not cost the other rules their sync.
			r.setStatus(ctx, rows[i].ID, versions, store.AlertSyncStatusError, truncate("render: "+rerr.Error()), nil)
			continue
		}
		candidates = append(candidates, renderedRow{rule: rule, row: &rows[i]})
	}
	candidates = r.dropAlertNameCollisions(ctx, candidates, versions)
	rules := make([]alerting.Rule, 0, len(candidates))
	ids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		rules = append(rules, c.rule)
		ids = append(ids, c.row.ID)
	}

	// An empty rule set renders `groups: []`, which the prometheus-operator admission webhook
	// rejects outright, so the apply would fail forever and the last deleted rule would keep
	// evaluating. Owning no rules means owning no object; the next non-empty pass recreates it.
	if len(rules) == 0 {
		if derr := r.deleteIfOwned(ctx, live, gerr); derr != nil {
			return fmt.Errorf("delete %s %s/%s: %w",
				alerting.BundleKind, r.client.Namespace(), r.bundleName, derr)
		}
		return nil
	}

	desired, err := r.renderer.RenderBundle(rules, r.client.Namespace(), r.bundleName)
	if err != nil {
		// Alert name collisions are resolved above, so nothing here is attributable to one rule:
		// every surviving rule is unappliable and every one of them says so.
		msg := truncate("render bundle: " + err.Error())
		for _, id := range ids {
			r.setStatus(ctx, id, versions, store.AlertSyncStatusError, msg, nil)
		}
		return fmt.Errorf("render bundle: %w", err)
	}

	drift, diff := false, ""
	switch {
	case gerr == nil:
		if !ownedByConsole(live) {
			ferr := r.foreignBundleError(live)
			msg := truncate(CauseOther + ": " + ferr.Error())
			for _, id := range ids {
				r.setStatus(ctx, id, versions, store.AlertSyncStatusError, msg, nil)
			}
			return ferr
		}
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
		/* A content rejection (the admission webhook refusing a raw rule's unparseable PromQL, which
		   nothing validates on the way in) is one rule's fault. Failing the whole object would freeze
		   the alert set: new rules and edits would never deploy, and a disabled or deleted rule would
		   go on firing. So whatever was added or changed since the last accepted set is quarantined
		   and the rest re-applied. CRD-missing and forbidden refuse the write itself, no subset would
		   apply, so those keep failing every rule. */
		if cause == CauseOther && isContentRejection(aerr) && r.lastApplied != nil {
			if retried, qerr := r.retryWithLastApplied(ctx, rules, ids, versions, ruleFingerprints(desired), aerr); retried {
				return qerr
			}
		}
		msg := causeMessage(cause, r.client.Namespace(), aerr)
		for _, id := range ids {
			r.setStatus(ctx, id, versions, store.AlertSyncStatusError, msg, nil)
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
		r.setStatus(ctx, id, versions, status, message, &now)
	}
	// The CONTENT that reached the cluster, remembered so a later content rejection has something to
	// fall back to; see the quarantine path above.
	r.lastApplied = ruleFingerprints(desired)
	return nil
}

// renderedRow is one enabled row that renders on its own.
type renderedRow struct {
	rule alerting.Rule
	row  *store.AlertRule
}

/*
dropAlertNameCollisions keeps one rule per Prometheus alert name: 'pair-loss' and 'PairLoss' both
sanitize to PairLoss, and a bundle holding both is refused as a whole. The store refuses a new clash
at write; this is the backstop for rows an older version let in and for writes that raced past that
check. The rule deployed under the name keeps it, else the least recently updated one, so the rule
that was created, renamed or re-enabled into the clash is the one refused, and every other rule still
reaches the cluster.
*/
func (r *Reconciler) dropAlertNameCollisions(ctx context.Context, rows []renderedRow, versions map[string]time.Time) []renderedRow {
	alerts := make(map[string]string, len(rows))
	for _, c := range rows {
		alerts[c.row.ID], _ = alerting.SanitizeAlertName(c.row.Name) // renderable already proved it
	}
	order := slices.Clone(rows)
	slices.SortStableFunc(order, func(a, b renderedRow) int {
		_, aDeployed := r.lastApplied[a.row.ID]
		_, bDeployed := r.lastApplied[b.row.ID]
		switch {
		case aDeployed != bDeployed:
			if aDeployed {
				return -1
			}
			return 1
		case !a.row.UpdatedAt.Equal(b.row.UpdatedAt):
			return a.row.UpdatedAt.Compare(b.row.UpdatedAt)
		case !a.row.CreatedAt.Equal(b.row.CreatedAt):
			return a.row.CreatedAt.Compare(b.row.CreatedAt)
		}
		return strings.Compare(a.row.ID, b.row.ID)
	})
	holder := make(map[string]string, len(rows)) // alert name -> rule name holding it
	refused := make(map[string]bool)
	for _, c := range order {
		alert := alerts[c.row.ID]
		if prev, taken := holder[alert]; taken {
			refused[c.row.ID] = true
			r.setStatus(ctx, c.row.ID, versions, store.AlertSyncStatusError, truncate(fmt.Sprintf(
				"alert name collision: %q and %q both sanitize to %q; rename this rule", prev, c.row.Name, alert)), nil)
			continue
		}
		holder[alert] = c.row.Name
	}
	if len(refused) == 0 {
		return rows
	}
	return slices.DeleteFunc(slices.Clone(rows), func(c renderedRow) bool { return refused[c.row.ID] })
}

/*
ruleFingerprints maps rule id -> hash of that rule's RENDERED entry in a bundle object.

alerting's renderer stamps RuleIDLabel on every entry it emits, so an object the CLUSTER holds can be
read back rule by rule: for the live bundle that yields, by construction, the exact content of the
last apply that succeeded, including one made by a previous process, which is what the quarantine
needs to tell an unchanged rule from an edited one. reconcileLocked reads the live object anyway.

Hashing the whole entry (expr, for, labels, annotations) rather than the id alone is the point: an
id set cannot see an edit, and an edit is the ordinary way a bundle gets a bad expression.

An object without the labels (hand-edited, or written by an older build) yields nothing for those
entries, and the quarantine simply treats those rules as suspects — offering them to the cluster one
at a time, which is the safe direction to be wrong in.
*/
func ruleFingerprints(obj *unstructured.Unstructured) map[string]string {
	if obj == nil {
		return nil
	}
	groups, found, err := unstructured.NestedSlice(obj.Object, "spec", "groups")
	if err != nil || !found {
		return nil
	}
	out := make(map[string]string)
	for _, g := range groups {
		group, ok := g.(map[string]any)
		if !ok {
			continue
		}
		entries, ok := group["rules"].([]any)
		if !ok {
			continue
		}
		for _, e := range entries {
			entry, ok := e.(map[string]any)
			if !ok {
				continue
			}
			labels, _ := entry["labels"].(map[string]any)
			id, _ := labels[alerting.RuleIDLabel].(string)
			if id == "" {
				continue
			}
			encoded, merr := json.Marshal(entry)
			if merr != nil {
				continue
			}
			sum := sha256.Sum256(encoded)
			out[id] = hex.EncodeToString(sum[:])
		}
	}
	return out
}

/*
retryWithLastApplied re-applies only the rules that were in the last bundle the cluster accepted,
quarantining everything added or changed since.

It reports whether it actually retried; false means the caller should fall through to marking every
rule (there was nothing to fall back to, or the fallback set is what is already deployed).

The message on a quarantined rule says the cluster refused it and carries the API's own words, which
for an unparseable expression is the parse error itself — the sentence the operator needs.
*/
func (r *Reconciler) retryWithLastApplied(
	ctx context.Context, rules []alerting.Rule, ids []string, versions map[string]time.Time,
	desiredFP map[string]string, cause error,
) (retried bool, err error) {
	keep := make([]alerting.Rule, 0, len(ids))
	keepIDs := make([]string, 0, len(ids))
	var quarantined []string
	for i, id := range ids {
		// Unchanged content, not merely a familiar id: an edited rule keeps its id, and a broken edit
		// has to be a suspect.
		applied, known := r.lastApplied[id]
		if known && desiredFP[id] == applied {
			keep = append(keep, rules[i])
			keepIDs = append(keepIDs, id)
			continue
		}
		quarantined = append(quarantined, id)
	}
	// Nothing new to blame: the last good set is the whole set, so the rejection is not about a
	// recent change and a retry would send the same object.
	if len(quarantined) == 0 {
		return false, nil
	}

	msg := truncate("this rule was refused by the cluster and is not deployed; the rest of the " +
		"bundle was re-applied without it: " + cause.Error())
	for _, id := range quarantined {
		r.setStatus(ctx, id, versions, store.AlertSyncStatusError, msg, nil)
	}

	// An empty fallback set still goes through the probe below, which then offers each suspect on
	// its own; the bundle goes only when every suspect was offered and refused.
	if len(keep) > 0 {
		desired, rerr := r.renderer.RenderBundle(keep, r.client.Namespace(), r.bundleName)
		if rerr != nil {
			return true, fmt.Errorf("render bundle after quarantine: %w", rerr)
		}
		if _, aerr := r.client.Apply(ctx, desired); aerr != nil {
			// The fallback set does not apply either, so the quarantine guessed wrong; every rule says so.
			m := causeMessage(Classify(aerr), r.client.Namespace(), aerr)
			for _, id := range keepIDs {
				r.setStatus(ctx, id, versions, store.AlertSyncStatusError, m, nil)
			}
			return true, fmt.Errorf("apply %s %s/%s after quarantine: %w",
				alerting.BundleKind, r.client.Namespace(), r.bundleName, aerr)
		}
	}

	/* Offer each suspect on its own on top of the set that just applied, so a rule stays quarantined
	   only when the cluster refused that rule, and a rule written after an unrelated broken one still
	   deploys. There is normally one suspect, the rule just edited; quarantineProbeLimit bounds a
	   pathological pass, and what it defers is logged. */
	accepted := keep
	acceptedIDs := keepIDs
	stillQuarantined := make([]string, 0, len(quarantined))
	probes := 0
	inconclusive := 0
	for _, id := range quarantined {
		if probes >= quarantineProbeLimit {
			stillQuarantined = append(stillQuarantined, id)
			continue
		}
		rule, ok := ruleByID(rules, ids, id)
		if !ok {
			stillQuarantined = append(stillQuarantined, id)
			continue
		}
		probes++
		candidate := append(append([]alerting.Rule{}, accepted...), rule)
		obj, cerr := r.renderer.RenderBundle(candidate, r.client.Namespace(), r.bundleName)
		if cerr != nil {
			stillQuarantined = append(stillQuarantined, id)
			continue
		}
		if _, aerr := r.client.Apply(ctx, obj); aerr != nil {
			// Refused; a non-content failure (timeout, 5xx) counts as inconclusive and blocks the
			// delete below.
			stillQuarantined = append(stillQuarantined, id)
			if !isContentRejection(aerr) {
				inconclusive++
			}
			// Put the last good object back before the next suspect. A refused apply leaves the
			// live object as it was, so with nothing accepted there is nothing to restore.
			if len(accepted) > 0 {
				if good, gerr := r.renderer.RenderBundle(accepted, r.client.Namespace(), r.bundleName); gerr == nil {
					_, _ = r.client.Apply(ctx, good)
				}
			}
			continue
		}
		accepted = candidate
		acceptedIDs = append(acceptedIDs, id)
	}
	if dropped := len(quarantined) - probes; dropped > 0 {
		slog.Warn("more rules were refused in one pass than the quarantine probes; the rest keep their "+
			"quarantine until the next pass",
			"bundle", r.bundleName, "probed", probes, "deferred", dropped)
	}

	slog.Warn("the cluster refused the alert bundle; re-applied it without the rules it objects to",
		"bundle", r.bundleName, "quarantined", len(stillQuarantined), "applied", len(acceptedIDs), "error", cause)

	now := r.now()
	for _, id := range acceptedIDs {
		r.setStatus(ctx, id, versions, store.AlertSyncStatusSynced, "", &now)
	}
	// lastApplied moves only when the cluster took something; a pass that accepted nothing keeps
	// the baseline it started from.
	if len(acceptedIDs) > 0 {
		r.lastApplied = make(map[string]string, len(acceptedIDs))
		for _, id := range acceptedIDs {
			r.lastApplied[id] = desiredFP[id]
		}
	}

	/* Own no object only when every suspect was offered and refused on content: a bundle left in
	   place would keep firing rules the console reports as disabled. A pass that deferred a probe
	   (the budget, or a rule that vanished mid-pass) or saw a transient failure proved nothing and
	   changes nothing. */
	if len(acceptedIDs) == 0 && probes == len(quarantined) && inconclusive == 0 && len(quarantined) > 0 {
		if derr := r.deleteOwnedBundle(ctx); derr != nil {
			return true, fmt.Errorf("delete %s %s/%s after quarantine: %w",
				alerting.BundleKind, r.client.Namespace(), r.bundleName, derr)
		}
		r.lastApplied = nil
	}
	return true, nil
}

/*
deleteOwnedBundle deletes the bundle only when it is the console's own. The chart's Role scopes
delete to the bundle name, but that name is plain config and can collide with another object (the
chart refuses a collision with its own PrometheusRule at render), so ownership is checked here.
*/
func (r *Reconciler) deleteOwnedBundle(ctx context.Context) error {
	live, err := r.client.Get(ctx, r.bundleName)
	return r.deleteIfOwned(ctx, live, err)
}

// deleteIfOwned is deleteOwnedBundle over a read the caller already made; readErr is that read's error.
func (r *Reconciler) deleteIfOwned(ctx context.Context, live *unstructured.Unstructured, readErr error) error {
	switch {
	case apierrors.IsNotFound(readErr):
		return nil
	case readErr != nil:
		return fmt.Errorf("read it before deleting: %w", readErr)
	case !ownedByConsole(live):
		return r.foreignBundleError(live)
	}
	return r.client.DeleteExact(ctx, live)
}

func (r *Reconciler) foreignBundleError(live *unstructured.Unstructured) error {
	return fmt.Errorf("%s %s/%s exists and is not the console's (%s=%q), so the console leaves it alone; "+
		"set console.alerting.bundleName to a name no other %s in the namespace uses",
		alerting.BundleKind, r.client.Namespace(), r.bundleName,
		alerting.ManagedByLabel, live.GetLabels()[alerting.ManagedByLabel], alerting.BundleKind)
}

// quarantineProbeLimit bounds how many refused-rule candidates one pass re-offers to the cluster;
// each is an apply, and a pass holds the reconciler's advisory lock while it runs.
const quarantineProbeLimit = 8

// ruleByID finds the rendered rule that goes with an id; ids[i] names rules[i] by construction.
func ruleByID(rules []alerting.Rule, ids []string, id string) (alerting.Rule, bool) {
	for i := range ids {
		if ids[i] == id {
			return rules[i], true
		}
	}
	return alerting.Rule{}, false
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

// setStatus records one rule's outcome against the version of the row the pass read (versions[id]);
// a rule edited or deleted since is left for the next pass. A failed write is logged and swallowed
// on purpose.
func (r *Reconciler) setStatus(ctx context.Context, id string, versions map[string]time.Time, status, message string, at *time.Time) {
	if _, err := r.store.UpdateAlertRuleSyncStatusIfUnchanged(ctx, id, versions[id], status, message, at); err != nil {
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
