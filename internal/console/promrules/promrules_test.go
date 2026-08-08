package promrules

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/alerting"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

const (
	testNamespace  = "kconmon-ng"
	testBundleName = "kconmon-ng-console-rules"
)

// ---------------------------------------------------------------------------
// The fake dynamic client's quirks — PROBED, not assumed
//
// Everything below was established by running it (client-go v0.36.3), the way
// kubectx pinned the fake clientset's field-selector behaviour. Two of the
// three are load-bearing for how the rest of this file is written.
// ---------------------------------------------------------------------------

// TestFakeDynamicClientCannotApply is the quirk that shapes every other test
// in this file: k8s.io/client-go/dynamic/fake CANNOT server-side-apply an
// unstructured object AT ALL, in either direction.
//
//   - Applying an ABSENT object returns NotFound. testing.tracker.Apply does a
//     Get first and propagates its error, so the fake never creates on apply —
//     which is the entire point of SSA against a real apiserver.
//   - Applying over an EXISTING object fails with "unable to find api field in
//     struct Unstructured for the json field \"metadata\"". The tracker merges
//     via strategicpatch.StrategicMergePatch, which reflects over Go struct
//     tags; *unstructured.Unstructured has none, so the merge cannot run.
//     client-go's own comment points typed clients at ManagedFieldObjectTracker
//     for real field-manager support — the DYNAMIC fake has no constructor
//     that accepts one.
//
// So every test that needs an apply installs applyReactor below. This test
// exists to make that workaround auditable rather than mysterious, and to
// break loudly if a client-go bump ever fixes the fake — at which point the
// reactor can go.
func TestFakeDynamicClientCannotApply(t *testing.T) {
	c := newFakeDynamic(t) // deliberately WITHOUT applyReactor
	ri := c.Resource(GVR).Namespace(testNamespace)
	obj := bundleObject(t, "expr-a")

	_, err := ri.Apply(context.Background(), testBundleName, obj,
		metav1.ApplyOptions{FieldManager: FieldManager, Force: true})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("apply-when-absent error = %v, want NotFound (the documented fake quirk)", err)
	}

	if cerr := c.Tracker().Create(GVR, obj, testNamespace); cerr != nil {
		t.Fatalf("seed the tracker: %v", cerr)
	}
	_, err = ri.Apply(context.Background(), testBundleName, bundleObject(t, "expr-b"),
		metav1.ApplyOptions{FieldManager: FieldManager, Force: true})
	if err == nil {
		t.Fatal("apply-over-existing = nil error, want the strategic-merge reflection failure")
	}
	if !strings.Contains(err.Error(), "unable to find api field") {
		t.Fatalf("apply-over-existing error = %v, want the strategic-merge reflection failure", err)
	}
}

// TestFakeDynamicClientListIsUnfiltered pins the third probed fact: the fake
// DOES honour a label selector on List (including a negated one), and it
// returns objects in name order. ListForeign relies on neither — it filters
// client-side and sorts itself — and this test is what says that redundancy is
// a choice rather than a misunderstanding of the fake.
func TestFakeDynamicClientListIsUnfiltered(t *testing.T) {
	c := newFakeDynamic(t,
		foreignObject("theirs", map[string]any{"app.kubernetes.io/managed-by": "some-other-chart"}),
		foreignObject("nolabels", nil),
	)
	list, err := c.Resource(GVR).Namespace(testNamespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: "!app.kubernetes.io/managed-by",
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// A negated selector drops "theirs", which IS foreign — the exact reason
	// ListForeign does not use one.
	if len(list.Items) != 1 || list.Items[0].GetName() != "nolabels" {
		t.Fatalf("negated selector returned %d item(s), want just nolabels", len(list.Items))
	}
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(GVR.GroupVersion().WithKind(alerting.BundleKind), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(GVR.GroupVersion().WithKind(alerting.BundleKind+"List"), &unstructured.UnstructuredList{})
	return s
}

func newFakeDynamic(t *testing.T, objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	t.Helper()
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(testScheme(),
		map[schema.GroupVersionResource]string{GVR: alerting.BundleKind + "List"}, objects...)
}

// applyReactor emulates what a real apiserver does for a FORCED apply by a
// SINGLE field manager over an object nobody else owns: create it when absent,
// replace it when present. That is not general SSA — it is exactly the slice
// of SSA this package uses, and modelling more would be modelling something
// the console never asks for.
func applyReactor(c *dynamicfake.FakeDynamicClient) {
	tracker := c.Tracker()
	c.PrependReactor("patch", GVR.Resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		pa, ok := action.(k8stesting.PatchActionImpl)
		if !ok || pa.GetPatchType() != types.ApplyPatchType {
			return false, nil, nil
		}
		obj := &unstructured.Unstructured{Object: map[string]any{}}
		if err := json.Unmarshal(pa.GetPatch(), &obj.Object); err != nil {
			return true, nil, err
		}
		obj.SetName(pa.GetName())
		obj.SetNamespace(pa.GetNamespace())
		if _, err := tracker.Get(pa.GetResource(), pa.GetNamespace(), pa.GetName()); err != nil {
			if !apierrors.IsNotFound(err) {
				return true, nil, err
			}
			return true, obj, tracker.Create(pa.GetResource(), obj, pa.GetNamespace())
		}
		return true, obj, tracker.Update(pa.GetResource(), obj, pa.GetNamespace())
	})
}

func newAppliableFake(t *testing.T, objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	t.Helper()
	c := newFakeDynamic(t, objects...)
	applyReactor(c)
	return c
}

func bundleObject(t *testing.T, expr string) *unstructured.Unstructured {
	t.Helper()
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": alerting.BundleAPIVersion,
		"kind":       alerting.BundleKind,
		"metadata": map[string]any{
			"name":        testBundleName,
			"namespace":   testNamespace,
			"labels":      map[string]any{alerting.ManagedByLabel: alerting.ManagedByValue},
			"annotations": map[string]any{alerting.RuleIDsAnnotation: "id-1"},
		},
		"spec": map[string]any{"groups": []any{map[string]any{
			"name":  alerting.GroupName,
			"rules": []any{map[string]any{"alert": "A", "expr": expr}},
		}}},
	}}
}

func foreignObject(name string, labels map[string]any) *unstructured.Unstructured {
	meta := map[string]any{"name": name, "namespace": testNamespace}
	if labels != nil {
		meta["labels"] = labels
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": alerting.BundleAPIVersion,
		"kind":       alerting.BundleKind,
		"metadata":   meta,
		"spec": map[string]any{"groups": []any{
			map[string]any{"name": "g1", "rules": []any{
				map[string]any{"alert": "X", "expr": "up == 0"},
				map[string]any{"record": "y", "expr": "up"},
			}},
			map[string]any{"name": "g2", "rules": []any{
				map[string]any{"alert": "Z", "expr": "up == 1"},
			}},
		}},
	}}
}

// row builds one enabled alert_rules row.
func row(id, name string) store.AlertRule {
	return store.AlertRule{
		ID:       id,
		Name:     name,
		Kind:     store.AlertRuleKindPairLoss,
		Params:   json.RawMessage(`{"protocol":"udp","thresholdPercent":5}`),
		Severity: store.AlertSeverityWarning,
		ForNs:    int64(5 * time.Minute),
		Enabled:  true,
	}
}

// ---------------------------------------------------------------------------
// Fake store seam
// ---------------------------------------------------------------------------

type statusWrite struct {
	ID      string
	Status  string
	Message string
	At      *time.Time
}

type fakeStore struct {
	mu       sync.Mutex
	rules    []store.AlertRule
	listErr  error
	writeErr error
	writes   []statusWrite
	lists    int
}

func (f *fakeStore) ListAlertRules(_ context.Context, enabledOnly bool) ([]store.AlertRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if f.listErr != nil {
		return nil, f.listErr
	}
	if !enabledOnly {
		panic("the reconciler must only ever ask for enabled rules")
	}
	out := make([]store.AlertRule, 0, len(f.rules))
	for _, r := range f.rules {
		if r.Enabled {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeStore) UpdateAlertRuleSyncStatus(
	_ context.Context, id, status, message string, lastSyncedAt *time.Time,
) (store.AlertRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, statusWrite{ID: id, Status: status, Message: message, At: lastSyncedAt})
	if f.writeErr != nil {
		return store.AlertRule{}, f.writeErr
	}
	return store.AlertRule{ID: id, SyncStatus: status, SyncMessage: message, LastSyncedAt: lastSyncedAt}, nil
}

func (f *fakeStore) snapshot() []statusWrite {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]statusWrite(nil), f.writes...)
}

func (f *fakeStore) listCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists
}

// writeFor returns the LAST status written for id.
func writeFor(writes []statusWrite, id string) (statusWrite, bool) {
	for i := len(writes) - 1; i >= 0; i-- {
		if writes[i].ID == id {
			return writes[i], true
		}
	}
	return statusWrite{}, false
}

var frozen = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

func newReconciler(t *testing.T, dyn dynamic.Interface, st Store) *Reconciler {
	t.Helper()
	client, err := NewClient(dyn, testNamespace)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	r, err := New(Deps{
		Client:     client,
		Store:      st,
		Renderer:   alerting.NewRenderer("kconmon_ng"),
		BundleName: testBundleName,
		Interval:   time.Hour,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.now = func() time.Time { return frozen }
	return r
}

// ---------------------------------------------------------------------------
// Client construction
// ---------------------------------------------------------------------------

func TestNewClientRejectsNothingToTalkTo(t *testing.T) {
	if _, err := NewClient(nil, testNamespace); err == nil {
		t.Error("NewClient(nil, ns) = nil error, want a rejection")
	}
	if _, err := NewClient(newFakeDynamic(t), "   "); err == nil {
		t.Error("NewClient(dyn, blank) = nil error, want a rejection")
	}
	c, err := NewClient(newFakeDynamic(t), testNamespace)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.Namespace() != testNamespace {
		t.Errorf("Namespace() = %q, want %q", c.Namespace(), testNamespace)
	}
}

// ---------------------------------------------------------------------------
// Apply
// ---------------------------------------------------------------------------

func TestApplyCreatesThenUpdates(t *testing.T) {
	c := newAppliableFake(t)
	client, err := NewClient(c, testNamespace)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err = client.Apply(ctx, bundleObject(t, "expr-a")); err != nil {
		t.Fatalf("first apply (create): %v", err)
	}
	live, err := client.Get(ctx, testBundleName)
	if err != nil {
		t.Fatalf("get after create: %v", err)
	}
	if got := firstExpr(t, live); got != "expr-a" {
		t.Fatalf("expr after create = %q, want expr-a", got)
	}

	if _, err = client.Apply(ctx, bundleObject(t, "expr-b")); err != nil {
		t.Fatalf("second apply (update): %v", err)
	}
	live, err = client.Get(ctx, testBundleName)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got := firstExpr(t, live); got != "expr-b" {
		t.Fatalf("expr after update = %q, want expr-b", got)
	}
}

// TestApplyUsesOurFieldManagerAndForces pins the two apply options Decision 5
// depends on: without the field manager the object has no owner, and without
// force a hand edit by another manager would make every apply conflict instead
// of correcting the drift.
func TestApplyUsesOurFieldManagerAndForces(t *testing.T) {
	c := newAppliableFake(t)
	var seen k8stesting.PatchActionImpl
	c.PrependReactor("patch", GVR.Resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		if pa, ok := action.(k8stesting.PatchActionImpl); ok {
			seen = pa
		}
		return false, nil, nil
	})
	client, _ := NewClient(c, testNamespace)
	if _, err := client.Apply(context.Background(), bundleObject(t, "e")); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if seen.GetPatchType() != types.ApplyPatchType {
		t.Errorf("patch type = %v, want %v", seen.GetPatchType(), types.ApplyPatchType)
	}
	if seen.PatchOptions.FieldManager != FieldManager {
		t.Errorf("field manager = %q, want %q", seen.PatchOptions.FieldManager, FieldManager)
	}
	if seen.PatchOptions.Force == nil || !*seen.PatchOptions.Force {
		t.Errorf("force = %v, want true", seen.PatchOptions.Force)
	}
	if seen.GetNamespace() != testNamespace {
		t.Errorf("namespace = %q, want %q — the client must be namespace-scoped", seen.GetNamespace(), testNamespace)
	}
}

func firstExpr(t *testing.T, obj *unstructured.Unstructured) string {
	t.Helper()
	groups, found, err := unstructured.NestedSlice(obj.Object, "spec", "groups")
	if err != nil || !found || len(groups) == 0 {
		t.Fatalf("spec.groups missing: found=%v err=%v", found, err)
	}
	rules, _ := groups[0].(map[string]any)["rules"].([]any)
	if len(rules) == 0 {
		t.Fatal("group has no rules")
	}
	expr, _ := rules[0].(map[string]any)["expr"].(string)
	return expr
}

// ---------------------------------------------------------------------------
// Reconcile — the happy path
// ---------------------------------------------------------------------------

func TestReconcileAppliesAndMarksSynced(t *testing.T) {
	c := newAppliableFake(t)
	st := &fakeStore{rules: []store.AlertRule{row("id-1", "Pair loss"), row("id-2", "Zone latency")}}
	r := newReconciler(t, c, st)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	live, err := c.Resource(GVR).Namespace(testNamespace).Get(context.Background(), testBundleName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the bundle was not created: %v", err)
	}
	if got := live.GetLabels()[alerting.ManagedByLabel]; got != alerting.ManagedByValue {
		t.Errorf("managed-by label = %q, want %q", got, alerting.ManagedByValue)
	}
	if got := live.GetAnnotations()[alerting.RuleIDsAnnotation]; got != "id-1,id-2" {
		t.Errorf("rule-ids annotation = %q, want id-1,id-2", got)
	}

	writes := st.snapshot()
	if len(writes) != 2 {
		t.Fatalf("status writes = %d, want 2 (one per enabled rule)", len(writes))
	}
	for _, id := range []string{"id-1", "id-2"} {
		w, ok := writeFor(writes, id)
		if !ok {
			t.Fatalf("no status written for %s", id)
		}
		if w.Status != store.AlertSyncStatusSynced {
			t.Errorf("%s status = %q, want synced", id, w.Status)
		}
		if w.Message != "" {
			t.Errorf("%s message = %q, want empty on success", id, w.Message)
		}
		if w.At == nil || !w.At.Equal(frozen) {
			t.Errorf("%s lastSyncedAt = %v, want %v", id, w.At, frozen)
		}
	}
}

// TestReconcileWithNoEnabledRulesStillAsserts: an operator who disabled
// everything must end up with an EMPTY bundle in the cluster, not with
// yesterday's rules still evaluating.
func TestReconcileWithNoEnabledRulesStillAsserts(t *testing.T) {
	c := newAppliableFake(t)
	st := &fakeStore{}
	r := newReconciler(t, c, st)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	live, err := c.Resource(GVR).Namespace(testNamespace).Get(context.Background(), testBundleName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the bundle was not asserted: %v", err)
	}
	groups, _, _ := unstructured.NestedSlice(live.Object, "spec", "groups")
	if len(groups) != 0 {
		t.Errorf("spec.groups = %d entries, want 0", len(groups))
	}
	if n := len(st.snapshot()); n != 0 {
		t.Errorf("status writes = %d, want 0 (there is no rule to write one onto)", n)
	}
}

// ---------------------------------------------------------------------------
// Drift
// ---------------------------------------------------------------------------

// TestReconcileRecordsDriftThenFixesIt is the semantic the package doc spells
// out: the hand edit is REPORTED and CORRECTED in the same pass, and the next
// pass reports synced.
func TestReconcileRecordsDriftThenFixesIt(t *testing.T) {
	tampered := bundleObject(t, "somebody edited this by hand")
	c := newAppliableFake(t, tampered)
	st := &fakeStore{rules: []store.AlertRule{row("id-1", "Pair loss")}}
	r := newReconciler(t, c, st)
	ctx := context.Background()

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	w, _ := writeFor(st.snapshot(), "id-1")
	if w.Status != store.AlertSyncStatusDrift {
		t.Fatalf("status = %q, want drift", w.Status)
	}
	if !strings.Contains(w.Message, "somebody edited this by hand") {
		t.Errorf("drift message does not carry the live text:\n%s", w.Message)
	}
	if !strings.Contains(w.Message, "--- rendered (console)") {
		t.Errorf("drift message is not a diff:\n%s", w.Message)
	}
	// Record THEN fix: lastSyncedAt moves, because the apply happened.
	if w.At == nil || !w.At.Equal(frozen) {
		t.Errorf("drift lastSyncedAt = %v, want %v — the apply DID happen", w.At, frozen)
	}
	live, err := c.Resource(GVR).Namespace(testNamespace).Get(ctx, testBundleName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := firstExpr(t, live); strings.Contains(got, "by hand") {
		t.Errorf("the hand edit survived the reconcile: expr = %q", got)
	}

	// Second pass over the corrected object: synced, no diff.
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	w2, _ := writeFor(st.snapshot(), "id-1")
	if w2.Status != store.AlertSyncStatusSynced {
		t.Errorf("second pass status = %q, want synced", w2.Status)
	}
}

func TestCompareIgnoresFieldsWeDoNotOwn(t *testing.T) {
	desired := bundleObject(t, "expr-a")
	live := bundleObject(t, "expr-a")
	live.SetResourceVersion("4711")
	live.SetUID("2b0b7c1a-0000-0000-0000-000000000000")
	live.SetGeneration(9)
	labels := live.GetLabels()
	labels["some-other-operator.io/instance"] = "x"
	live.SetLabels(labels)
	anns := live.GetAnnotations()
	anns["kubectl.kubernetes.io/last-applied-configuration"] = "{...}"
	live.SetAnnotations(anns)

	drift, diff := Compare(desired, live)
	if drift {
		t.Errorf("Compare reported drift on fields the console does not render:\n%s", diff)
	}
}

func TestCompareDetectsEachRenderedField(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*unstructured.Unstructured)
	}{
		{"spec changed", func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(u.Object, []any{}, "spec", "groups")
		}},
		{"spec removed", func(u *unstructured.Unstructured) {
			unstructured.RemoveNestedField(u.Object, "spec")
		}},
		{"managed-by label stripped", func(u *unstructured.Unstructured) {
			u.SetLabels(map[string]string{})
		}},
		{"rule-ids annotation stripped", func(u *unstructured.Unstructured) {
			u.SetAnnotations(map[string]string{})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desired := bundleObject(t, "expr-a")
			live := bundleObject(t, "expr-a")
			tc.mutate(live)
			drift, diff := Compare(desired, live)
			if !drift {
				t.Fatal("Compare reported no drift")
			}
			if diff == "" {
				t.Fatal("drift reported with an empty diff")
			}
		})
	}
}

func TestCompareAgainstAnAbsentObject(t *testing.T) {
	drift, diff := Compare(bundleObject(t, "expr-a"), nil)
	if !drift || !strings.Contains(diff, "<absent>") {
		t.Fatalf("Compare(desired, nil) = (%v, %q), want drift naming the absence", drift, diff)
	}
}

// TestDiffFitsTheStoreColumn: a bundle that diverges completely still produces
// a message the store will accept. The bound is applied where the message is
// built, so a 1 MiB divergence cannot turn a recorded drift into a rejected
// write.
func TestDiffFitsTheStoreColumn(t *testing.T) {
	desired := bundleObject(t, strings.Repeat("a", 40_000))
	live := bundleObject(t, strings.Repeat("b", 40_000))
	_, diff := Compare(desired, live)
	msg := truncate(diff)
	if len(msg) > syncMessageMaxLen {
		t.Fatalf("truncated message = %d bytes, want <= %d", len(msg), syncMessageMaxLen)
	}
	if !strings.HasSuffix(msg, truncationMarker) {
		t.Errorf("a cut message must read as cut, got tail %q", msg[max(0, len(msg)-10):])
	}
}

// ---------------------------------------------------------------------------
// Failure classification
// ---------------------------------------------------------------------------

func TestClassify(t *testing.T) {
	gr := schema.GroupResource{Group: GVR.Group, Resource: GVR.Resource}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"resource not found", apierrors.NewNotFound(gr, ""), CauseCRDMissing},
		{"forbidden", apierrors.NewForbidden(gr, testBundleName, errors.New("nope")), CauseForbidden},
		{"unauthorized", apierrors.NewUnauthorized("no token"), CauseForbidden},
		{"conflict", apierrors.NewConflict(gr, testBundleName, errors.New("x")), CauseOther},
		{"plain error", errors.New("connection refused"), CauseOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.err); got != tc.want {
				t.Errorf("Classify(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// failApply makes every apply return err.
func failApply(c *dynamicfake.FakeDynamicClient, err error) {
	c.PrependReactor("patch", GVR.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, err
	})
}

func TestReconcileMarksEveryRuleWithTheCauseClass(t *testing.T) {
	gr := schema.GroupResource{Group: GVR.Group, Resource: GVR.Resource}
	for _, tc := range []struct {
		name      string
		err       error
		wantCause string
		wantHint  string
	}{
		{"CRD absent", apierrors.NewNotFound(gr, ""), CauseCRDMissing, "Prometheus Operator CRDs"},
		{"RBAC denied", apierrors.NewForbidden(gr, testBundleName, errors.New("denied")), CauseForbidden, "Role and RoleBinding"},
		{"anything else", apierrors.NewInternalError(errors.New("boom")), CauseOther, "rejected the apply"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newAppliableFake(t)
			failApply(c, tc.err)
			st := &fakeStore{rules: []store.AlertRule{row("id-1", "One"), row("id-2", "Two")}}
			r := newReconciler(t, c, st)

			err := r.Reconcile(context.Background())
			if err == nil {
				t.Fatal("Reconcile returned nil, want the apply failure")
			}
			writes := st.snapshot()
			if len(writes) != 2 {
				t.Fatalf("status writes = %d, want 2 (EVERY enabled rule is marked)", len(writes))
			}
			for _, w := range writes {
				if w.Status != store.AlertSyncStatusError {
					t.Errorf("%s status = %q, want error", w.ID, w.Status)
				}
				if !strings.HasPrefix(w.Message, tc.wantCause+":") {
					t.Errorf("%s message must START with the cause class %q, got: %s", w.ID, tc.wantCause, w.Message)
				}
				if !strings.Contains(w.Message, tc.wantHint) {
					t.Errorf("%s message must tell the operator what to fix (%q), got: %s", w.ID, tc.wantHint, w.Message)
				}
				if w.At != nil {
					t.Errorf("%s lastSyncedAt = %v, want nil — nothing was applied", w.ID, w.At)
				}
				if len(w.Message) > syncMessageMaxLen {
					t.Errorf("%s message = %d bytes, over the store's %d bound", w.ID, len(w.Message), syncMessageMaxLen)
				}
			}
		})
	}
}

// TestReconcileSurvivesAFailedRead: a GET that fails for a reason other than
// NotFound costs the drift observation and nothing else — the apply still
// happens and the rules are still marked synced.
func TestReconcileSurvivesAFailedRead(t *testing.T) {
	c := newAppliableFake(t)
	c.PrependReactor("get", GVR.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver is down")
	})
	st := &fakeStore{rules: []store.AlertRule{row("id-1", "One")}}
	r := newReconciler(t, c, st)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	w, _ := writeFor(st.snapshot(), "id-1")
	if w.Status != store.AlertSyncStatusSynced {
		t.Errorf("status = %q, want synced", w.Status)
	}
}

func TestReconcileFailsWhenTheRulesCannotBeRead(t *testing.T) {
	st := &fakeStore{listErr: errors.New("pool closed")}
	r := newReconciler(t, newAppliableFake(t), st)
	if err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile = nil error, want the list failure")
	}
	if n := len(st.snapshot()); n != 0 {
		t.Errorf("status writes = %d, want 0 — there is no id list to mark", n)
	}
}

// TestReconcileSwallowsAFailedStatusWrite: the bookkeeping is best-effort. A
// database hiccup on the write-back must not become a sync outage.
func TestReconcileSwallowsAFailedStatusWrite(t *testing.T) {
	c := newAppliableFake(t)
	st := &fakeStore{rules: []store.AlertRule{row("id-1", "One")}, writeErr: errors.New("write conflict")}
	r := newReconciler(t, c, st)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile = %v, want nil — the apply succeeded", err)
	}
	if _, err := c.Resource(GVR).Namespace(testNamespace).Get(
		context.Background(), testBundleName, metav1.GetOptions{}); err != nil {
		t.Fatalf("the bundle was not applied: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Per-rule render failures
// ---------------------------------------------------------------------------

// TestReconcileIsolatesAnUnrenderableRule: the store's kind vocabulary is
// WIDER than the renderer's (cert-expiry is a legal column value that the
// renderer deliberately dropped, and a params blob can be edited into
// nonsense). One such row is marked and skipped; the rest still sync.
func TestReconcileIsolatesAnUnrenderableRule(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*store.AlertRule)
		wantMsg string
	}{
		{"kind the renderer dropped", func(r *store.AlertRule) {
			r.Kind = store.AlertRuleKindCertExpiry
			r.Params = json.RawMessage(`{}`)
		}, "unknown kind"},
		{"params out of range", func(r *store.AlertRule) {
			r.Params = json.RawMessage(`{"protocol":"udp","thresholdPercent":900}`)
		}, "between 0 and 100"},
		{"name that cannot be an alert name", func(r *store.AlertRule) {
			r.Name = "5xx"
		}, "does not start with a letter"},
		{"reserved label", func(r *store.AlertRule) {
			r.Labels = json.RawMessage(`{"severity":"critical"}`)
		}, "reserved"},
		{"non-string label value", func(r *store.AlertRule) {
			r.Labels = json.RawMessage(`{"team":7}`)
		}, `labels["team"] must be a string`},
		{"params not an object", func(r *store.AlertRule) {
			r.Params = json.RawMessage(`[1,2,3]`)
		}, "params is not a JSON object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := row("id-bad", "Bad rule")
			tc.mutate(&bad)
			c := newAppliableFake(t)
			st := &fakeStore{rules: []store.AlertRule{bad, row("id-ok", "Good rule")}}
			r := newReconciler(t, c, st)

			if err := r.Reconcile(context.Background()); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			w, ok := writeFor(st.snapshot(), "id-bad")
			if !ok {
				t.Fatal("the unrenderable rule was not marked")
			}
			if w.Status != store.AlertSyncStatusError {
				t.Errorf("status = %q, want error", w.Status)
			}
			if !strings.Contains(w.Message, tc.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", w.Message, tc.wantMsg)
			}
			if w.At != nil {
				t.Error("an unrendered rule must not get a lastSyncedAt bump")
			}
			good, _ := writeFor(st.snapshot(), "id-ok")
			if good.Status != store.AlertSyncStatusSynced {
				t.Errorf("the healthy rule status = %q, want synced", good.Status)
			}
			live, err := c.Resource(GVR).Namespace(testNamespace).Get(
				context.Background(), testBundleName, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("the bundle was not applied: %v", err)
			}
			if got := live.GetAnnotations()[alerting.RuleIDsAnnotation]; got != "id-ok" {
				t.Errorf("rule-ids annotation = %q, want id-ok only", got)
			}
		})
	}
}

// TestReconcileMarksEveryRuleOnABundleLevelFailure: an alert-name collision is
// a relationship BETWEEN rules, so it cannot be blamed on one of them.
func TestReconcileMarksEveryRuleOnABundleLevelFailure(t *testing.T) {
	c := newAppliableFake(t)
	st := &fakeStore{rules: []store.AlertRule{row("id-1", "pair loss"), row("id-2", "Pair-Loss")}}
	r := newReconciler(t, c, st)

	if err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile = nil error, want the collision")
	}
	writes := st.snapshot()
	if len(writes) != 2 {
		t.Fatalf("status writes = %d, want 2", len(writes))
	}
	for _, w := range writes {
		if w.Status != store.AlertSyncStatusError {
			t.Errorf("%s status = %q, want error", w.ID, w.Status)
		}
		if !strings.Contains(w.Message, "collision") {
			t.Errorf("%s message = %q, want it to name the collision", w.ID, w.Message)
		}
	}
}

// ---------------------------------------------------------------------------
// Foreign rules
// ---------------------------------------------------------------------------

func TestListForeignExcludesOurs(t *testing.T) {
	ours := bundleObject(t, "expr-a")
	c := newAppliableFake(t, ours,
		foreignObject("zzz-last", map[string]any{"app.kubernetes.io/managed-by": "some-other-chart"}),
		foreignObject("aaa-first", nil),
	)
	client, _ := NewClient(c, testNamespace)
	got, err := client.ListForeign(context.Background())
	if err != nil {
		t.Fatalf("ListForeign: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListForeign returned %d, want 2 (ours must be excluded)", len(got))
	}
	if got[0].Name != "aaa-first" || got[1].Name != "zzz-last" {
		t.Errorf("ListForeign is not sorted by name: %q, %q", got[0].Name, got[1].Name)
	}
	if got[0].ManagedBy != "" {
		t.Errorf("unlabelled object ManagedBy = %q, want empty", got[0].ManagedBy)
	}
	if got[1].ManagedBy != "some-other-chart" {
		t.Errorf("ManagedBy = %q, want some-other-chart — a rule owned by ANOTHER tool is still foreign",
			got[1].ManagedBy)
	}
	for _, f := range got {
		if f.Groups != 2 || f.Rules != 3 {
			t.Errorf("%s counts = %d groups / %d rules, want 2/3", f.Name, f.Groups, f.Rules)
		}
		if f.Object == nil || f.Object.GetName() != f.Name {
			t.Errorf("%s: the raw object must travel with the summary", f.Name)
		}
	}
}

func TestListForeignSurvivesAMalformedObject(t *testing.T) {
	broken := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": alerting.BundleAPIVersion,
		"kind":       alerting.BundleKind,
		"metadata":   map[string]any{"name": "broken", "namespace": testNamespace},
		"spec":       map[string]any{"groups": "not-a-list"},
	}}
	c := newAppliableFake(t, broken)
	client, _ := NewClient(c, testNamespace)
	got, err := client.ListForeign(context.Background())
	if err != nil {
		t.Fatalf("ListForeign: %v", err)
	}
	if len(got) != 1 || got[0].Groups != 0 || got[0].Rules != 0 {
		t.Fatalf("ListForeign = %+v, want the object listed with zero counts", got)
	}
}

func TestListForeignReportsAnAbsentCRD(t *testing.T) {
	c := newAppliableFake(t)
	c.PrependReactor("list", GVR.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: GVR.Group, Resource: GVR.Resource}, "")
	})
	client, _ := NewClient(c, testNamespace)
	_, err := client.ListForeign(context.Background())
	if err == nil {
		t.Fatal("ListForeign = nil error, want the NotFound")
	}
	if Classify(err) != CauseCRDMissing {
		t.Errorf("Classify(list error) = %q, want %q", Classify(err), CauseCRDMissing)
	}
}

// The reconciler delegates, so callers holding it for Kick() need nothing else.
func TestReconcilerDelegatesListForeign(t *testing.T) {
	c := newAppliableFake(t, foreignObject("theirs", nil))
	r := newReconciler(t, c, &fakeStore{})
	got, err := r.ListForeign(context.Background())
	if err != nil || len(got) != 1 || got[0].Name != "theirs" {
		t.Fatalf("Reconciler.ListForeign = (%+v, %v)", got, err)
	}
	if r.Namespace() != testNamespace {
		t.Errorf("Namespace() = %q, want %q", r.Namespace(), testNamespace)
	}
}

// ---------------------------------------------------------------------------
// The loop
// ---------------------------------------------------------------------------

func TestKickCoalesces(t *testing.T) {
	r := newReconciler(t, newAppliableFake(t), &fakeStore{})
	for range 10 {
		r.Kick() // must never block, even with nobody reading
	}
	if got := len(r.kick); got != 1 {
		t.Fatalf("queued kicks = %d, want exactly 1 (ten kicks queue one pass)", got)
	}
	<-r.kick
	if got := len(r.kick); got != 0 {
		t.Fatalf("queued kicks after a drain = %d, want 0", got)
	}
}

// TestRunReconcilesImmediatelyThenOnKick pins both halves of the loop's
// contract: the first pass does not wait for the interval, and a kick brings
// the next one forward from an interval that would otherwise never elapse
// inside a test.
func TestRunReconcilesImmediatelyThenOnKick(t *testing.T) {
	c := newAppliableFake(t)
	st := &fakeStore{rules: []store.AlertRule{row("id-1", "One")}}
	r := newReconciler(t, c, st) // interval = 1h

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()

	waitFor(t, func() bool { return st.listCount() >= 1 }, "the immediate first reconcile")
	r.Kick()
	waitFor(t, func() bool { return st.listCount() >= 2 }, "the kicked reconcile")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return on context cancellation")
	}
}

// TestRunSurvivesAPermanentFailure: a cluster with no CRD must not turn the
// loop into a crash or a tight retry — it keeps its cadence and keeps marking.
func TestRunSurvivesAPermanentFailure(t *testing.T) {
	c := newAppliableFake(t)
	failApply(c, apierrors.NewNotFound(schema.GroupResource{Group: GVR.Group, Resource: GVR.Resource}, ""))
	st := &fakeStore{rules: []store.AlertRule{row("id-1", "One")}}
	r := newReconciler(t, c, st)
	r.interval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r.Run(ctx)

	if st.listCount() < 2 {
		t.Fatalf("reconciles = %d, want the loop to have kept its cadence", st.listCount())
	}
	w, ok := writeFor(st.snapshot(), "id-1")
	if !ok || w.Status != store.AlertSyncStatusError {
		t.Fatalf("status = %+v, want a recorded error", w)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ---------------------------------------------------------------------------
// Construction and small pieces
// ---------------------------------------------------------------------------

func TestNewRejectsMissingDependencies(t *testing.T) {
	client, _ := NewClient(newFakeDynamic(t), testNamespace)
	for _, tc := range []struct {
		name string
		deps Deps
	}{
		{"no client", Deps{Store: &fakeStore{}, BundleName: testBundleName}},
		{"no store", Deps{Client: client, BundleName: testBundleName}},
		{"no bundle name", Deps{Client: client, Store: &fakeStore{}, BundleName: "  "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.deps); err == nil {
				t.Error("New = nil error, want a rejection")
			}
		})
	}
}

func TestNewRepairsANonPositiveInterval(t *testing.T) {
	client, _ := NewClient(newFakeDynamic(t), testNamespace)
	r, err := New(Deps{Client: client, Store: &fakeStore{}, BundleName: testBundleName, Interval: 0})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if r.interval != DefaultInterval {
		t.Errorf("interval = %v, want %v", r.interval, DefaultInterval)
	}
}

func TestJitterStaysWithinTheBand(t *testing.T) {
	const base = time.Minute
	lo, hi := time.Duration(float64(base)*(1-jitterFraction)), time.Duration(float64(base)*(1+jitterFraction))
	for range 1000 {
		got := jitter(base)
		if got < lo || got > hi {
			t.Fatalf("jitter(%v) = %v, outside [%v, %v]", base, got, lo, hi)
		}
	}
	if jitter(0) != 0 {
		t.Error("jitter(0) must be 0")
	}
}

func TestLogLimiterAdmitsOncePerWindow(t *testing.T) {
	now := frozen
	l := newLogLimiter(func() time.Time { return now })
	if !l.allow("k") {
		t.Fatal("first call must be admitted")
	}
	if l.allow("k") {
		t.Fatal("a second call inside the window must be suppressed")
	}
	if !l.allow("other") {
		t.Fatal("a different key has its own window")
	}
	now = now.Add(logRateLimit + time.Second)
	if !l.allow("k") {
		t.Fatal("the window must reopen")
	}
}

// The seam is the store's own contract: if *store.DB ever stops satisfying it,
// this file stops compiling, which is the point.
func TestStoreSeamIsSatisfiedByTheRealStore(t *testing.T) {
	var _ Store = (*store.DB)(nil)
}
