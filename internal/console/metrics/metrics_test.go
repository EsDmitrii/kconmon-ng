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
	// Every RetentionDeleted table label, exercised rather than sampled: the
	// set is closed (store/prune.go owns it) and M5 widened it by three.
	for _, table := range []string{
		"topology_events", "audit_log", "check_runs",
		"mtr_path_snapshots", "mtr_hop_enrichment", "annotations",
	} {
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

// TestNewRegistersRateLimitMetrics exercises the M4 Task 8 limiter metrics:
// both carry exactly one label, limit, over the CLOSED set runs|login --
// never a username, subject ID or source IP.
func TestNewRegistersRateLimitMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New("kconmon_ng", reg)

	for _, limit := range []string{"runs", "login"} {
		m.RateLimited.WithLabelValues(limit).Inc()
		m.RateLimitFailOpen.WithLabelValues(limit).Inc()
	}

	if got := testutil.ToFloat64(m.RateLimited.WithLabelValues("login")); got != 1 {
		t.Errorf("RateLimited(login) = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.RateLimitFailOpen.WithLabelValues("runs")); got != 1 {
		t.Errorf("RateLimitFailOpen(runs) = %v, want 1", got)
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
		if !strings.HasPrefix(mf.GetName(), "kconmon_ng_console_rate_limit") {
			continue
		}
		for _, metric := range mf.GetMetric() {
			labels := metric.GetLabel()
			if len(labels) != 1 || labels[0].GetName() != "limit" {
				t.Errorf("%s has labels %v, want exactly one {limit}", mf.GetName(), labels)
				continue
			}
			switch labels[0].GetValue() {
			case "runs", "login":
			default:
				t.Errorf("%s has limit=%q, outside the closed set runs|login",
					mf.GetName(), labels[0].GetValue())
			}
		}
	}
	for _, name := range []string{
		"kconmon_ng_console_rate_limited_total",
		"kconmon_ng_console_rate_limit_failopen_total",
	} {
		if !present[name] {
			t.Errorf("metric %q was not registered", name)
		}
	}
}

// TestNewRegistersExternalReconcilerMetrics exercises the M4 Task 17
// continuous-assignment metrics: the projected-series gauge carries NO labels
// at all, and both counters carry exactly one label over a closed set.
func TestNewRegistersExternalReconcilerMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New("kconmon_ng", reg)

	m.ExternalSeriesProjected.WithLabelValues().Set(12)
	for _, result := range []string{"pushed", "unchanged", "not-leader", "error"} {
		m.ExternalReconciles.WithLabelValues(result).Inc()
	}
	for _, reason := range []string{"check-type", "destination-kind"} {
		m.ExternalSpecsSkipped.WithLabelValues(reason).Inc()
	}

	if got := testutil.ToFloat64(m.ExternalSeriesProjected.WithLabelValues()); got != 12 {
		t.Errorf("ExternalSeriesProjected = %v, want 12", got)
	}
	if got := testutil.ToFloat64(m.ExternalReconciles.WithLabelValues("unchanged")); got != 1 {
		t.Errorf("ExternalReconciles(unchanged) = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.ExternalSpecsSkipped.WithLabelValues("check-type")); got != 1 {
		t.Errorf("ExternalSpecsSkipped(check-type) = %v, want 1", got)
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
		for _, metric := range mf.GetMetric() {
			labels := metric.GetLabel()
			switch mf.GetName() {
			case "kconmon_ng_console_external_series_projected":
				if len(labels) != 0 {
					t.Errorf("%s has labels %v, want none", mf.GetName(), labels)
				}
			case "kconmon_ng_console_external_reconciles_total":
				if len(labels) != 1 || labels[0].GetName() != "result" {
					t.Errorf("%s has labels %v, want exactly one {result}", mf.GetName(), labels)
					continue
				}
				switch labels[0].GetValue() {
				case "pushed", "unchanged", "not-leader", "error":
				default:
					t.Errorf("%s has result=%q, outside the closed set", mf.GetName(), labels[0].GetValue())
				}
			case "kconmon_ng_console_external_specs_skipped_total":
				if len(labels) != 1 || labels[0].GetName() != "reason" {
					t.Errorf("%s has labels %v, want exactly one {reason}", mf.GetName(), labels)
					continue
				}
				switch labels[0].GetValue() {
				case "check-type", "destination-kind":
				default:
					t.Errorf("%s has reason=%q, outside the closed set", mf.GetName(), labels[0].GetValue())
				}
			}
		}
	}
	for _, name := range []string{
		"kconmon_ng_console_external_series_projected",
		"kconmon_ng_console_external_reconciles_total",
		"kconmon_ng_console_external_specs_skipped_total",
	} {
		if !present[name] {
			t.Errorf("metric %q was not registered", name)
		}
	}
}

// TestNewRegistersMTRSnapshotMetric exercises the M5 Task 2 path-history
// projector metric: exactly one label, result, over the CLOSED set
// new-path|repeat|error. A hop address, a path hash or a destination in a
// label here would be the cardinality bomb the whole package's discipline
// exists to prevent, so the label VALUES are pinned, not just the name.
func TestNewRegistersMTRSnapshotMetric(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New("kconmon_ng", reg)

	for _, result := range []string{"new-path", "repeat", "error"} {
		m.MTRSnapshots.WithLabelValues(result).Inc()
	}

	if got := testutil.ToFloat64(m.MTRSnapshots.WithLabelValues("new-path")); got != 1 {
		t.Errorf("MTRSnapshots(new-path) = %v, want 1", got)
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
		if mf.GetName() != "kconmon_ng_console_mtr_snapshots_total" {
			continue
		}
		for _, metric := range mf.GetMetric() {
			labels := metric.GetLabel()
			if len(labels) != 1 || labels[0].GetName() != "result" {
				t.Errorf("%s has labels %v, want exactly one {result}", mf.GetName(), labels)
				continue
			}
			switch labels[0].GetValue() {
			case "new-path", "repeat", "error":
			default:
				t.Errorf("%s has result=%q, outside the closed set new-path|repeat|error",
					mf.GetName(), labels[0].GetValue())
			}
		}
	}
	if !present["kconmon_ng_console_mtr_snapshots_total"] {
		t.Error("metric \"kconmon_ng_console_mtr_snapshots_total\" was not registered")
	}
}

// TestNewRegistersEnrichmentMetrics pins M5 Task 5's two counters and, more
// importantly, their SHAPE. The obvious single-counter design --
// enrichment_lookups_total{source,result} with result hit|miss|error -- is
// wrong: a cache hit is not per-source (one cached row answers rdns, asn and
// city at once), so "hit" would have had to be attributed to a source that
// never ran. The split below keeps each counter answering one question:
//
//	enrichment_cache_total{result}      -- did the TTL cache answer? hit|miss,
//	                                       one increment per IP, so hit/(hit+miss)
//	                                       is the cache hit ratio.
//	enrichment_lookups_total{source,result} -- what did each source that ACTUALLY
//	                                       RAN produce? source rdns|asn|city,
//	                                       result ok|miss|error. Only misses reach
//	                                       here, so this never double-counts the
//	                                       cache.
//
// Neither carries an IP, a hostname, an ASN, a country or a snapshot id.
func TestNewRegistersEnrichmentMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New("kconmon_ng", reg)

	for _, result := range []string{"hit", "miss"} {
		m.EnrichmentCache.WithLabelValues(result).Inc()
	}
	for _, source := range []string{"rdns", "asn", "city"} {
		for _, result := range []string{"ok", "miss", "error"} {
			m.EnrichmentLookups.WithLabelValues(source, result).Inc()
		}
	}

	if got := testutil.ToFloat64(m.EnrichmentCache.WithLabelValues("hit")); got != 1 {
		t.Errorf("EnrichmentCache(hit) = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.EnrichmentLookups.WithLabelValues("rdns", "error")); got != 1 {
		t.Errorf("EnrichmentLookups(rdns, error) = %v, want 1", got)
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
		switch mf.GetName() {
		case "kconmon_ng_console_enrichment_cache_total":
			for _, metric := range mf.GetMetric() {
				labels := metric.GetLabel()
				if len(labels) != 1 || labels[0].GetName() != "result" {
					t.Errorf("%s has labels %v, want exactly one {result}", mf.GetName(), labels)
					continue
				}
				switch labels[0].GetValue() {
				case "hit", "miss":
				default:
					t.Errorf("%s has result=%q, outside the closed set hit|miss", mf.GetName(), labels[0].GetValue())
				}
			}
		case "kconmon_ng_console_enrichment_lookups_total":
			for _, metric := range mf.GetMetric() {
				labels := metric.GetLabel()
				if len(labels) != 2 || labels[0].GetName() != "result" || labels[1].GetName() != "source" {
					t.Errorf("%s has labels %v, want exactly {result, source}", mf.GetName(), labels)
					continue
				}
				switch labels[1].GetValue() {
				case "rdns", "asn", "city":
				default:
					t.Errorf("%s has source=%q, outside the closed set rdns|asn|city", mf.GetName(), labels[1].GetValue())
				}
				switch labels[0].GetValue() {
				case "ok", "miss", "error":
				default:
					t.Errorf("%s has result=%q, outside the closed set ok|miss|error", mf.GetName(), labels[0].GetValue())
				}
			}
		}
	}
	for _, name := range []string{
		"kconmon_ng_console_enrichment_cache_total",
		"kconmon_ng_console_enrichment_lookups_total",
	} {
		if !present[name] {
			t.Errorf("metric %q was not registered", name)
		}
	}
}
