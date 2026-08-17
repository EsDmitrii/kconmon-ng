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

	base := []string{"node-a", "zone-a", "vendor-api", "url", "http"}
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
	m.ExternalResults.WithLabelValues("node-a", "zone-a", "vendor-api", "host", "icmp", "fail").Inc()

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

/*
Forgetting one external target leaves the others alone, and leaves the counters alone.

The old ResetPeerGauges() called Reset() on these vectors wholesale, from the PEER-update callback —
an external target's packet loss disappeared because some unrelated agent pod restarted, and every
other external target went with it.
*/
func TestForgetExternalTargetDropsOnlyThatTarget(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics("kconmon_ng", reg)

	gone := []string{"node-a", "zone-a", "vendor-api", "host", "icmp"}
	stays := []string{"node-a", "zone-a", "billing-api", "url", "http"}
	m.ExternalPacketLoss.WithLabelValues(gone...).Set(0.5)
	m.ExternalHTTPStatusCode.WithLabelValues(gone...).Set(503)
	m.ExternalPacketLoss.WithLabelValues(stays...).Set(0.1)
	m.ExternalResults.WithLabelValues(append(slices.Clone(gone), "fail")...).Inc()

	before := gatheredNames(t, reg)
	for _, name := range []string{"kconmon_ng_external_packet_loss_ratio", "kconmon_ng_external_http_status_code"} {
		if !slices.Contains(before, name) {
			t.Fatalf("setup failed: %s not exposed before the retire", name)
		}
	}

	m.ForgetExternalTarget("vendor-api")

	if got := testutil.CollectAndCount(m.ExternalHTTPStatusCode); got != 0 {
		t.Errorf("vendor-api still has %d status-code series", got)
	}
	if got := testutil.ToFloat64(m.ExternalPacketLoss.WithLabelValues(stays...)); got != 0.1 {
		t.Errorf("billing-api packet loss = %v, want the 0.1 it was set to: an unrelated target was dropped", got)
	}
	if got := testutil.CollectAndCount(m.ExternalPacketLoss); got != 1 {
		t.Errorf("packet loss has %d series, want only billing-api's 1", got)
	}
	// Counters are cumulative and must survive: only gauges pin a dead reading.
	if !slices.Contains(gatheredNames(t, reg), "kconmon_ng_external_results_total") {
		t.Error("retiring a target must not clear the external results counter")
	}
}

/*
Forgetting a departed peer leaves every peer that is still there reporting.

The wholesale Reset() blanked the loss and jitter of every live pair too, and nothing repopulates a
gauge but the next probe of that pair — so each peer update opened a hole up to one check interval
wide in the series alerts evaluate, once per pod event.
*/
func TestForgetPeerDropsOnlyThatDestination(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics("kconmon_ng", reg)

	gone := []string{"node-a", "node-gone", "zone-a", "zone-b"}
	stays := []string{"node-a", "node-live", "zone-a", "zone-b"}
	m.UDPLossRatio.WithLabelValues(gone...).Set(1)
	m.UDPJitter.WithLabelValues(gone...).Set(0.02)
	m.ICMPLossRatio.WithLabelValues(gone...).Set(1)
	m.MTRHops.WithLabelValues(gone...).Set(7)
	m.MTRHopRTT.WithLabelValues("node-a", "node-gone", "3", "10.0.0.3").Set(0.004)

	m.UDPLossRatio.WithLabelValues(stays...).Set(0)
	m.UDPJitter.WithLabelValues(stays...).Set(0.001)
	m.MTRHopRTT.WithLabelValues("node-a", "node-live", "3", "10.0.0.3").Set(0.002)

	m.ForgetPeer("node-gone")

	for name, vec := range map[string]*prometheus.GaugeVec{
		"udp_packet_loss_ratio": m.UDPLossRatio,
		"udp_jitter_seconds":    m.UDPJitter,
		"mtr_hop_rtt_seconds":   m.MTRHopRTT,
	} {
		if got := testutil.CollectAndCount(vec); got != 1 {
			t.Errorf("%s has %d series after forgetting one peer, want the 1 that is still live", name, got)
		}
	}
	if got := testutil.ToFloat64(m.UDPLossRatio.WithLabelValues(stays...)); got != 0 {
		t.Errorf("the live peer's loss ratio = %v, want the 0 it was set to", got)
	}
	// The departed peer's own vectors, which had no live twin, are empty rather than stale.
	for name, vec := range map[string]*prometheus.GaugeVec{
		"icmp_packet_loss_ratio": m.ICMPLossRatio,
		"mtr_hops":               m.MTRHops,
	} {
		if got := testutil.CollectAndCount(vec); got != 0 {
			t.Errorf("%s still carries %d series for a peer that left", name, got)
		}
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
