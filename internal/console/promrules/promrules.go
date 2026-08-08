// Package promrules is the Console's PrometheusRule sync: it renders the
// alert_rules table into ONE PrometheusRule object and server-side-applies it
// into the Console's own namespace (M7 Decision 3).
//
// # What it is, and what it deliberately is not
//
// It is a CONVERGENCE loop, not an event pipeline. Prometheus evaluates alert
// rules; the Console only makes sure the cluster is holding the bytes the
// database says it should. Nothing here reads alert state, nothing here talks
// to Alertmanager, and nothing here decides that an alert fired.
//
// It is also the only part of the Console besides kubectx that talks to the
// apiserver, and its grant is deliberately the smallest one that can do the
// job: a namespaced Role over exactly one resource
// (monitoring.coreos.com/v1 prometheusrules), never a ClusterRole. The
// constructor takes a dynamic.Interface rather than building one, so the
// in-cluster config lives in cmd/console next to kubectx's -- internal/console
// must not import internal/controller, and cmd is where that copy already is.
//
// # Every replica runs this loop, on purpose (Decision 5)
//
// The scheduler serializes itself on a PostgreSQL advisory lock and this does
// not, and the difference is not an oversight. The scheduler FIRES SIDE
// EFFECTS: two replicas ticking together would run one check twice, and there
// is no way to undo the second run. This loop ASSERTS STATE: every replica
// renders the same bytes from the same rows and applies them with the same
// field manager, so N replicas racing produce exactly the object one replica
// would have produced. Last write wins, and every candidate write is identical.
//
// The cost of a lock here would be real -- a replica that cannot get the lock
// does not converge, so a wedged lock holder becomes a silent sync outage --
// and the benefit is a few redundant PATCHes a minute. The jittered interval is
// what keeps those PATCHes from arriving in lockstep.
//
// # Drift semantics: RECORD, THEN FIX
//
// A reconcile ALWAYS re-asserts our bytes. Drift is what we OBSERVED in the
// live object immediately BEFORE re-asserting, and it is recorded on the rules
// so an operator learns that somebody edited the CRD by hand -- not so the
// console can leave the hand edit in place. There is no mode in which this
// package sees drift and declines to fix it.
//
// That has one consequence worth stating plainly, because it looks like a bug
// in the status column and is not: a rule that reports sync_status=drift also
// carries a fresh last_synced_at. Both are true. The drift was observed, the
// apply then happened, and the very next reconcile will report the same rule
// as synced. A status of drift means "the cluster had diverged as of
// last_synced_at, and we corrected it", never "the cluster is diverged right
// now and we left it".
//
// # Failure is a status, never a crash
//
// The PrometheusRule CRD may not exist (the Prometheus Operator is not a
// dependency of this chart), the Role may not have been applied, the apiserver
// may be down. Every one of those is written back onto every enabled rule as
// sync_status=error with a message naming the CAUSE CLASS (crd-missing,
// forbidden, other), and the loop keeps its cadence. The rules themselves live
// in PostgreSQL, so a degraded sync costs an operator nothing but the
// alerting: the builder, the list and the API all keep working.
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
	// FieldManager is the server-side-apply field manager the console owns.
	// Every apply uses it AND force=true, which is correct precisely because
	// the object is ours end to end: force only ever takes fields back from
	// whoever edited them out of band, which is the drift this package exists
	// to correct.
	FieldManager = "kconmon-ng-console"

	// DefaultInterval backs up config validation. The reconciler is only ever
	// built from a validated config, so a non-positive interval here means a
	// hand-built caller -- repaired rather than trusted, because a zero would
	// be a hot apply loop against the apiserver.
	DefaultInterval = 60 * time.Second

	// jitterFraction is the +/-20% spread on the interval. Same constant and
	// same idiom as the webhook dispatcher's retry spread, for the same
	// reason: N replicas started by one rollout must not converge on the same
	// second forever.
	jitterFraction = 0.2

	// syncMessageMaxLen mirrors store's alertRuleSyncMessageMaxLen. The store
	// would reject a longer message and the reconciler would then log a write
	// failure instead of recording the outcome it just observed, so the bound
	// is applied HERE, where the message is built.
	syncMessageMaxLen = 1024
	truncationMarker  = "..."

	// logRateLimit bounds this package's own logging. A cluster with no CRD
	// produces one failing reconcile per interval forever; at one line a
	// minute per key that is a readable signal, at one line per reconcile it
	// is noise an operator learns to ignore.
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

// NewClient binds dyn to one namespace. The dynamic.Interface is built by the
// caller (cmd/console, from the in-cluster REST config) for kubectx's reason:
// this package must be constructible in a test with a fake client and no
// cluster anywhere.
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

// Apply server-side-applies obj. One call, no read-modify-write and no
// create-then-update fallback: SSA creates the object when it is absent and
// merges when it is present, and adding a fallback would only mean a second
// code path that a real apiserver never takes.
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

// ForeignRule is one PrometheusRule in the namespace that the console does NOT
// own (M7 Decision 4). It is listed read-only; adoption is an explicit import
// that copies the groups into builder rows and creates a NEW object, and this
// package never mutates a foreign object under any circumstance.
type ForeignRule struct {
	// Name is the object's name.
	Name string
	// Groups is how many entries spec.groups holds.
	Groups int
	// Rules is the total number of rule entries across all groups -- alerting
	// and recording alike, because a recording rule is still something an
	// import would have to carry.
	Rules int
	// ManagedBy is the value of app.kubernetes.io/managed-by, or "" when the
	// object carries no such label. Surfaced because "managed by some other
	// chart" and "managed by nobody" are different facts for an operator
	// deciding whether to import.
	ManagedBy string
	// Object is the raw object, handed straight to the API layer (Task 4).
	// Carried rather than projected because an import has to read the actual
	// groups, and a second projection here would be a shape to keep in sync.
	Object *unstructured.Unstructured
}

// ListForeign returns every PrometheusRule in the namespace that is not ours,
// sorted by name.
//
// The filter is CLIENT-SIDE on purpose. The server-side alternative
// (`app.kubernetes.io/managed-by!=kconmon-ng-console`) is subtly wrong-adjacent
// -- an inequality selector also matches objects with no such label, which is
// what we want, but it makes the definition of "foreign" live in a selector
// string instead of next to the constant it is checked against. The list is a
// namespace's worth of PrometheusRules, dozens at the very most, so there is
// nothing to optimise.
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

// countGroups counts spec.groups and the rule entries inside them. Every shape
// that is not what the CRD promises counts as zero rather than erroring: this
// is a read of somebody else's object, and refusing to list a malformed
// foreign rule would hide it from the operator who needs to see it most.
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

// Compare reports whether the live object diverges from the desired one on the
// fields the console RENDERS, and returns a compact diff when it does.
//
// Only the rendered fields are compared, and that scope is the whole design.
// A live object carries resourceVersion, uid, creationTimestamp, managedFields
// and whatever annotations the cluster's own tooling stapled on; none of that
// is ours, none of it is something an apply would change, and comparing it
// would report drift on every single reconcile forever. What IS compared:
// spec, our managed-by label, and our rule-ids annotation -- exactly the three
// things RenderBundle produces.
//
// It takes desired explicitly rather than reading it off a Reconciler field:
// the desired bundle is per-reconcile state, and hanging it off the loop would
// make this racy for no gain. Pure function, no I/O, no clock.
func Compare(desired, live *unstructured.Unstructured) (drift bool, diff string) {
	want := renderRelevantJSON(desired)
	got := renderRelevantJSON(live)
	if want == got {
		return false, ""
	}
	return true, lineDiff(want, got)
}

// renderRelevantJSON projects an object down to the fields we own and renders
// them as stable, indented JSON. encoding/json sorts map keys, so the same
// object always produces the same bytes -- which is what makes a byte
// comparison a legitimate drift test.
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

// lineDiff renders a compact unified-ish diff of two texts.
//
// It is NOT a minimal edit script and does not try to be. The common prefix
// and suffix are elided and the divergent middle is printed whole, which for
// the shape this actually sees -- one expression changed, one rule added,
// somebody's editor reindented a block -- reads exactly like a real diff at a
// fraction of the cost. An LCS would be O(n*m) per reconcile to produce a
// better rendering of a string that is then truncated to 1 KiB anyway.
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

// truncate bounds s at syncMessageMaxLen bytes INCLUDING the marker, cutting
// on a rune boundary. Same shape and same reason as kubectx's message bound:
// the column has a length CHECK, and a message the store rejects turns a
// recorded outcome into a logged write failure.
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

// Classify names the cause class of a failed API call.
//
// The NotFound case is the one that needs stating. A dynamic client builds its
// URL from the GVR without consulting discovery, so a request for a resource
// no apiserver serves comes back as a 404 -- the same status code a missing
// OBJECT produces. That ambiguity is harmless HERE and only here: Apply
// creates the object when it is absent, so an apply can never 404 on a missing
// object. A NotFound from an APPLY therefore means the RESOURCE is missing,
// which is the CRD. (Get is ambiguous, which is why Reconcile never classifies
// a failed Get -- it just skips the drift observation.)
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

// Store is the narrow store seam: the two methods this package calls, and no
// others. Same convention checks.Runner's Store follows -- a local interface
// naming exactly what is used, so a test substitutes a fake without a database
// and a reader can see the whole persistence surface in four lines.
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

	// kick is capacity 1, which IS the coalescing rule: a kick that arrives
	// while a reconcile is in flight queues exactly one more pass, and ten
	// kicks in the same second queue exactly one. Anything larger would let a
	// burst of CRUD writes schedule a burst of identical applies.
	kick chan struct{}

	// now is time.Now indirected so a test can assert the lastSyncedAt that
	// reaches the store without comparing against a live clock.
	now func() time.Time

	logs *logLimiter
}

// New builds a Reconciler. It never touches the network and never fails on
// anything an operator can misconfigure: the config layer already rejected a
// bad interval and a bad bundle name, so the only errors here are nil
// dependencies, which are programmer errors.
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

// Kick asks for a reconcile as soon as the loop is free. Non-blocking and
// coalescing: it is called from HTTP handlers (Task 4) after every alert-rule
// write, and a handler must never wait on a Kubernetes round trip.
func (r *Reconciler) Kick() {
	select {
	case r.kick <- struct{}{}:
	default:
		// A pass is already queued. It will read the same table this kick
		// would have, so dropping this one loses nothing.
	}
}

// ListForeign delegates to the client, so callers that hold the reconciler
// (Task 4's API layer, which also needs Kick) do not additionally have to be
// handed the client.
func (r *Reconciler) ListForeign(ctx context.Context) ([]ForeignRule, error) {
	return r.client.ListForeign(ctx)
}

// Namespace reports the namespace the bundle is applied into.
func (r *Reconciler) Namespace() string { return r.client.Namespace() }

// Run reconciles immediately, then on every jittered interval and on every
// kick, until ctx is cancelled. Spawned through cmd/console's `spawn` helper,
// whose wg.Wait blocks shutdown on this return.
//
// It reconciles FIRST and waits after, so a console that just started applies
// the operator's rules now rather than a minute from now.
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

// Reconcile is ONE pass: read the enabled rules, render, observe, apply,
// write the outcome back onto every rule. Exported so a test (and Task 4's
// synchronous POST /{id}/sync) can run exactly one.
//
// The returned error is the loop's log line. It is NOT the operator's report:
// the operator's report is the per-rule sync_status this method writes before
// returning, which is why every failure path writes statuses first and returns
// second.
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
			// It is marked, dropped from the bundle, and the pass continues --
			// which is also what makes a stored 'cert-expiry' rule (a kind the
			// store accepts and the renderer deliberately dropped) show up as
			// a named error on that one rule instead of a dead sync.
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

	// The apply succeeded, so lastSyncedAt moves -- including for a rule
	// reported as drift. See the package doc: drift is past tense here, the
	// correction already happened, and pretending nothing was applied would be
	// the dishonest option.
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

// renderable turns one stored row into the renderer's input and proves it
// renders. The proof is a single-rule bundle rather than a bare Render,
// because the things that make a row unappliable are spread across both: the
// EXPRESSION comes from Render, but the severity, the alert-name sanitisation,
// the reserved labels and the `for` duration are all checked by RenderBundle.
// Checking one rule at a time is what buys per-rule attribution.
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

// setStatus records one rule's outcome. A failed write is logged and swallowed
// on purpose: the reconcile itself either happened or did not, and failing the
// whole pass because the bookkeeping row would not update would turn a
// database hiccup into a sync outage.
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
