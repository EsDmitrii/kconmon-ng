package metrics //nolint:revive // var-naming: "metrics" is a valid internal package name, not a stdlib conflict

import (
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestNewPrometheusMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics("kconmon_ng", reg)

	if m == nil {
		t.Fatal("expected non-nil metrics")
	}

	m.TCPResults.WithLabelValues("src", "dst", "zone-a", "zone-b", "success").Inc()
	m.UDPResults.WithLabelValues("src", "dst", "zone-a", "zone-b", "success").Inc()
	m.ICMPResults.WithLabelValues("src", "dst", "zone-a", "zone-b", "fail").Inc()
	m.DNSResults.WithLabelValues("host", "system", "src", "zone-a", "success").Inc()
	m.HTTPResults.WithLabelValues("http://example.com", "GET", "200", "src", "zone-a", "success").Inc()

	m.TCPConnectDuration.WithLabelValues("src", "dst", "zone-a", "zone-b").Observe(0.001)
	m.UDPRtt.WithLabelValues("src", "dst", "zone-a", "zone-b").Observe(0.005)
	m.ICMPRtt.WithLabelValues("src", "dst", "zone-a", "zone-b").Observe(0.002)
	m.DNSDuration.WithLabelValues("host", "system", "src", "zone-a").Observe(0.01)

	m.UDPJitter.WithLabelValues("src", "dst", "zone-a", "zone-b").Set(0.001)
	m.UDPLossRatio.WithLabelValues("src", "dst", "zone-a", "zone-b").Set(0.0)
	m.ICMPLossRatio.WithLabelValues("src", "dst", "zone-a", "zone-b").Set(0.0)

	m.ControllerRegisteredAgents.WithLabelValues().Set(3)
	m.ControllerExpectedAgents.WithLabelValues().Set(4)
	m.ControllerLeader.WithLabelValues().Set(1)

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}

	expectedNames := map[string]bool{
		"kconmon_ng_tcp_results_total":            false,
		"kconmon_ng_udp_results_total":            false,
		"kconmon_ng_icmp_results_total":           false,
		"kconmon_ng_dns_results_total":            false,
		"kconmon_ng_http_results_total":           false,
		"kconmon_ng_tcp_connect_duration_seconds": false,
		"kconmon_ng_udp_rtt_seconds":              false,
		"kconmon_ng_controller_registered_agents": false,
		"kconmon_ng_controller_expected_agents":   false,
		"kconmon_ng_controller_leader":            false,
	}

	for _, f := range families {
		if _, ok := expectedNames[f.GetName()]; ok {
			expectedNames[f.GetName()] = true
		}
	}

	for name, found := range expectedNames {
		if !found {
			t.Errorf("expected metric %s not found", name)
		}
	}
}

func TestPrometheusMetricsCustomPrefix(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics("custom_prefix", reg)

	m.TCPResults.WithLabelValues("src", "dst", "za", "zb", "success").Inc()

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, f := range families {
		if f.GetName() == "custom_prefix_tcp_results_total" {
			found = true
			break
		}
	}

	if !found {
		t.Error("expected metric with custom prefix")
	}
}

// gatheredNames lists the metric family names a registry currently exposes.
func gatheredNames(t *testing.T, reg *prometheus.Registry) []string {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering registry: %v", err)
	}
	names := make([]string, 0, len(families))
	for _, f := range families {
		names = append(names, f.GetName())
	}
	return names
}

func TestExternalMetricFamiliesRegisteredUnderPrefix(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics("kconmon_ng", reg)

	base := []string{"node-a", "zone-a", "vendor-api", "url"}
	m.ExternalDuration.WithLabelValues(base...).Observe(0.012)
	m.ExternalRtt.WithLabelValues(base...).Observe(0.003)
	m.ExternalPacketLoss.WithLabelValues(base...).Set(0)
	m.ExternalHTTPStatusCode.WithLabelValues(base...).Set(200)
	m.ExternalResults.WithLabelValues(append(slices.Clone(base), "success")...).Inc()
	m.ExternalDenied.WithLabelValues(append(slices.Clone(base), "cidr")...).Inc()

	want := []string{
		"kconmon_ng_external_duration_seconds",
		"kconmon_ng_external_rtt_seconds",
		"kconmon_ng_external_packet_loss_ratio",
		"kconmon_ng_external_results_total",
		"kconmon_ng_external_http_status_code",
		"kconmon_ng_external_denied_total",
	}
	got := gatheredNames(t, reg)
	for _, name := range want {
		if !slices.Contains(got, name) {
			t.Errorf("expected metric %s not found in %v", name, got)
		}
	}
}

func TestExternalMetricFamiliesHonourCustomPrefix(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics("custom_prefix", reg)
	m.ExternalResults.WithLabelValues("node-a", "zone-a", "vendor-api", "host", "fail").Inc()

	if got := gatheredNames(t, reg); !slices.Contains(got, "custom_prefix_external_results_total") {
		t.Errorf("expected custom_prefix_external_results_total, got %v", got)
	}
}

// TestExternalFamiliesAbsentWhenFeatureUnused is the byte-for-byte exposition
// guard: an agent that never runs an external check must not gain a single
// kconmon_ng_external_* line, and an untouched *Vec collects nothing at all.
func TestExternalFamiliesAbsentWhenFeatureUnused(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics("kconmon_ng", reg)
	m.TCPResults.WithLabelValues("src", "dst", "za", "zb", "success").Inc()

	for _, name := range gatheredNames(t, reg) {
		if strings.Contains(name, "_external_") {
			t.Errorf("external family %s exposed with the feature unused", name)
		}
	}
}

func TestResetPeerGaugesClearsExternalGauges(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics("kconmon_ng", reg)

	base := []string{"node-a", "zone-a", "vendor-api", "host"}
	m.ExternalPacketLoss.WithLabelValues(base...).Set(0.5)
	m.ExternalHTTPStatusCode.WithLabelValues(base...).Set(503)
	m.ExternalResults.WithLabelValues(append(slices.Clone(base), "fail")...).Inc()

	before := gatheredNames(t, reg)
	for _, name := range []string{"kconmon_ng_external_packet_loss_ratio", "kconmon_ng_external_http_status_code"} {
		if !slices.Contains(before, name) {
			t.Fatalf("setup failed: %s not exposed before reset", name)
		}
	}

	m.ResetPeerGauges()

	after := gatheredNames(t, reg)
	for _, name := range []string{"kconmon_ng_external_packet_loss_ratio", "kconmon_ng_external_http_status_code"} {
		if slices.Contains(after, name) {
			t.Errorf("%s still exposed after ResetPeerGauges", name)
		}
	}
	// Counters are cumulative and must survive: only gauges pin a dead reading.
	if !slices.Contains(after, "kconmon_ng_external_results_total") {
		t.Error("ResetPeerGauges must not clear the external results counter")
	}
}

func TestNewPrometheusMetricsEventGauges(t *testing.T) {
	m := NewPrometheusMetrics("test", prometheus.NewRegistry())
	m.ControllerEventSubscribers.WithLabelValues().Set(2)
	m.ControllerEventsPublished.WithLabelValues("topology_changed").Inc()
	if got := testutil.ToFloat64(m.ControllerEventSubscribers.WithLabelValues()); got != 2 {
		t.Errorf("ControllerEventSubscribers = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.ControllerEventsPublished.WithLabelValues("topology_changed")); got != 1 {
		t.Errorf("ControllerEventsPublished = %v, want 1", got)
	}
}
