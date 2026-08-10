package alerting

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// marshalBundle serializes the object the way the dynamic client will: as JSON; that module IS in
// the build graph, but only as an INDIRECT requirement.
func marshalBundle(t *testing.T, obj *unstructured.Unstructured) string {
	t.Helper()
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(obj.Object); err != nil {
		t.Fatalf("json encode error = %v", err)
	}
	return buf.String()
}

func bundleFixture() []Rule {
	return []Rule{
		// Deliberately NOT in output order: the bundle sorts by lower(Name).
		{
			ID:       "b2c3d4e5-0000-4000-8000-000000000002",
			Name:     "zone a latency",
			Kind:     KindZoneLatency,
			Params:   map[string]any{"protocol": "udp", "quantile": 0.95, "thresholdMs": 25.0},
			Severity: "warning",
			ForNS:    5 * 60 * 1_000_000_000,
			Enabled:  true,
		},
		{
			ID:          "a1b2c3d4-0000-4000-8000-000000000001",
			Name:        "udp loss high",
			Kind:        KindPairLoss,
			Params:      map[string]any{"protocol": "udp", "thresholdPercent": 50.0},
			Severity:    "critical",
			ForNS:       90 * 1_000_000_000,
			Labels:      map[string]string{"team": "network"},
			Annotations: map[string]string{"summary": "High UDP packet loss between nodes"},
			Enabled:     true,
		},
		{
			ID:       "c3d4e5f6-0000-4000-8000-000000000003",
			Name:     "disabled rule",
			Kind:     KindRaw,
			Params:   map[string]any{"expr": "vector(1)"},
			Severity: "info",
			Enabled:  false,
		},
		{
			ID:       "d4e5f6a7-0000-4000-8000-000000000004",
			Name:     "agents missing",
			Kind:     KindAgentMissing,
			Severity: "info",
			Enabled:  true,
		},
	}
}

// bundleGolden is the whole object, byte for byte.
const bundleGolden = `{
  "apiVersion": "monitoring.coreos.com/v1",
  "kind": "PrometheusRule",
  "metadata": {
    "annotations": {
      "kconmon-ng.io/rule-ids": "a1b2c3d4-0000-4000-8000-000000000001,b2c3d4e5-0000-4000-8000-000000000002,d4e5f6a7-0000-4000-8000-000000000004"
    },
    "labels": {
      "app.kubernetes.io/managed-by": "kconmon-ng-console"
    },
    "name": "kconmon-ng-console-rules",
    "namespace": "kconmon-ng"
  },
  "spec": {
    "groups": [
      {
        "name": "kconmon-ng-console",
        "rules": [
          {
            "alert": "AgentsMissing",
            "expr": "kconmon_ng_controller_registered_agents < kconmon_ng_controller_expected_agents",
            "labels": {
              "kconmon_ng_rule_id": "d4e5f6a7-0000-4000-8000-000000000004",
              "severity": "info"
            }
          },
          {
            "alert": "UdpLossHigh",
            "annotations": {
              "summary": "High UDP packet loss between nodes"
            },
            "expr": "kconmon_ng_udp_packet_loss_ratio * 100 > 50",
            "for": "90s",
            "labels": {
              "kconmon_ng_rule_id": "a1b2c3d4-0000-4000-8000-000000000001",
              "severity": "critical",
              "team": "network"
            }
          },
          {
            "alert": "ZoneALatency",
            "expr": "histogram_quantile(0.95, sum by (le, source_zone, destination_zone) (rate(kconmon_ng_udp_rtt_seconds_bucket[5m]))) * 1000 > 25",
            "for": "5m",
            "labels": {
              "kconmon_ng_rule_id": "b2c3d4e5-0000-4000-8000-000000000002",
              "severity": "warning"
            }
          }
        ]
      }
    ]
  }
}
`

func TestRenderBundleGolden(t *testing.T) {
	obj, err := defaultRenderer.RenderBundle(bundleFixture(), "kconmon-ng", "kconmon-ng-console-rules")
	if err != nil {
		t.Fatalf("RenderBundle() error = %v", err)
	}
	if got := marshalBundle(t, obj); got != bundleGolden {
		t.Errorf("bundle mismatch\n--- got ---\n%s\n--- want ---\n%s", got, bundleGolden)
	}
}

// The expr values inside the object are single-line strings.
func TestRenderBundleGoldenExpressions(t *testing.T) {
	obj, err := defaultRenderer.RenderBundle(bundleFixture(), "kconmon-ng", "kconmon-ng-console-rules")
	if err != nil {
		t.Fatalf("RenderBundle() error = %v", err)
	}
	groups, _, err := unstructuredSlice(obj, "spec", "groups")
	if err != nil {
		t.Fatalf("spec.groups: %v", err)
	}
	rules, _ := groups[0].(map[string]any)["rules"].([]any)

	want := []string{
		`kconmon_ng_controller_registered_agents < kconmon_ng_controller_expected_agents`,
		`kconmon_ng_udp_packet_loss_ratio * 100 > 50`,
		`histogram_quantile(0.95, sum by (le, source_zone, destination_zone) ` +
			`(rate(kconmon_ng_udp_rtt_seconds_bucket[5m]))) * 1000 > 25`,
	}
	if len(rules) != len(want) {
		t.Fatalf("got %d rules, want %d", len(rules), len(want))
	}
	for i, w := range want {
		entry, _ := rules[i].(map[string]any)
		if got := entry["expr"]; got != w {
			t.Errorf("rule %d expr:\n got: %v\nwant: %s", i, got, w)
		}
		if s, ok := entry["expr"].(string); ok && strings.Contains(s, "\n") {
			t.Errorf("rule %d expr contains a newline", i)
		}
	}
}

// Determinism pin: two independent renders of the same input must produce
// byte-identical output. Map iteration must never leak into the object.
func TestRenderBundleIsByteIdenticalAcrossCalls(t *testing.T) {
	var first string
	for i := range 25 {
		obj, err := defaultRenderer.RenderBundle(bundleFixture(), "kconmon-ng", "kconmon-ng-console-rules")
		if err != nil {
			t.Fatalf("RenderBundle() error = %v", err)
		}
		out := marshalBundle(t, obj)
		if i == 0 {
			first = out
			continue
		}
		if out != first {
			t.Fatalf("bundle not byte-identical on call %d\n--- got ---\n%s\n--- first ---\n%s", i, out, first)
		}
	}
}

// Input order must not change output order.
func TestRenderBundleIsOrderInsensitive(t *testing.T) {
	forward := bundleFixture()
	reversed := make([]Rule, 0, len(forward))
	for i := len(forward) - 1; i >= 0; i-- {
		reversed = append(reversed, forward[i])
	}

	a, err := defaultRenderer.RenderBundle(forward, "kconmon-ng", "kconmon-ng-console-rules")
	if err != nil {
		t.Fatalf("RenderBundle(forward) error = %v", err)
	}
	b, err := defaultRenderer.RenderBundle(reversed, "kconmon-ng", "kconmon-ng-console-rules")
	if err != nil {
		t.Fatalf("RenderBundle(reversed) error = %v", err)
	}
	if !reflect.DeepEqual(a.Object, b.Object) {
		t.Error("bundle depends on input slice order")
	}
}

func TestRenderBundleObjectShape(t *testing.T) {
	obj, err := defaultRenderer.RenderBundle(bundleFixture(), "kconmon-ng", "kconmon-ng-console-rules")
	if err != nil {
		t.Fatalf("RenderBundle() error = %v", err)
	}
	if got := obj.GetAPIVersion(); got != "monitoring.coreos.com/v1" {
		t.Errorf("apiVersion = %q", got)
	}
	if got := obj.GetKind(); got != "PrometheusRule" {
		t.Errorf("kind = %q", got)
	}
	if got := obj.GetName(); got != "kconmon-ng-console-rules" {
		t.Errorf("name = %q", got)
	}
	if got := obj.GetNamespace(); got != "kconmon-ng" {
		t.Errorf("namespace = %q", got)
	}
	// Ownership label, and NOTHING else at object level.
	wantLabels := map[string]string{"app.kubernetes.io/managed-by": "kconmon-ng-console"}
	if got := obj.GetLabels(); !reflect.DeepEqual(got, wantLabels) {
		t.Errorf("object labels = %v, want %v", got, wantLabels)
	}
	wantAnn := map[string]string{
		"kconmon-ng.io/rule-ids": "a1b2c3d4-0000-4000-8000-000000000001," +
			"b2c3d4e5-0000-4000-8000-000000000002," +
			"d4e5f6a7-0000-4000-8000-000000000004",
	}
	if got := obj.GetAnnotations(); !reflect.DeepEqual(got, wantAnn) {
		t.Errorf("object annotations = %v, want %v", got, wantAnn)
	}

	groups, found, err := unstructuredSlice(obj, "spec", "groups")
	if err != nil || !found {
		t.Fatalf("spec.groups: found=%v err=%v", found, err)
	}
	if len(groups) != 1 {
		t.Fatalf("want exactly ONE group (single-bundle strategy), got %d", len(groups))
	}
	group, ok := groups[0].(map[string]any)
	if !ok {
		t.Fatalf("group is %T", groups[0])
	}
	if group["name"] != "kconmon-ng-console" {
		t.Errorf("group name = %v", group["name"])
	}
	rules, ok := group["rules"].([]any)
	if !ok {
		t.Fatalf("group rules is %T", group["rules"])
	}
	if len(rules) != 3 {
		t.Fatalf("want 3 enabled rules, got %d (disabled rules must be dropped)", len(rules))
	}
}

// The unstructured content must be JSON-safe: DeepCopy panics on any other
// type, and the dynamic client marshals it as JSON.
func TestRenderBundleContentIsDeepCopyable(t *testing.T) {
	obj, err := defaultRenderer.RenderBundle(bundleFixture(), "kconmon-ng", "kconmon-ng-console-rules")
	if err != nil {
		t.Fatalf("RenderBundle() error = %v", err)
	}
	cp := obj.DeepCopy()
	if !reflect.DeepEqual(cp.Object, obj.Object) {
		t.Error("DeepCopy() diverged from the original")
	}
}

func TestRenderBundleEmptyAndAllDisabled(t *testing.T) {
	for name, rules := range map[string][]Rule{
		"nil slice":    nil,
		"empty slice":  {},
		"all disabled": {{ID: "x", Name: "n", Kind: KindAgentMissing, Severity: "info", Enabled: false}},
	} {
		obj, err := defaultRenderer.RenderBundle(rules, "kconmon-ng", "kconmon-ng-console-rules")
		if err != nil {
			t.Fatalf("%s: RenderBundle() error = %v", name, err)
		}
		groups, found, err := unstructuredSlice(obj, "spec", "groups")
		if err != nil || !found {
			t.Fatalf("%s: spec.groups: found=%v err=%v", name, found, err)
		}
		if len(groups) != 0 {
			t.Errorf("%s: want an empty groups list, got %d", name, len(groups))
		}
		if got := obj.GetAnnotations()["kconmon-ng.io/rule-ids"]; got != "" {
			t.Errorf("%s: rule-ids = %q, want empty", name, got)
		}
	}
}

func TestRenderBundleErrors(t *testing.T) {
	ok := Rule{ID: "id-1", Name: "ok rule", Kind: KindAgentMissing, Severity: "info", Enabled: true}

	tests := []struct {
		name      string
		rules     []Rule
		namespace string
		bundle    string
		wantSub   []string
	}{
		{
			name: "empty namespace", rules: []Rule{ok}, namespace: "", bundle: "b",
			wantSub: []string{"namespace"},
		},
		{
			name: "empty bundle name", rules: []Rule{ok}, namespace: "ns", bundle: "",
			wantSub: []string{"bundleName"},
		},
		{
			name: "missing id",
			rules: []Rule{{
				Name: "no id", Kind: KindAgentMissing, Severity: "info", Enabled: true,
			}},
			namespace: "ns", bundle: "b", wantSub: []string{"no id", "ID"},
		},
		{
			name: "blank name",
			rules: []Rule{{
				ID: "id-1", Name: "  ", Kind: KindAgentMissing, Severity: "info", Enabled: true,
			}},
			namespace: "ns", bundle: "b", wantSub: []string{"Name"},
		},
		{
			name: "bad severity",
			rules: []Rule{{
				ID: "id-1", Name: "n", Kind: KindAgentMissing, Severity: "page", Enabled: true,
			}},
			namespace: "ns", bundle: "b", wantSub: []string{"severity", "page", "warning"},
		},
		{
			name: "unsanitizable name",
			rules: []Rule{{
				ID: "id-1", Name: "5xx", Kind: KindAgentMissing, Severity: "info", Enabled: true,
			}},
			namespace: "ns", bundle: "b", wantSub: []string{"5xx"},
		},
		{
			name: "sanitize collision",
			rules: []Rule{
				{ID: "id-1", Name: "udp loss", Kind: KindAgentMissing, Severity: "info", Enabled: true},
				{ID: "id-2", Name: "udp-loss", Kind: KindAgentMissing, Severity: "info", Enabled: true},
			},
			namespace: "ns", bundle: "b", wantSub: []string{"collision", "UdpLoss"},
		},
		{
			name: "reserved severity label",
			rules: []Rule{{
				ID: "id-1", Name: "n", Kind: KindAgentMissing, Severity: "info", Enabled: true,
				Labels: map[string]string{"severity": "critical"},
			}},
			namespace: "ns", bundle: "b", wantSub: []string{"severity", "reserved"},
		},
		{
			name: "reserved rule-id label",
			rules: []Rule{{
				ID: "id-1", Name: "n", Kind: KindAgentMissing, Severity: "info", Enabled: true,
				Labels: map[string]string{"kconmon_ng_rule_id": "other"},
			}},
			namespace: "ns", bundle: "b", wantSub: []string{"kconmon_ng_rule_id", "reserved"},
		},
		{
			name: "invalid label name",
			rules: []Rule{{
				ID: "id-1", Name: "n", Kind: KindAgentMissing, Severity: "info", Enabled: true,
				Labels: map[string]string{"my-team": "network"},
			}},
			namespace: "ns", bundle: "b", wantSub: []string{"my-team", "label name"},
		},
		{
			name: "invalid annotation name",
			rules: []Rule{{
				ID: "id-1", Name: "n", Kind: KindAgentMissing, Severity: "info", Enabled: true,
				Annotations: map[string]string{"run book": "x"},
			}},
			namespace: "ns", bundle: "b", wantSub: []string{"run book", "annotation name"},
		},
		{
			name: "negative for_ns",
			rules: []Rule{{
				ID: "id-1", Name: "n", Kind: KindAgentMissing, Severity: "info",
				ForNS: -1, Enabled: true,
			}},
			namespace: "ns", bundle: "b", wantSub: []string{"for"},
		},
		{
			name: "render failure names the rule",
			rules: []Rule{{
				ID: "id-1", Name: "broken", Kind: KindPairLoss, Severity: "info", Enabled: true,
				Params: map[string]any{"protocol": "quic", "thresholdPercent": 5.0},
			}},
			namespace: "ns", bundle: "b", wantSub: []string{"broken", "protocol"},
		},
		{
			name: "a DISABLED broken rule must not break the bundle",
			rules: []Rule{
				{ID: "id-1", Name: "broken", Kind: "nope", Severity: "info", Enabled: false},
				ok,
			},
			namespace: "ns", bundle: "b", wantSub: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := defaultRenderer.RenderBundle(tt.rules, tt.namespace, tt.bundle)
			if tt.wantSub == nil {
				if err != nil {
					t.Fatalf("RenderBundle() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("RenderBundle() = nil error, want error")
			}
			for _, sub := range tt.wantSub {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error %q does not mention %q", err.Error(), sub)
				}
			}
		})
	}
}

// for is OMITTED when ForNS is zero rather than written as "0s" — a live
// object will not carry the key either, so drift stays quiet.
func TestRenderBundleOmitsZeroFor(t *testing.T) {
	obj, err := defaultRenderer.RenderBundle([]Rule{{
		ID: "id-1", Name: "n", Kind: KindAgentMissing, Severity: "info", Enabled: true,
	}}, "ns", "b")
	if err != nil {
		t.Fatalf("RenderBundle() error = %v", err)
	}
	out := marshalBundle(t, obj)
	if strings.Contains(out, `"for"`) {
		t.Errorf("zero ForNS must not emit a for key:\n%s", out)
	}
}

func unstructuredSlice(obj *unstructured.Unstructured, fields ...string) ([]any, bool, error) {
	return unstructured.NestedSlice(obj.Object, fields...)
}
