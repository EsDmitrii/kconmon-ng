package metrics //nolint:revive // var-naming: "metrics" is a valid internal package name, not a stdlib conflict

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// seriesWith counts exposed series, across every family, that carry label=value.
func seriesWith(t *testing.T, reg *prometheus.Registry, label, value string) map[string]int {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering registry: %v", err)
	}
	out := map[string]int{}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == label && lp.GetValue() == value {
					out[f.GetName()]++
				}
			}
		}
	}
	return out
}

// writePair records one result of every per-pair family for source node-a toward dst.
func writePair(m *PrometheusMetrics, dst, dstZone string) {
	l := []string{"node-a", dst, "zone-a", dstZone}
	for _, r := range []string{"success", "fail"} {
		rl := append(append([]string{}, l...), r)
		m.TCPResults.WithLabelValues(rl...).Inc()
		m.UDPResults.WithLabelValues(rl...).Inc()
		m.ICMPResults.WithLabelValues(rl...).Inc()
		m.PMTUResults.WithLabelValues(rl...).Inc()
	}
	m.TCPConnectDuration.WithLabelValues(l...).Observe(0.001)
	m.TCPTotalDuration.WithLabelValues(l...).Observe(0.002)
	m.UDPRtt.WithLabelValues(l...).Observe(0.001)
	m.ICMPRtt.WithLabelValues(l...).Observe(0.001)
	m.MTRTriggered.WithLabelValues(l...).Inc()
	m.UDPLossRatio.WithLabelValues(l...).Set(1)
	m.UDPJitter.WithLabelValues(l...).Set(0.001)
	m.ICMPLossRatio.WithLabelValues(l...).Set(1)
	m.PMTUBytes.WithLabelValues(l...).Set(1500)
	m.SetPMTUProbe(l, 1500)
	m.MTRHops.WithLabelValues(l...).Set(3)
	m.MTRHopRTT.WithLabelValues("node-a", dst, "1", "10.0.0.1").Set(0.001)
	m.ProbeIntended.WithLabelValues("node-a", dst).Set(1)
	m.ZoneTCPResults.WithLabelValues("zone-a", dstZone, "fail").Inc()
}

// A destination gone for good takes its counters and histograms with it; other peers and the zone
// family keep theirs.
func TestRetirePeerDropsEveryPairSeriesOfThatDestinationOnly(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics("kconmon_ng", reg)
	writePair(m, "node-gone", "zone-b")
	writePair(m, "node-live", "zone-b")

	if got := seriesWith(t, reg, "destination_node", "node-gone"); len(got) == 0 {
		t.Fatal("setup failed: nothing exposed for node-gone")
	}
	liveBefore := seriesWith(t, reg, "destination_node", "node-live")

	m.ForgetPeer("node-gone")
	m.RetirePeer("node-gone")

	if left := seriesWith(t, reg, "destination_node", "node-gone"); len(left) != 0 {
		t.Errorf("series left for a retired destination: %v", left)
	}
	liveAfter := seriesWith(t, reg, "destination_node", "node-live")
	for family, n := range liveBefore {
		if liveAfter[family] != n {
			t.Errorf("%s: node-live has %d series after retiring node-gone, want %d", family, liveAfter[family], n)
		}
	}
	// Zones outlive peers.
	if got := testutil.ToFloat64(m.ZoneTCPResults.WithLabelValues("zone-a", "zone-b", "fail")); got != 2 {
		t.Errorf("zone tcp results = %v after retiring a peer, want the 2 it accumulated", got)
	}
}

// A zone change keeps the node, so ForgetPeer never runs, but the old-zone gauges must go.
func TestRetirePeerZoneDropsOnlyTheOldZoneGauges(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics("kconmon_ng", reg)
	writePair(m, "node-b", "zone-old")
	writePair(m, "node-b", "zone-new")

	m.RetirePeerZone("node-b", "zone-old")

	for name, vec := range map[string]*prometheus.GaugeVec{
		"udp_packet_loss_ratio":  m.UDPLossRatio,
		"udp_jitter_seconds":     m.UDPJitter,
		"icmp_packet_loss_ratio": m.ICMPLossRatio,
		"pmtu_bytes":             m.PMTUBytes,
		"pmtu_probe_bytes":       m.PMTUProbeBytes,
		"mtr_hops":               m.MTRHops,
	} {
		if got := testutil.CollectAndCount(vec); got != 1 {
			t.Errorf("%s has %d series after the zone change, want only the new zone's 1", name, got)
		}
	}
	if got := testutil.ToFloat64(m.UDPLossRatio.WithLabelValues("node-a", "node-b", "zone-a", "zone-new")); got != 1 {
		t.Errorf("new-zone loss = %v, want the 1 it was set to", got)
	}
	// The pair is still planned and its path is still the path.
	if got := testutil.CollectAndCount(m.ProbeIntended); got != 1 {
		t.Errorf("probe_intended has %d series, want 1: the pair is still assigned", got)
	}
	if got := testutil.CollectAndCount(m.MTRHopRTT); got != 1 {
		t.Errorf("mtr_hop_rtt has %d series, want 1: it carries no zone", got)
	}
}

func TestRetireSourceZoneDropsOnlyThisAgentsOldZoneGauges(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics("kconmon_ng", reg)
	old := []string{"node-a", "node-b", "zone-old", "zone-b"}
	cur := []string{"node-a", "node-b", "zone-a", "zone-b"}
	m.UDPLossRatio.WithLabelValues(old...).Set(0.8)
	m.ICMPLossRatio.WithLabelValues(old...).Set(0.8)
	m.UDPLossRatio.WithLabelValues(cur...).Set(0)

	m.RetireSourceZone("node-a", "zone-old")

	if got := testutil.CollectAndCount(m.UDPLossRatio); got != 1 {
		t.Errorf("udp loss has %d series after this agent left zone-old, want 1", got)
	}
	if got := testutil.CollectAndCount(m.ICMPLossRatio); got != 0 {
		t.Errorf("icmp loss still has %d series under the old source zone", got)
	}
}

// A target that left the assignment keeps nothing, counters and histograms included.
func TestRetireExternalTargetDropsEveryFamilyOfThatTargetOnly(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics("kconmon_ng", reg)
	for _, target := range []string{"gone", "stays"} {
		l := []string{"node-a", "zone-a", target, "host", "icmp"}
		m.ExternalDuration.WithLabelValues(l...).Observe(0.01)
		m.ExternalRtt.WithLabelValues(l...).Observe(0.01)
		m.ExternalPacketLoss.WithLabelValues(l...).Set(0.5)
		m.ExternalHTTPStatusCode.WithLabelValues(l...).Set(200)
		m.ExternalResults.WithLabelValues(append(l, "fail")...).Inc()
		m.ExternalDenied.WithLabelValues(append(l, "cidr")...).Inc()
	}

	m.RetireExternalTarget("gone")

	if left := seriesWith(t, reg, "target", "gone"); len(left) != 0 {
		t.Errorf("series left for a retired external target: %v", left)
	}
	if got := seriesWith(t, reg, "target", "stays"); len(got) != 6 {
		t.Errorf("the other target has series in %d families, want all 6: %v", len(got), got)
	}
}

/*
An agent that adopts a zone (the node label arrived after it registered) leaves its DNS, HTTP and
external series under the old source_zone. The external gauges are the worst: a last loss of 1 or a
503 keeps serving as a current reading next to the new zone's series.
*/
func TestSourceZoneChangeRetiresTheAgentsDNSHTTPAndExternalSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics("kconmon_ng", reg)
	for _, src := range [][2]string{{"node-a", ""}, {"node-a", "zone-a"}, {"node-x", ""}} {
		node, zone := src[0], src[1]
		m.DNSDuration.WithLabelValues("svc", "10.96.0.10", node, zone).Observe(0.01)
		m.DNSResults.WithLabelValues("svc", "10.96.0.10", node, zone, "fail").Inc()
		for _, h := range []*prometheus.HistogramVec{m.HTTPDNSDuration, m.HTTPConnectDuration, m.HTTPTLSDuration, m.HTTPTTFBDuration, m.HTTPTotalDuration} {
			h.WithLabelValues("https://svc/health", node, zone).Observe(0.01)
		}
		m.HTTPResults.WithLabelValues("https://svc/health", "GET", "503", node, zone, "fail").Inc()
		l := []string{node, zone, "ext", "host", "icmp"}
		m.ExternalDuration.WithLabelValues(l...).Observe(0.01)
		m.ExternalRtt.WithLabelValues(l...).Observe(0.01)
		m.ExternalPacketLoss.WithLabelValues(l...).Set(1)
		m.ExternalHTTPStatusCode.WithLabelValues(l...).Set(503)
		m.ExternalResults.WithLabelValues(append(l, "fail")...).Inc()
		m.ExternalDenied.WithLabelValues(append(l, "cidr")...).Inc()
	}

	m.RetireSourceZone("node-a", "")

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	left := map[[2]string]map[string]int{}
	for _, f := range families {
		for _, s := range f.GetMetric() {
			var node, zone string
			hasZone := false
			for _, lp := range s.GetLabel() {
				switch lp.GetName() {
				case "source_node":
					node = lp.GetValue()
				case "source_zone":
					zone, hasZone = lp.GetValue(), true
				}
			}
			if !hasZone || node == "" {
				continue
			}
			k := [2]string{node, zone}
			if left[k] == nil {
				left[k] = map[string]int{}
			}
			left[k][f.GetName()]++
		}
	}
	if got := left[[2]string{"node-a", ""}]; len(got) != 0 {
		t.Errorf("series left under node-a's old source_zone: %v", got)
	}
	for _, k := range [][2]string{{"node-a", "zone-a"}, {"node-x", ""}} {
		if got := len(left[k]); got != 14 {
			t.Errorf("%v has series in %d families, want all 14: %v", k, got, left[k])
		}
	}
}
