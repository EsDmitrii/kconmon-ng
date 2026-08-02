package metrics //nolint:revive // var-naming: "metrics" is a valid internal package name, not a stdlib conflict

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestNewRegistersConsoleNamespacedMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New("kconmon_ng", reg)

	m.BuildInfo.WithLabelValues("v1", "abc").Set(1)
	m.HTTPRequests.WithLabelValues("GET", "/healthz", "200").Inc()

	got := testutil.CollectAndCount(reg)
	if got == 0 {
		t.Fatal("expected metrics registered, got 0")
	}

	names, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var haveBuild, haveReq bool
	for _, mf := range names {
		switch mf.GetName() {
		case "kconmon_ng_console_build_info":
			haveBuild = true
		case "kconmon_ng_console_http_requests_total":
			haveReq = true
		}
		if !strings.HasPrefix(mf.GetName(), "kconmon_ng_console_") {
			t.Errorf("metric %q not in kconmon_ng_console_ namespace", mf.GetName())
		}
	}
	if !haveBuild || !haveReq {
		t.Errorf("missing metrics: build=%v req=%v", haveBuild, haveReq)
	}
}

func TestNewRegistersRealtimeMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New("kconmon_ng", reg)

	m.EventsReceived.WithLabelValues("check_observed").Inc()
	m.EventsDeduped.WithLabelValues().Inc()
	m.IngesterConnected.WithLabelValues().Set(1)
	m.IngesterReconnects.WithLabelValues("capability").Inc()
	m.WSClients.WithLabelValues().Set(3)
	m.WSMessagesSent.WithLabelValues("live").Inc()
	m.WSDroppedClients.WithLabelValues().Inc()
	m.PushSnapshots.WithLabelValues("matrix:tcp:pod", "ok").Inc()

	if got := testutil.ToFloat64(m.EventsReceived.WithLabelValues("check_observed")); got != 1 {
		t.Errorf("EventsReceived = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.IngesterConnected.WithLabelValues()); got != 1 {
		t.Errorf("IngesterConnected = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.WSClients.WithLabelValues()); got != 3 {
		t.Errorf("WSClients = %v, want 3", got)
	}
	if got := testutil.ToFloat64(m.PushSnapshots.WithLabelValues("matrix:tcp:pod", "ok")); got != 1 {
		t.Errorf("PushSnapshots = %v, want 1", got)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool, len(families))
	for _, mf := range families {
		present[mf.GetName()] = true
		if !strings.HasPrefix(mf.GetName(), "kconmon_ng_console_") {
			t.Errorf("metric %q not in kconmon_ng_console_ namespace", mf.GetName())
		}
	}
	for _, name := range []string{
		"kconmon_ng_console_events_received_total",
		"kconmon_ng_console_events_deduped_total",
		"kconmon_ng_console_ingester_connected",
		"kconmon_ng_console_ingester_reconnects_total",
		"kconmon_ng_console_ws_clients",
		"kconmon_ng_console_ws_messages_sent_total",
		"kconmon_ng_console_ws_dropped_clients_total",
		"kconmon_ng_console_push_snapshots_total",
	} {
		if !present[name] {
			t.Errorf("metric %q was not registered", name)
		}
	}
}
