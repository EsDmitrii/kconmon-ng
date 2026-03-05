package metrics //nolint:revive // var-naming: "metrics" is a valid internal package name, not a stdlib conflict

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
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
