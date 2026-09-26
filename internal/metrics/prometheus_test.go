package metrics //nolint:revive // var-naming: "metrics" is a valid internal package name, not a stdlib conflict

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/config"
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
	m.ControllerExternalAgents.WithLabelValues().Set(1)
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
		"kconmon_ng_controller_external_agents":   false,
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

// Forgetting a departed peer leaves every peer that is still there reporting: nothing repopulates a
// gauge but the next probe of that pair, so a live pair's gap would be a hole in what alerts evaluate.
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

// A zone change retires the pair's counters and histograms under the old zone only: the same pair
// under its new zone, and other pairs, keep counting.
func TestRetireZoneDropsOnlyTheOldZonesPairSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics("kconmon_ng", reg)
	for _, pair := range [][2]string{{"node-b", "zone-1"}, {"node-b", "zone-2"}, {"node-c", "zone-1"}} {
		m.UDPResults.WithLabelValues("node-a", pair[0], "zone-a", pair[1], "success").Inc()
		m.UDPRtt.WithLabelValues("node-a", pair[0], "zone-a", pair[1]).Observe(0.001)
	}

	m.RetirePeerZone("node-b", "zone-1")
	if got := testutil.CollectAndCount(m.UDPResults); got != 2 {
		t.Errorf("udp_results_total has %d series after node-b left zone-1, want 2", got)
	}
	if got := testutil.ToFloat64(m.UDPResults.WithLabelValues("node-a", "node-b", "zone-a", "zone-2", "success")); got != 1 {
		t.Errorf("node-b's counter under its new zone = %v, want the 1 it counted", got)
	}

	m.RetireSourceZone("node-a", "zone-a")
	for name, c := range map[string]prometheus.Collector{"udp_results_total": m.UDPResults, "udp_rtt_seconds": m.UDPRtt} {
		if got := testutil.CollectAndCount(c); got != 0 {
			t.Errorf("%s keeps %d series after node-a left zone-a", name, got)
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

// Mirrors the console's kconmon_ng_console_build_info: same labels, value fixed
// at 1, populated at construction so both binaries expose it without wiring.
func TestNewPrometheusMetricsBuildInfo(t *testing.T) {
	reg := prometheus.NewRegistry()
	NewPrometheusMetrics("kconmon_ng", reg)

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, f := range families {
		if f.GetName() != "kconmon_ng_build_info" {
			continue
		}
		found = true
		if len(f.GetMetric()) != 1 {
			t.Fatalf("build_info has %d series, want exactly 1", len(f.GetMetric()))
		}
		s := f.GetMetric()[0]
		if got := s.GetGauge().GetValue(); got != 1 {
			t.Errorf("build_info value = %v, want 1", got)
		}
		labels := map[string]string{}
		for _, lp := range s.GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		if labels["version"] != config.Version || labels["commit"] != config.Commit {
			t.Errorf("build_info labels = %v, want version=%q commit=%q", labels, config.Version, config.Commit)
		}
	}
	if !found {
		t.Error("kconmon_ng_build_info not found in gathered families")
	}
}

/*
The zone family's exact metric names are part of the design contract: the chart's rules and the
zone dashboard address them literally, so a rename here silently kills alerts.
*/
func TestZoneFamilyRegisteredUnderPrefix(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics("kconmon_ng", reg)

	zone := []string{"zone-a", "zone-b"}
	m.ZoneTCPConnect.WithLabelValues(zone...).Observe(0.001)
	m.ZoneTCPTotal.WithLabelValues(zone...).Observe(0.002)
	m.ZoneUDPRtt.WithLabelValues(zone...).Observe(0.003)
	m.ZoneICMPRtt.WithLabelValues(zone...).Observe(0.004)
	m.ZoneTCPResults.WithLabelValues("zone-a", "zone-b", "success").Inc()
	m.ZoneUDPResults.WithLabelValues("zone-a", "zone-b", "fail").Inc()
	m.ZoneICMPResults.WithLabelValues("zone-a", "zone-b", "success").Inc()
	m.ZoneUDPPacketsSent.WithLabelValues(zone...).Add(5)
	m.ZoneUDPPacketsReceived.WithLabelValues(zone...).Add(3)
	m.ZoneICMPPacketsSent.WithLabelValues(zone...).Inc()
	m.ZoneICMPPacketsReceived.WithLabelValues(zone...).Inc()

	want := []string{
		"kconmon_ng_zone_tcp_connect_seconds",
		"kconmon_ng_zone_tcp_total_seconds",
		"kconmon_ng_zone_udp_rtt_seconds",
		"kconmon_ng_zone_icmp_rtt_seconds",
		"kconmon_ng_zone_tcp_results_total",
		"kconmon_ng_zone_udp_results_total",
		"kconmon_ng_zone_icmp_results_total",
		"kconmon_ng_zone_udp_packets_sent_total",
		"kconmon_ng_zone_udp_packets_received_total",
		"kconmon_ng_zone_icmp_packets_sent_total",
		"kconmon_ng_zone_icmp_packets_received_total",
	}
	got := gatheredNames(t, reg)
	for _, name := range want {
		if !slices.Contains(got, name) {
			t.Errorf("expected zone metric %s not found in %v", name, got)
		}
	}
}

// The zone histograms must share defaultBuckets with the per-pair family, or a recording of the
// same probe lands in different buckets depending on which family a panel reads.
func TestZoneHistogramsUseDefaultBuckets(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics("kconmon_ng", reg)
	m.ZoneICMPRtt.WithLabelValues("zone-a", "zone-b").Observe(0.001)

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "kconmon_ng_zone_icmp_rtt_seconds" {
			continue
		}
		buckets := f.GetMetric()[0].GetHistogram().GetBucket()
		if len(buckets) != len(defaultBuckets) {
			t.Fatalf("zone histogram has %d buckets, want the %d defaultBuckets", len(buckets), len(defaultBuckets))
		}
		for i, b := range buckets {
			if b.GetUpperBound() != defaultBuckets[i] {
				t.Errorf("bucket %d bound = %v, want %v", i, b.GetUpperBound(), defaultBuckets[i])
			}
		}
		return
	}
	t.Fatal("kconmon_ng_zone_icmp_rtt_seconds not gathered")
}

// ZoneResultCounter mirrors PeerResultCounter: only the pair-probing check types have a zone
// results counter, and the preinit path relies on nil for everything else.
func TestZoneResultCounterMapsOnlyPeerCheckTypes(t *testing.T) {
	m := NewPrometheusMetrics("kconmon_ng", prometheus.NewRegistry())
	if m.ZoneResultCounter("tcp") != m.ZoneTCPResults {
		t.Error("tcp must map to ZoneTCPResults")
	}
	if m.ZoneResultCounter("udp") != m.ZoneUDPResults {
		t.Error("udp must map to ZoneUDPResults")
	}
	if m.ZoneResultCounter("icmp") != m.ZoneICMPResults {
		t.Error("icmp must map to ZoneICMPResults")
	}
	for _, other := range []string{"dns", "http", "mtr", "external", ""} {
		if m.ZoneResultCounter(other) != nil {
			t.Errorf("%q must have no zone results counter", other)
		}
	}
}

// ZonePacketCounters exist only for the check types that count packets; tcp is one connect, not a
// packet train, and inventing a packet counter for it would fabricate a loss signal.
func TestZonePacketCountersOnlyForLossCapableTypes(t *testing.T) {
	m := NewPrometheusMetrics("kconmon_ng", prometheus.NewRegistry())
	if sent, recv := m.ZonePacketCounters("udp"); sent != m.ZoneUDPPacketsSent || recv != m.ZoneUDPPacketsReceived {
		t.Error("udp must map to the udp packet counters")
	}
	if sent, recv := m.ZonePacketCounters("icmp"); sent != m.ZoneICMPPacketsSent || recv != m.ZoneICMPPacketsReceived {
		t.Error("icmp must map to the icmp packet counters")
	}
	if sent, recv := m.ZonePacketCounters("tcp"); sent != nil || recv != nil {
		t.Error("tcp must have no packet counters")
	}
}

/*
ForgetPeer retires PAIR series; the zone family survives every peer departure.

Zones outlive peers: a node draining out of zone-b says nothing about zone-a→zone-b as a path, and
the zone counters are cumulative aggregates that the zone alerts rate() over — deleting them on
peer churn would reset the very series the alerts watch, once per pod event.
*/
func TestForgetPeerLeavesZoneFamilyStanding(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics("kconmon_ng", reg)

	m.UDPLossRatio.WithLabelValues("node-a", "node-gone", "zone-a", "zone-b").Set(1)
	m.ZoneUDPPacketsSent.WithLabelValues("zone-a", "zone-b").Add(5)
	m.ZoneUDPPacketsReceived.WithLabelValues("zone-a", "zone-b").Add(3)
	m.ZoneUDPResults.WithLabelValues("zone-a", "zone-b", "success").Inc()
	m.ZoneICMPRtt.WithLabelValues("zone-a", "zone-b").Observe(0.002)

	m.ForgetPeer("node-gone")

	if got := testutil.CollectAndCount(m.UDPLossRatio); got != 0 {
		t.Errorf("per-pair loss gauge has %d series after the peer left, want 0", got)
	}
	if got := testutil.ToFloat64(m.ZoneUDPPacketsSent.WithLabelValues("zone-a", "zone-b")); got != 5 {
		t.Errorf("zone packets sent = %v after ForgetPeer, want the 5 it accumulated", got)
	}
	if got := testutil.ToFloat64(m.ZoneUDPResults.WithLabelValues("zone-a", "zone-b", "success")); got != 1 {
		t.Errorf("zone results = %v after ForgetPeer, want 1", got)
	}
	if got := testutil.CollectAndCount(m.ZoneICMPRtt); got != 1 {
		t.Errorf("zone icmp rtt has %d series after ForgetPeer, want 1", got)
	}
}

/*
The peer-list age is computed AT SCRAPE TIME from the caller's stamp: a
value written once at update time would serve a stale age on every scrape,
hiding exactly the cut-off-agent condition the series exists to expose. Before
the first update the age runs from arming, so "never had a peer list" reads as
a growing number instead of a lie.
*/
func TestEnablePeerListAge(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics("kconmon_ng", reg)

	var mu sync.Mutex
	last := time.Time{}
	m.EnablePeerListAge(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return last
	})

	readAge := func() float64 {
		t.Helper()
		families, err := reg.Gather()
		if err != nil {
			t.Fatalf("gather: %v", err)
		}
		for _, f := range families {
			if f.GetName() == "kconmon_ng_agent_peer_list_age_seconds" {
				return f.GetMetric()[0].GetGauge().GetValue()
			}
		}
		t.Fatal("kconmon_ng_agent_peer_list_age_seconds not exported")
		return 0
	}

	// Zero stamp: age counts from arming, small and non-negative.
	if age := readAge(); age < 0 || age > 5 {
		t.Fatalf("age before any update = %v, want a small non-negative number", age)
	}

	mu.Lock()
	last = time.Now().Add(-42 * time.Second)
	mu.Unlock()
	if age := readAge(); age < 41 || age > 44 {
		t.Fatalf("age for a 42s-old stamp = %v, want ~42", age)
	}
}

// The self-observation family registers under the agent prefix and the
// documented names.
func TestAgentSelfMetricNames(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics("kconmon_ng", reg)

	m.AgentProbeCycleDuration.WithLabelValues("tcp").Observe(0.1)
	m.AgentProbeCycleOverruns.WithLabelValues("tcp").Inc()
	m.AgentControllerReconnects.WithLabelValues().Inc()
	m.AgentMTRReactiveInflight.WithLabelValues().Set(2)
	m.AgentMTRReactiveCoalesced.WithLabelValues("cooldown").Inc()
	m.AgentMTRReactiveCoalesced.WithLabelValues("saturated").Inc()

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, f := range families {
		found[f.GetName()] = true
	}
	for _, name := range []string{
		"kconmon_ng_agent_probe_cycle_duration_seconds",
		"kconmon_ng_agent_probe_cycle_overruns_total",
		"kconmon_ng_agent_controller_reconnects_total",
		"kconmon_ng_agent_mtr_reactive_inflight",
		"kconmon_ng_agent_mtr_reactive_coalesced_total",
	} {
		if !found[name] {
			t.Errorf("expected self-metric %s not found", name)
		}
	}
}

/*
kconmon_ng_probe_intended is the topology PLAN made scrapable: value 1 for every directed pair
this agent is assigned to probe, labelled by the two node names alone. PairWentSilent joins on it,
so the family must exist under the configured prefix with exactly {source_node, destination_node}
— an extra label would break the on() join, a missing one would collapse pairs.
*/
func TestProbeIntendedRegisteredUnderPrefix(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics("kconmon_ng", reg)
	m.ProbeIntended.WithLabelValues("node-a", "node-b").Set(1)

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "kconmon_ng_probe_intended" {
			continue
		}
		metric := f.GetMetric()[0]
		if got := metric.GetGauge().GetValue(); got != 1 {
			t.Errorf("probe_intended value = %v, want 1: the plan is a membership set, not a measurement", got)
		}
		labels := make([]string, 0, len(metric.GetLabel()))
		for _, l := range metric.GetLabel() {
			labels = append(labels, l.GetName())
		}
		want := []string{"destination_node", "source_node"}
		if !slices.Equal(labels, want) {
			t.Errorf("probe_intended labels = %v, want exactly %v", labels, want)
		}
		return
	}
	t.Fatal("kconmon_ng_probe_intended not gathered")
}

// A peer that leaves the assignment must take its intent series with it, and only its own:
// PairWentSilent reads this family as the plan, and a stale 1 keeps alerting on a pair the
// controller no longer schedules.
func TestForgetPeerDropsProbeIntendedForThatDestinationOnly(t *testing.T) {
	m := NewPrometheusMetrics("kconmon_ng", prometheus.NewRegistry())
	m.ProbeIntended.WithLabelValues("node-a", "node-gone").Set(1)
	m.ProbeIntended.WithLabelValues("node-a", "node-live").Set(1)

	m.ForgetPeer("node-gone")

	if got := testutil.CollectAndCount(m.ProbeIntended); got != 1 {
		t.Errorf("probe_intended has %d series after forgetting one peer, want the 1 still assigned", got)
	}
	if got := testutil.ToFloat64(m.ProbeIntended.WithLabelValues("node-a", "node-live")); got != 1 {
		t.Errorf("the still-assigned pair's probe_intended = %v, want 1", got)
	}
}

func TestPrometheusMetricsPMTUFamilies(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics("kconmon_ng", reg)

	m.PMTUBytes.WithLabelValues("src", "dst", "zone-a", "zone-b").Set(1400)
	m.PMTUResults.WithLabelValues("src", "dst", "zone-a", "zone-b", "fail").Inc()
	m.ZonePMTUResults.WithLabelValues("zone-a", "zone-b", "fail").Inc()
	m.AgentPMTUProbeBytes.WithLabelValues("src").Set(1500)

	got := gatheredNames(t, reg)
	for _, name := range []string{
		"kconmon_ng_pmtu_bytes",
		"kconmon_ng_pmtu_results_total",
		"kconmon_ng_zone_pmtu_results_total",
		"kconmon_ng_agent_pmtu_probe_bytes",
	} {
		if !slices.Contains(got, name) {
			t.Errorf("expected metric %s not found in %v", name, got)
		}
	}
	if m.PeerResultCounter("pmtu") != m.PMTUResults {
		t.Error(`PeerResultCounter("pmtu") must return PMTUResults so the peer preinit covers it`)
	}
	if m.ZoneResultCounter("pmtu") != m.ZonePMTUResults {
		t.Error(`ZoneResultCounter("pmtu") must return ZonePMTUResults so the zone preinit covers it`)
	}
	if sent, recv := m.ZonePacketCounters("pmtu"); sent != nil || recv != nil {
		t.Error("pmtu sends a search, not a packet train: it must not get packet counters")
	}

	m.ForgetPeer("dst")
	if n := testutil.CollectAndCount(m.PMTUBytes); n != 0 {
		t.Errorf("ForgetPeer left %d pmtu_bytes series for the departed peer", n)
	}
	if n := testutil.CollectAndCount(m.PMTUResults); n != 1 {
		t.Errorf("ForgetPeer must not drop counters, pmtu_results_total has %d series, want 1", n)
	}
}

// The probe size is per pair (the route to each peer), and the per-agent gauge is the max over the
// pairs still published: it follows every retirement of the per-pair series.
func TestPMTUProbeBytesPerPairAndAgentMax(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics("kconmon_ng", reg)
	agent := func() (float64, bool) {
		if testutil.CollectAndCount(m.AgentPMTUProbeBytes) == 0 {
			return 0, false
		}
		return testutil.ToFloat64(m.AgentPMTUProbeBytes.WithLabelValues("a")), true
	}

	m.SetPMTUProbe([]string{"a", "vpn", "zone-a", "zone-v"}, 1420)
	m.SetPMTUProbe([]string{"a", "lan", "zone-a", "zone-a"}, 1500)
	m.SetPMTUProbe([]string{"a", "far", "zone-a", "zone-f"}, 1450)
	if got := testutil.ToFloat64(m.PMTUProbeBytes.WithLabelValues("a", "vpn", "zone-a", "zone-v")); got != 1420 {
		t.Errorf("pmtu_probe_bytes a->vpn = %v, want 1420", got)
	}
	if got, ok := agent(); !ok || got != 1500 {
		t.Errorf("agent_pmtu_probe_bytes = %v (present=%v), want the max 1500", got, ok)
	}

	m.SetPMTUProbe([]string{"a", "lan", "zone-a", "zone-a"}, 1400)
	if got, _ := agent(); got != 1450 {
		t.Errorf("agent_pmtu_probe_bytes after lan dropped to 1400 = %v, want 1450", got)
	}
	m.ForgetPeer("far")
	if got, _ := agent(); got != 1420 {
		t.Errorf("agent_pmtu_probe_bytes after far left = %v, want 1420", got)
	}
	m.RetirePeerZone("vpn", "zone-v")
	if n := testutil.CollectAndCount(m.PMTUProbeBytes); n != 1 {
		t.Errorf("pmtu_probe_bytes has %d series after vpn changed zone, want lan's 1", n)
	}
	if got, _ := agent(); got != 1400 {
		t.Errorf("agent_pmtu_probe_bytes after vpn changed zone = %v, want 1400", got)
	}
	m.RetireSourceZone("a", "zone-a")
	if n := testutil.CollectAndCount(m.PMTUProbeBytes); n != 0 {
		t.Errorf("pmtu_probe_bytes has %d series after a left zone-a, want 0", n)
	}
	if got, ok := agent(); ok {
		t.Errorf("agent_pmtu_probe_bytes = %v with no pair left, want the series gone", got)
	}

	m.SetPMTUProbe([]string{"a", "lan", "zone-b", "zone-a"}, 1500)
	m.ForgetPlane("pmtu")
	if n := testutil.CollectAndCount(m.PMTUProbeBytes) + testutil.CollectAndCount(m.AgentPMTUProbeBytes); n != 0 {
		t.Errorf("%d probe-size series left after the pmtu plane was switched off", n)
	}
	m.SetPMTUProbe([]string{"a", "lan", "zone-b", "zone-a"}, 1300)
	if got, _ := agent(); got != 1300 {
		t.Errorf("agent_pmtu_probe_bytes after the plane came back = %v, want 1300, not a stale max", got)
	}
}

// Result handlers run concurrently per peer while peer updates retire series.
func TestPMTUProbeBytesConcurrentWritesAndRetirement(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics("kconmon_ng", reg)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			dst := "peer-" + string(rune('a'+i))
			for j := range 200 {
				m.SetPMTUProbe([]string{"a", dst, "zone-a", "zone-b"}, float64(1400+j%100))
				if j%50 == 0 {
					m.ForgetPeer(dst)
				}
			}
		})
	}
	wg.Wait()
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("gather: %v", err)
	}
	if got := testutil.ToFloat64(m.AgentPMTUProbeBytes.WithLabelValues("a")); got != 1499 {
		t.Errorf("agent_pmtu_probe_bytes = %v, want 1499, every peer's last write", got)
	}
}
