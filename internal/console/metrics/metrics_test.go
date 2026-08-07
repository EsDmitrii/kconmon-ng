package metrics //nolint:revive // var-naming: "metrics" is a valid internal package name, not a stdlib conflict

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
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

// TestNewRegistersStoreMetrics exercises the M3 persistence metrics. query is
// a closed set of generated sqlc method names (here: InsertTopologyEvent,
// ListTopologyEvents); result is always one of ok|conflict|error -- both
// closed sets are asserted by exercising every value, not just one.
func TestNewRegistersStoreMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New("kconmon_ng", reg)

	for _, query := range []string{"InsertTopologyEvent", "ListTopologyEvents"} {
		for _, result := range []string{"ok", "conflict", "error"} {
			m.StoreQueries.WithLabelValues(query, result).Inc()
		}
		m.StoreQueryDuration.WithLabelValues(query).Observe(0.01)
	}
	for _, state := range []string{"acquired", "idle", "total"} {
		m.StorePoolConns.WithLabelValues(state).Set(1)
	}
	for _, result := range []string{"ok", "conflict", "error"} {
		m.EventsPersisted.WithLabelValues(result).Inc()
	}
	for _, table := range []string{"topology_events", "audit_log", "check_runs"} {
		m.RetentionDeleted.WithLabelValues(table).Add(5)
	}

	if got := testutil.ToFloat64(m.StoreQueries.WithLabelValues("InsertTopologyEvent", "conflict")); got != 1 {
		t.Errorf("StoreQueries(InsertTopologyEvent, conflict) = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.StorePoolConns.WithLabelValues("idle")); got != 1 {
		t.Errorf("StorePoolConns(idle) = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.EventsPersisted.WithLabelValues("error")); got != 1 {
		t.Errorf("EventsPersisted(error) = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.RetentionDeleted.WithLabelValues("topology_events")); got != 5 {
		t.Errorf("RetentionDeleted(topology_events) = %v, want 5", got)
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
		"kconmon_ng_console_store_queries_total",
		"kconmon_ng_console_store_query_duration_seconds",
		"kconmon_ng_console_store_pool_conns",
		"kconmon_ng_console_events_persisted_total",
		"kconmon_ng_console_retention_deleted_total",
	} {
		if !present[name] {
			t.Errorf("metric %q was not registered", name)
		}
	}
}

// TestNewRegistersAuthMetrics exercises the M3 auth metrics: AuthRequests's
// result label (ok|invalid|expired|error) and AuthzDenied's permission
// label, which must stay bounded by authz.AllPermissions -- every permission
// this build knows about, and nothing else, is exercised here.
func TestNewRegistersAuthMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New("kconmon_ng", reg)

	for _, mode := range []string{"basic", "oidc", "token"} {
		for _, result := range []string{"ok", "invalid", "expired", "error"} {
			m.AuthRequests.WithLabelValues(mode, result).Inc()
		}
	}
	if len(authz.AllPermissions) == 0 {
		t.Fatal("authz.AllPermissions is empty -- nothing to assert AuthzDenied's label set against")
	}
	for _, perm := range authz.AllPermissions {
		m.AuthzDenied.WithLabelValues(string(perm)).Inc()
	}

	if got := testutil.ToFloat64(m.AuthRequests.WithLabelValues("basic", "invalid")); got != 1 {
		t.Errorf("AuthRequests(basic, invalid) = %v, want 1", got)
	}
	for _, perm := range authz.AllPermissions {
		if got := testutil.ToFloat64(m.AuthzDenied.WithLabelValues(string(perm))); got != 1 {
			t.Errorf("AuthzDenied(%s) = %v, want 1", perm, got)
		}
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
		"kconmon_ng_console_auth_requests_total",
		"kconmon_ng_console_authz_denied_total",
	} {
		if !present[name] {
			t.Errorf("metric %q was not registered", name)
		}
	}
}

// TestNewRegistersRunMetrics exercises the M3 checks.Runner metrics (Task
// 22): RunsTotal's status label (succeeded|partial|failed -- never a
// boolean) and RunPairs' result label (ok|failed|timeout), both closed sets.
func TestNewRegistersRunMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New("kconmon_ng", reg)

	for _, status := range []string{"succeeded", "partial", "failed"} {
		m.RunsTotal.WithLabelValues("tcp", status).Inc()
	}
	for _, result := range []string{"ok", "failed", "timeout"} {
		m.RunPairs.WithLabelValues(result).Inc()
	}
	m.RunDuration.WithLabelValues("tcp").Observe(12.5)

	if got := testutil.ToFloat64(m.RunsTotal.WithLabelValues("tcp", "partial")); got != 1 {
		t.Errorf("RunsTotal(tcp, partial) = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.RunPairs.WithLabelValues("timeout")); got != 1 {
		t.Errorf("RunPairs(timeout) = %v, want 1", got)
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
		"kconmon_ng_console_runs_total",
		"kconmon_ng_console_run_pairs_total",
		"kconmon_ng_console_run_duration_seconds",
	} {
		if !present[name] {
			t.Errorf("metric %q was not registered", name)
		}
	}
}
