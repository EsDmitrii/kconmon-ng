package agent

import (
	"context"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

// testNewConfig is DefaultConfig plus a resolvable advertise address: New now
// fails hard when it cannot determine one (no config, no pod env, no
// controller route), which is the M6-1 contract, not an accident.
func testNewConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Agent.AdvertiseAddress = "127.0.0.1"
	return cfg
}

type mockDeregisterer struct {
	called    bool
	gotCtx    context.Context
	returnErr error
}

func (m *mockDeregisterer) Deregister(ctx context.Context) error {
	m.called = true
	m.gotCtx = ctx
	return m.returnErr
}

func TestGracefulDeregisterCallsClient(t *testing.T) {
	a := &Agent{}
	m := &mockDeregisterer{}

	a.gracefulDeregister(m)

	if !m.called {
		t.Fatal("expected Deregister to be called on shutdown")
	}
	if _, ok := m.gotCtx.Deadline(); !ok {
		t.Error("expected Deregister to be called with a bounded (timeout) context")
	}
}

func TestGracefulDeregisterDoesNotBlockOnError(t *testing.T) {
	a := &Agent{}
	m := &mockDeregisterer{returnErr: errors.New("controller unreachable")}

	done := make(chan struct{})
	go func() {
		a.gracefulDeregister(m)
		close(done)
	}()

	select {
	case <-done:
		// A failing Deregister must not block or panic the shutdown path.
	case <-time.After(3 * time.Second):
		t.Fatal("gracefulDeregister blocked on Deregister error")
	}

	if !m.called {
		t.Fatal("expected Deregister to be attempted even though it fails")
	}
}

// TestAgentCapabilitiesGatedOnExternalEnabled pins the advertisement that makes an opted-out agent
// invisible to the controller's external dispatch path.
func TestAgentCapabilitiesGatedOnExternalEnabled(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		want    []string
	}{
		{name: "external disabled advertises nothing", enabled: false, want: []string{}},
		{name: "external enabled advertises external-checks", enabled: true, want: []string{"external-checks"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Checkers.External.Enabled = tc.enabled

			got := agentCapabilities(cfg)
			if got == nil {
				t.Fatal("agentCapabilities returned nil; an empty slice is required so the JSON stays an array")
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("agentCapabilities(external.enabled=%v) = %v, want %v", tc.enabled, got, tc.want)
			}
		})
	}
}

// A default-configured agent advertises nothing: opting in is the operator's
// deliberate act, never a build-time default.
func TestNewAgentDefaultConfigAdvertisesNoExternalCapability(t *testing.T) {
	a, err := New(testNewConfig())
	if err != nil {
		t.Fatalf("New with default config failed: %v", err)
	}
	if slices.Contains(a.info.Capabilities, "external-checks") {
		t.Errorf("a default agent must not advertise external-checks, got %v", a.info.Capabilities)
	}
	if a.external.Enabled {
		t.Error("a default agent must have a closed external gate")
	}
}

// Enabling the feature builds the enforcing allowlist at startup, so a
// misconfiguration cannot wait until the first probe to surface.
func TestNewAgentExternalEnabledBuildsAllowlistAndFailsOnBadCIDR(t *testing.T) {
	cfg := testNewConfig()
	cfg.Checkers.External.Enabled = true
	cfg.Checkers.External.AllowedCIDRs = []string{"10.0.0.0/8"}

	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New with a valid external block failed: %v", err)
	}
	if !a.external.Enabled || a.external.Allowlist == nil || a.external.Resolver == nil {
		t.Fatalf("external gate not wired: %+v", a.external)
	}
	if !slices.Contains(a.info.Capabilities, "external-checks") {
		t.Errorf("an opted-in agent must advertise external-checks, got %v", a.info.Capabilities)
	}

	bad := testNewConfig()
	bad.Checkers.External.Enabled = true
	bad.Checkers.External.AllowedCIDRs = []string{"not-a-cidr"}
	if _, badErr := New(bad); badErr == nil {
		t.Error("New must fail on a malformed allowed CIDR rather than start with a partial allowlist")
	}
}

// The continuous external checker exists only for an opted-in agent: that is
// what decides whether it subscribes to WatchExternalChecks at all.
func TestNewAgentExternalCheckerGatedOnEnabled(t *testing.T) {
	if a, err := New(testNewConfig()); err != nil {
		t.Fatalf("New with default config failed: %v", err)
	} else if a.externalChecker != nil {
		t.Error("an opted-out agent must not build a continuous external checker")
	}

	cfg := testNewConfig()
	cfg.Checkers.External.Enabled = true
	cfg.Checkers.External.AllowedCIDRs = []string{"10.0.0.0/8"}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New with external enabled failed: %v", err)
	}
	if a.externalChecker == nil {
		t.Fatal("an opted-in agent must build a continuous external checker")
	}
	if a.externalChecker.SpecCount() != 0 {
		t.Error("the checker must start empty: nothing is probed until the controller pushes an assignment")
	}
}

// One malformed spec must not take the rest of the assignment down with it, and
// check types the controller already rejects must be refused here too.
func TestApplyExternalAssignmentDropsInvalidSpecsAndKeepsTheRest(t *testing.T) {
	cfg := testNewConfig()
	cfg.Checkers.External.Enabled = true
	cfg.Checkers.External.AllowedCIDRs = []string{"10.0.0.0/8"}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	a.applyExternalAssignment(&pb.ExternalCheckAssignment{Specs: []*pb.ExternalCheckSpec{
		{
			DefinitionId: "ok-tcp",
			Target:       &pb.ExternalTarget{Name: "good", Kind: "host", Address: "10.1.2.3", Port: 443},
			CheckType:    "tcp",
			IntervalNs:   int64(30 * time.Second),
			TimeoutNs:    int64(2 * time.Second),
		},
		{
			// mtr never reaches an agent (the controller rejects it at the PUT),
			// but a continuous internet traceroute is exactly the thing that must
			// fail closed rather than be trusted.
			DefinitionId: "bad-mtr",
			Target:       &pb.ExternalTarget{Name: "trace", Address: "10.1.2.4"},
			CheckType:    "mtr",
			IntervalNs:   int64(30 * time.Second),
		},
		{
			DefinitionId: "bad-dns",
			Target:       &pb.ExternalTarget{Name: "no-query", Address: "10.1.2.5"},
			CheckType:    "dns",
			IntervalNs:   int64(30 * time.Second),
		},
		{
			DefinitionId: "ok-http",
			Target:       &pb.ExternalTarget{Name: "portal", Kind: "url", Address: "https://example.com/health"},
			CheckType:    "http",
			IntervalNs:   int64(60 * time.Second),
			ParamsJson:   []byte(`{"insecureSkipVerify":false,"method":"GET","expectStatus":200}`),
		},
	}})

	if got := a.externalChecker.SpecCount(); got != 2 {
		t.Fatalf("expected the 2 valid specs to survive, got %d", got)
	}
	counts := a.externalChecker.Counts()
	if len(counts) != 2 || counts[0].Name != "good" || counts[1].Name != "portal" {
		t.Errorf("surviving specs wrong: %+v", counts)
	}

	// An empty assignment converges: it is how the controller expresses a
	// deletion, so it must clear the list rather than be ignored.
	a.applyExternalAssignment(&pb.ExternalCheckAssignment{})
	if got := a.externalChecker.SpecCount(); got != 0 {
		t.Fatalf("an empty assignment must clear the target list, %d specs left", got)
	}
}

// --- kconmon_ng_external_* metric family ------------------------------------

// exposition renders a registry into a flat, searchable blob of family names and LABEL VALUES;
// hand-rolled rather than expfmt on purpose.
func exposition(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering registry: %v", err)
	}
	var b strings.Builder
	for _, f := range families {
		for _, mtr := range f.GetMetric() {
			b.WriteString(f.GetName())
			b.WriteByte('{')
			for _, lp := range mtr.GetLabel() {
				b.WriteString(lp.GetName())
				b.WriteByte('=')
				b.WriteString(lp.GetValue())
				b.WriteByte(',')
			}
			b.WriteString("}\n")
		}
	}
	return b.String()
}

// labelValues collects every value carried under label name for one family.
func labelValues(t *testing.T, reg *prometheus.Registry, family, label string) []string {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering registry: %v", err)
	}
	var out []string
	for _, f := range families {
		if f.GetName() != family {
			continue
		}
		for _, mtr := range f.GetMetric() {
			for _, lp := range mtr.GetLabel() {
				if lp.GetName() == label {
					out = append(out, lp.GetValue())
				}
			}
		}
	}
	return out
}

func newTestRegistry(t *testing.T) (*prometheus.Registry, ResultHandler) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m := metrics.NewPrometheusMetrics("kconmon_ng", reg)
	return reg, NewResultHandler(m, checker.Target{NodeName: "node-a", Zone: "zone-a"})
}

// externalTCPResult drives a REAL ExternalChecker against a loopback listener and returns the
// CheckResult a scheduler would hand the result handler.
func externalTCPResult(t *testing.T, allowed []string) (res model.CheckResult, host, port string) {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening on loopback: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	host, port, err = net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("splitting listener address: %v", err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parsing listener port: %v", err)
	}

	allowlist, err := checker.NewAllowlist(allowed, nil)
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	spec, err := checker.ParseExternalSpec(&checker.ExternalSpecInput{
		DefinitionID: "def-1",
		Name:         "vendor-api",
		Address:      host,
		Port:         uint32(p), //nolint:gosec // G115: a listener port is always in range
		CheckType:    "tcp",
		Interval:     30 * time.Second,
		Timeout:      2 * time.Second,
	})
	if err != nil {
		t.Fatalf("ParseExternalSpec: %v", err)
	}

	c := checker.NewExternalChecker(allowlist, nil, time.Second)
	c.SetSpecs([]checker.ExternalSpec{spec})
	res = c.Check(context.Background(), checker.Target{})
	res.Source = "node-a"
	res.SourceZone = "zone-a"
	return res, host, port
}

// THE guard: the operator's target NAME is a label value, the destination
// ADDRESS is not, anywhere, ever.
func TestResultHandlerExternalExposesNameNeverAddress(t *testing.T) {
	res, host, port := externalTCPResult(t, []string{"127.0.0.0/8"})
	reg, handle := newTestRegistry(t)
	handle(res)

	text := exposition(t, reg)
	if !strings.Contains(text, "vendor-api") {
		t.Fatalf("target name missing from exposition:\n%s", text)
	}
	if strings.Contains(text, host) {
		t.Errorf("destination address %q leaked into a label:\n%s", host, text)
	}
	if strings.Contains(text, port) {
		t.Errorf("destination port %q leaked into a label:\n%s", port, text)
	}
	if strings.Contains(text, "def-1") {
		t.Errorf("definition id leaked into a label:\n%s", text)
	}
	for _, want := range []string{"kconmon_ng_external_duration_seconds", "kconmon_ng_external_results_total"} {
		if !strings.Contains(text, want) {
			t.Errorf("expected %s in exposition:\n%s", want, text)
		}
	}
}

func TestResultHandlerExternalResultLabelIsClosedSet(t *testing.T) {
	reg, handle := newTestRegistry(t)
	handle(model.CheckResult{
		Type: model.CheckExternal, Source: "node-a", SourceZone: "zone-a", Success: false,
		Details: []model.ExternalDetails{
			{Name: "ok-host", CheckType: model.CheckTCP, Success: true, Duration: 5 * time.Millisecond},
			{Name: "bad-host", CheckType: model.CheckICMP, Success: false, Duration: time.Second, Error: "timeout"},
			{Name: "portal", CheckType: model.CheckHTTP, Success: true, Duration: 20 * time.Millisecond, StatusCode: 200},
		},
	})

	got := labelValues(t, reg, "kconmon_ng_external_results_total", "result")
	if len(got) != 3 {
		t.Fatalf("expected 3 result series, got %v", got)
	}
	for _, v := range got {
		if v != "success" && v != "fail" {
			t.Errorf("result label %q outside the closed set {success, fail}", v)
		}
	}

	kinds := labelValues(t, reg, "kconmon_ng_external_results_total", "target_kind")
	for _, v := range kinds {
		if v != "host" && v != "url" {
			t.Errorf("target_kind %q outside the closed set {host, url}", v)
		}
	}
	if !slices.Contains(kinds, "url") {
		t.Errorf("an http target must report target_kind=url, got %v", kinds)
	}
}

func TestResultHandlerExternalPerCheckTypeFamilies(t *testing.T) {
	reg, handle := newTestRegistry(t)
	handle(model.CheckResult{
		Type: model.CheckExternal, Source: "node-a", SourceZone: "zone-a", Success: true,
		Details: []model.ExternalDetails{
			{Name: "ping-host", CheckType: model.CheckICMP, Success: true,
				Duration: 4 * time.Millisecond, RTT: 3 * time.Millisecond, LossRatio: 0.25},
			{Name: "portal", CheckType: model.CheckHTTP, Success: true,
				Duration: 20 * time.Millisecond, StatusCode: 503},
			{Name: "plain-tcp", CheckType: model.CheckTCP, Success: true, Duration: time.Millisecond},
		},
	})

	text := exposition(t, reg)
	for _, want := range []string{
		"kconmon_ng_external_rtt_seconds{check_type=icmp,source_node=node-a,source_zone=zone-a,target=ping-host,target_kind=host,}",
		"kconmon_ng_external_packet_loss_ratio{check_type=icmp,source_node=node-a,source_zone=zone-a,target=ping-host,target_kind=host,}",
		"kconmon_ng_external_http_status_code{check_type=http,source_node=node-a,source_zone=zone-a,target=portal,target_kind=url,}",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing series %s in:\n%s", want, text)
		}
	}
	// A tcp probe has neither an RTT nor a loss ratio; recording a zero would
	// invent a measurement that was never taken.
	if strings.Contains(text, "target=plain-tcp,target_kind=host,}\nkconmon_ng_external_packet_loss") {
		t.Error("tcp probe must not report a packet loss ratio")
	}
	for _, v := range labelValues(t, reg, "kconmon_ng_external_packet_loss_ratio", "target") {
		if v != "ping-host" {
			t.Errorf("packet_loss_ratio is icmp-only, got a series for %q", v)
		}
	}
	for _, v := range labelValues(t, reg, "kconmon_ng_external_http_status_code", "target") {
		if v != "portal" {
			t.Errorf("http_status_code is http-only, got a series for %q", v)
		}
	}
}

// A denial never reached the network: it is not a success and not a failure, so
// it must land on denied_total and leave results_total alone.
func TestResultHandlerExternalDeniedCountsReasonNotResult(t *testing.T) {
	// Loopback is deliberately NOT in the allowlist, so the real checker denies.
	res, _, _ := externalTCPResult(t, []string{"10.0.0.0/8"})
	reg, handle := newTestRegistry(t)
	handle(res)

	text := exposition(t, reg)
	if strings.Contains(text, "kconmon_ng_external_results_total") {
		t.Errorf("a denied probe must not increment results_total:\n%s", text)
	}
	if strings.Contains(text, "kconmon_ng_external_duration_seconds") {
		t.Errorf("a denied probe never ran, so it has no duration:\n%s", text)
	}
	reasons := labelValues(t, reg, "kconmon_ng_external_denied_total", "reason")
	if len(reasons) != 1 || reasons[0] != string(model.ExternalDenyCIDR) {
		t.Fatalf("reason labels = %v, want [cidr]", reasons)
	}
}

func TestResultHandlerExternalDeniedReasonIsAClosedSet(t *testing.T) {
	reg, handle := newTestRegistry(t)
	handle(model.CheckResult{
		Type: model.CheckExternal, Source: "node-a", SourceZone: "zone-a", Success: false,
		Details: []model.ExternalDetails{
			{Name: "a", CheckType: model.CheckTCP, Denied: true, DenyReason: model.ExternalDenyCIDR},
			{Name: "b", CheckType: model.CheckDNS, Denied: true, DenyReason: model.ExternalDenyResolve},
			{Name: "c", CheckType: model.CheckHTTP, Denied: true, DenyReason: model.ExternalDenyDisabled},
			// A denial that somehow arrives without a typed reason must still land
			// in the closed set rather than mint a new label value.
			{Name: "d", CheckType: model.CheckICMP, Denied: true},
		},
	})

	allowed := map[string]bool{"cidr": true, "resolve": true, "disabled": true}
	reasons := labelValues(t, reg, "kconmon_ng_external_denied_total", "reason")
	if len(reasons) != 4 {
		t.Fatalf("expected 4 denied series, got %v", reasons)
	}
	for _, r := range reasons {
		if !allowed[r] {
			t.Errorf("reason %q outside the closed set {cidr, resolve, disabled}", r)
		}
	}
}

// An agent with the feature off must expose a byte-identical default
// exposition: not one kconmon_ng_external_* line.
func TestResultHandlerFeatureDisabledExposesNoExternalSeries(t *testing.T) {
	reg, handle := newTestRegistry(t)
	handle(model.CheckResult{
		Type: model.CheckTCP, Source: "node-a", Destination: "node-b",
		SourceZone: "zone-a", DestZone: "zone-b", Success: true,
		Details: &model.TCPDetails{ConnectTime: time.Millisecond, TotalTime: 2 * time.Millisecond},
	})

	for _, line := range strings.Split(exposition(t, reg), "\n") {
		if strings.Contains(line, "_external_") {
			t.Errorf("external series exposed with the feature unused: %s", line)
		}
	}
}

// --- ICMP packet loss gauge -------------------------------------------------

// The one pair every ICMP pin below drives. Fixed rather than threaded through
// the helpers: what the pins are about is the VALUE on the series, not which
// pair carries it.
const (
	icmpSrcNode = "node-a"
	icmpDstNode = "node-b"
)

// icmpLoss reads the kconmon_ng_icmp_packet_loss_ratio sample for that pair. The
// bool separates "no series at all" from "a series reading 0.0": those are
// different states and only one of them is the bug pinned below.
func icmpLoss(t *testing.T, reg *prometheus.Registry) (float64, bool) {
	t.Helper()
	for _, mtr := range icmpPairSamples(t, reg, "kconmon_ng_icmp_packet_loss_ratio") {
		return mtr.GetGauge().GetValue(), true
	}
	return 0, false
}

// icmpFails reads kconmon_ng_icmp_results_total{result="fail"} for that pair.
func icmpFails(t *testing.T, reg *prometheus.Registry) (float64, bool) {
	t.Helper()
	for _, mtr := range icmpPairSamples(t, reg, "kconmon_ng_icmp_results_total") {
		for _, lp := range mtr.GetLabel() {
			if lp.GetName() == "result" && lp.GetValue() == "fail" {
				return mtr.GetCounter().GetValue(), true
			}
		}
	}
	return 0, false
}

// icmpPairSamples collects every sample of one family carrying the pin's pair.
func icmpPairSamples(t *testing.T, reg *prometheus.Registry, family string) []*dto.Metric {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering registry: %v", err)
	}
	var out []*dto.Metric
	for _, f := range families {
		if f.GetName() != family {
			continue
		}
		for _, mtr := range f.GetMetric() {
			var gotSrc, gotDst string
			for _, lp := range mtr.GetLabel() {
				switch lp.GetName() {
				case "source_node":
					gotSrc = lp.GetValue()
				case "destination_node":
					gotDst = lp.GetValue()
				}
			}
			if gotSrc == icmpSrcNode && gotDst == icmpDstNode {
				out = append(out, mtr)
			}
		}
	}
	return out
}

// THE pin for a total ICMP outage.
func TestResultHandlerICMPFailureWithoutDetailsReportsTotalLoss(t *testing.T) {
	reg, handle := newTestRegistry(t)

	// A healthy probe first: this is the 0.0 the bug used to freeze on the pair.
	handle(model.CheckResult{
		Type: model.CheckICMP, Source: "node-a", Destination: "node-b",
		SourceZone: "zone-a", DestZone: "zone-b", Success: true,
		Details: &model.ICMPDetails{RTT: 2 * time.Millisecond, LossRatio: 0},
	})
	if got, ok := icmpLoss(t, reg); !ok || got != 0 {
		t.Fatalf("healthy probe must record loss 0.0, got %v (present=%v)", got, ok)
	}

	// The peer goes away. This is what the checker hands back on every error
	// path that is not the read deadline.
	handle(model.CheckResult{
		Type: model.CheckICMP, Source: "node-a", Destination: "node-b",
		SourceZone: "zone-a", DestZone: "zone-b", Success: false,
		Error: "ICMP write: sendto: network is unreachable",
	})

	got, ok := icmpLoss(t, reg)
	if !ok {
		t.Fatal("the pair lost its icmp_packet_loss_ratio series entirely")
	}
	if got != 1 {
		t.Errorf("a peer that answered nothing must read loss ratio 1.0, got %v", got)
	}

	// The counter half of the report was already correct; keep it that way.
	if c, ok := icmpFails(t, reg); !ok || c != 1 {
		t.Errorf("icmp_results_total{result=fail} = %v (present=%v), want 1", c, ok)
	}
}

// A probe that never got a reply has no round-trip time, so the failed attempt must not land in the
// RTT histogram.
func TestResultHandlerICMPTimeoutRecordsLossNotLatency(t *testing.T) {
	reg, handle := newTestRegistry(t)
	handle(model.CheckResult{
		Type: model.CheckICMP, Source: "node-a", Destination: "node-b",
		SourceZone: "zone-a", DestZone: "zone-b", Success: false,
		Error:   "ICMP read: i/o timeout",
		Details: &model.ICMPDetails{RTT: 2 * time.Second, LossRatio: 1.0},
	})

	if got, ok := icmpLoss(t, reg); !ok || got != 1 {
		t.Errorf("a timed-out probe must read loss ratio 1.0, got %v (present=%v)", got, ok)
	}
	if strings.Contains(exposition(t, reg), "kconmon_ng_icmp_rtt_seconds") {
		t.Error("a timed-out probe published its timeout as an RTT observation")
	}
}

// The same guard driven through the REAL ICMPChecker against RFC 5737 TEST-NET-1, which cannot
// answer.
func TestResultHandlerICMPUnreachableTargetReportsTotalLoss(t *testing.T) {
	reg, handle := newTestRegistry(t)

	c := checker.NewICMPChecker(300 * time.Millisecond)
	res := c.Check(context.Background(), checker.Target{PodIP: "192.0.2.1"})
	if res.Success {
		t.Fatal("192.0.2.1 (TEST-NET-1) answered an echo request; the pin is meaningless here")
	}
	res.Source, res.Destination = "node-a", "node-b"
	res.SourceZone, res.DestZone = "zone-a", "zone-b"
	handle(res)

	got, ok := icmpLoss(t, reg)
	if !ok {
		t.Fatalf("no loss series for an unreachable peer (checker error: %s)", res.Error)
	}
	if got != 1 {
		t.Errorf("unreachable peer read loss %v, want 1.0 (checker error: %s)", got, res.Error)
	}
}

// TestPreinitPeerResultsCoversEveryEnabledPeerChecker is the null-vs-zero regression: a pair that
// has never failed had no `result="fail"` series at all, so the console matrix rendered null. TCP
// only looked initialized because every pair had genuinely failed at some point.
/*
A re-registration must not re-assert a zone this agent merely ADOPTED.

The registry consults its ZoneResolver only when the agent supplies no zone, so an agent that echoes
back the zone it learned last time wins over the node's own failure-domain label. Relabel the node,
the informer corrects the registry, and the next re-registration puts the stale value back — for
good, because every registration after that re-asserts it. Cross-zone matrix views and zone-scoped
alerts read that field.

What the agent asserts is what an operator CONFIGURED, and nothing else.
*/
func TestRegistrationAssertsOnlyTheConfiguredZone(t *testing.T) {
	// No agent.zone: the agent has adopted "zone-from-node" from a previous registration.
	adopted := &Agent{configuredZone: "", info: model.AgentInfo{ID: "a1", NodeName: "node-a", Zone: "zone-from-node"}}
	if got := adopted.registrationInfo().Zone; got != "" {
		t.Errorf("registration zone = %q, want empty: an adopted zone must not be re-asserted, "+
			"or it overrides the node label the controller resolved it from", got)
	}
	// Everything else about the agent still travels.
	if got := adopted.registrationInfo().NodeName; got != "node-a" {
		t.Errorf("registration nodeName = %q, want node-a", got)
	}
	// And the agent's own effective zone is untouched — it is what labels its metrics.
	if adopted.info.Zone != "zone-from-node" {
		t.Errorf("registrationInfo mutated the agent's effective zone: %q", adopted.info.Zone)
	}

	// WITH agent.zone: the override is an assertion, and it travels.
	configured := &Agent{configuredZone: "zone-override", info: model.AgentInfo{ID: "a2", NodeName: "node-b", Zone: "zone-override"}}
	if got := configured.registrationInfo().Zone; got != "zone-override" {
		t.Errorf("registration zone = %q, want the configured zone-override", got)
	}
}

func TestPreinitPeerResultsCoversEveryEnabledPeerChecker(t *testing.T) {
	m := metrics.NewPrometheusMetrics("test_preinit", prometheus.NewRegistry())
	source := checker.Target{NodeName: "node-1", Zone: "zone-a"}
	peers := []checker.Target{
		{NodeName: "node-2", Zone: "zone-a"},
		{NodeName: "node-3", Zone: "zone-b"},
		{NodeName: "node-4", Zone: "zone-b"},
	}
	enabled := map[model.CheckType]checker.Checker{
		model.CheckTCP: nil,
		model.CheckUDP: nil,
	}

	preinitPeerResults(m, source, peers, enabled)

	// Three peers, two outcomes each.
	if got := testutil.CollectAndCount(m.TCPResults); got != 6 {
		t.Errorf("tcp_results_total series = %d, want 6", got)
	}
	if got := testutil.CollectAndCount(m.UDPResults); got != 6 {
		t.Errorf("udp_results_total series = %d, want 6; UDP must be initialized exactly like TCP", got)
	}
	if got := testutil.CollectAndCount(m.ICMPResults); got != 0 {
		t.Errorf("icmp_results_total series = %d, want 0 when the icmp checker is disabled", got)
	}
}

// TestPreinitPeerResultsStartsAtZeroAndDoesNotDoubleCount pins that initialization is not an
// observation: the series must exist reading 0, and repeating it must not move a counter.
func TestPreinitPeerResultsStartsAtZeroAndDoesNotDoubleCount(t *testing.T) {
	m := metrics.NewPrometheusMetrics("test_preinit_zero", prometheus.NewRegistry())
	source := checker.Target{NodeName: "node-1", Zone: "zone-a"}
	peers := []checker.Target{{NodeName: "node-2", Zone: "zone-a"}}
	enabled := map[model.CheckType]checker.Checker{model.CheckUDP: nil}

	preinitPeerResults(m, source, peers, enabled)

	failCounter := m.UDPResults.WithLabelValues("node-1", "node-2", "zone-a", "zone-a", "fail")
	if got := testutil.ToFloat64(failCounter); got != 0 {
		t.Fatalf("preinitialized fail counter = %v, want 0", got)
	}

	failCounter.Inc()
	preinitPeerResults(m, source, peers, enabled)
	if got := testutil.ToFloat64(failCounter); got != 1 {
		t.Errorf("fail counter = %v after a re-preinit, want 1; initialization must be idempotent", got)
	}
}

/*
 * Two checks on ONE target must not share a series.
 *
 * target_kind is derived from the check type and everything that is not http collapses to "host", so
 * an icmp check and a tcp check on the same target wrote the same series: their successes and
 * failures were averaged together, and the ExternalChecksFailing rule -- which sums by exactly those
 * labels -- stayed quiet while one of them failed every probe, diluted by the other.
 */
func TestResultHandlerExternalSeparatesChecksOnTheSameTarget(t *testing.T) {
	reg, handle := newTestRegistry(t)
	handle(model.CheckResult{
		Type: model.CheckExternal, Source: "node-a", SourceZone: "zone-a", Success: false,
		Details: []model.ExternalDetails{
			{Name: "edge", CheckType: model.CheckICMP, Success: true, Duration: 4 * time.Millisecond},
			{Name: "edge", CheckType: model.CheckTCP, Success: false, Duration: 9 * time.Millisecond},
		},
	})

	text := exposition(t, reg)
	for _, want := range []string{
		`kconmon_ng_external_results_total{check_type=icmp,result=success,source_node=node-a,source_zone=zone-a,target=edge,target_kind=host,}`,
		`kconmon_ng_external_results_total{check_type=tcp,result=fail,source_node=node-a,source_zone=zone-a,target=edge,target_kind=host,}`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing series %s in:\n%s", want, text)
		}
	}
}

/*
 * A probe abandoned before it touched the network is not a result.
 *
 * Shutdown fills one of these for every target still waiting on the concurrency semaphore. Recording
 * it observed a 0 s sample into the duration histogram, incremented a counter whose Help says
 * "results that reached the network", and published 100% packet loss for an icmp probe that sent no
 * packets -- once per target, on every rolling update.
 */
func TestResultHandlerExternalIgnoresAProbeThatNeverRan(t *testing.T) {
	reg, handle := newTestRegistry(t)
	handle(model.CheckResult{
		Type: model.CheckExternal, Source: "node-a", SourceZone: "zone-a", Success: false,
		Details: []model.ExternalDetails{
			{Name: "edge", CheckType: model.CheckICMP, NotRun: true, Error: "external probe cancelled"},
		},
	})

	text := exposition(t, reg)
	for _, unwanted := range []string{
		"kconmon_ng_external_results_total",
		"kconmon_ng_external_duration_seconds",
		"kconmon_ng_external_packet_loss_ratio",
	} {
		if strings.Contains(text, unwanted) {
			t.Errorf("%s was written for a probe that never reached the network:\n%s", unwanted, text)
		}
	}
}

// zoneHistSampleCount reads the total observation count of one zone histogram family, summed over
// its series; -1 means the family is not exposed at all.
func zoneHistSampleCount(t *testing.T, reg *prometheus.Registry, name string) int {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering registry: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		total := 0
		for _, mtr := range f.GetMetric() {
			total += int(mtr.GetHistogram().GetSampleCount()) //nolint:gosec // test data, tiny counts
		}
		return total
	}
	return -1
}

// newZoneTestHandler is newTestRegistry with the metrics struct exposed, so zone counters can be
// read back directly.
func newZoneTestHandler(t *testing.T, sourceZone string) (*prometheus.Registry, *metrics.PrometheusMetrics, ResultHandler) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m := metrics.NewPrometheusMetrics("kconmon_ng", reg)
	return reg, m, NewResultHandler(m, checker.Target{NodeName: "node-a", Zone: sourceZone})
}

// One TCP probe writes TWICE: the per-pair family and the zone family, same buckets, same outcome.
func TestResultHandlerTCPWritesZoneFamily(t *testing.T) {
	reg, m, handle := newZoneTestHandler(t, "zone-a")
	handle(model.CheckResult{
		Type: model.CheckTCP, Source: "node-a", Destination: "node-b",
		SourceZone: "zone-a", DestZone: "zone-b", Success: true,
		Details: &model.TCPDetails{ConnectTime: time.Millisecond, TotalTime: 2 * time.Millisecond},
	})
	handle(model.CheckResult{
		Type: model.CheckTCP, Source: "node-a", Destination: "node-c",
		SourceZone: "zone-a", DestZone: "zone-b", Success: false,
	})

	if got := zoneHistSampleCount(t, reg, "kconmon_ng_zone_tcp_connect_seconds"); got != 1 {
		t.Errorf("zone tcp connect observations = %d, want 1 (the failed probe has no timings)", got)
	}
	if got := zoneHistSampleCount(t, reg, "kconmon_ng_zone_tcp_total_seconds"); got != 1 {
		t.Errorf("zone tcp total observations = %d, want 1", got)
	}
	// Two pairs, one zone pair: the zone counter aggregates what the per-pair counters split.
	if got := testutil.ToFloat64(m.ZoneTCPResults.WithLabelValues("zone-a", "zone-b", "success")); got != 1 {
		t.Errorf("zone tcp success = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.ZoneTCPResults.WithLabelValues("zone-a", "zone-b", "fail")); got != 1 {
		t.Errorf("zone tcp fail = %v, want 1", got)
	}
	// The per-pair family still gets its write: the zone family is a second write, not a move.
	if got := testutil.ToFloat64(m.TCPResults.WithLabelValues("node-a", "node-b", "zone-a", "zone-b", "success")); got != 1 {
		t.Errorf("per-pair tcp success = %v, want 1", got)
	}
}

/*
Zone UDP loss is COUNTERS, never a ratio gauge: sum(rate(received))/sum(rate(sent)) weights every
probe by its packets, while averaging per-pair ratio gauges weights every pair equally — one dead
pair among ten healthy ones reads 9% loss regardless of traffic. The roadmap declined the gauge.
*/
func TestResultHandlerZoneUDPPacketCounters(t *testing.T) {
	reg, m, handle := newZoneTestHandler(t, "zone-a")

	// Total blackout: 5 sent, nothing back, no RTT was measured so none may be observed.
	handle(model.CheckResult{
		Type: model.CheckUDP, Source: "node-a", Destination: "node-b",
		SourceZone: "zone-a", DestZone: "zone-b", Success: false,
		Details: &model.UDPDetails{PacketsSent: 5, PacketsRecv: 0, LossRatio: 1},
	})
	if got := zoneHistSampleCount(t, reg, "kconmon_ng_zone_udp_rtt_seconds"); got != -1 {
		t.Errorf("a probe with zero replies observed %d zone RTT samples, want none", got)
	}

	// Partial loss: counters accumulate the real packet counts.
	handle(model.CheckResult{
		Type: model.CheckUDP, Source: "node-a", Destination: "node-b",
		SourceZone: "zone-a", DestZone: "zone-b", Success: true,
		Details: &model.UDPDetails{PacketsSent: 5, PacketsRecv: 3, LossRatio: 0.4, MeanRTT: time.Millisecond},
	})

	if got := testutil.ToFloat64(m.ZoneUDPPacketsSent.WithLabelValues("zone-a", "zone-b")); got != 10 {
		t.Errorf("zone udp packets sent = %v, want 10", got)
	}
	if got := testutil.ToFloat64(m.ZoneUDPPacketsReceived.WithLabelValues("zone-a", "zone-b")); got != 3 {
		t.Errorf("zone udp packets received = %v, want 3", got)
	}
	if got := zoneHistSampleCount(t, reg, "kconmon_ng_zone_udp_rtt_seconds"); got != 1 {
		t.Errorf("zone udp rtt observations = %d, want 1", got)
	}
	if got := testutil.ToFloat64(m.ZoneUDPResults.WithLabelValues("zone-a", "zone-b", "fail")); got != 1 {
		t.Errorf("zone udp fail = %v, want 1", got)
	}
	if strings.Contains(exposition(t, reg), "zone_udp_packet_loss_ratio") {
		t.Error("a zone loss ratio gauge exists; loss must be derivable from counters only")
	}
}

/*
ICMPDetails carries no packet counts, but the checker sends exactly ONE echo per probe and attaches
Details only after the request went on the wire — so sent/received are derivable per result:
Details present = 1 sent, success = 1 received. A probe that died before the write (bad IP, listen
or marshal error) put nothing on the wire and counts nothing.
*/
func TestResultHandlerZoneICMPPacketsSingleEcho(t *testing.T) {
	reg, m, handle := newZoneTestHandler(t, "zone-a")

	// Echo out, reply back.
	handle(model.CheckResult{
		Type: model.CheckICMP, Source: "node-a", Destination: "node-b",
		SourceZone: "zone-a", DestZone: "zone-b", Success: true,
		Details: &model.ICMPDetails{RTT: 2 * time.Millisecond, LossRatio: 0},
	})
	// Echo out, read deadline hit: sent, not received, and the timeout is NOT an RTT.
	handle(model.CheckResult{
		Type: model.CheckICMP, Source: "node-a", Destination: "node-b",
		SourceZone: "zone-a", DestZone: "zone-b", Success: false,
		Error:   "ICMP read: i/o timeout",
		Details: &model.ICMPDetails{RTT: 2 * time.Second, LossRatio: 1},
	})
	// Died before the write: no Details, nothing on the wire, nothing counted.
	handle(model.CheckResult{
		Type: model.CheckICMP, Source: "node-a", Destination: "node-b",
		SourceZone: "zone-a", DestZone: "zone-b", Success: false,
		Error: "ICMP write: sendto: network is unreachable",
	})

	if got := testutil.ToFloat64(m.ZoneICMPPacketsSent.WithLabelValues("zone-a", "zone-b")); got != 2 {
		t.Errorf("zone icmp packets sent = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.ZoneICMPPacketsReceived.WithLabelValues("zone-a", "zone-b")); got != 1 {
		t.Errorf("zone icmp packets received = %v, want 1", got)
	}
	if got := zoneHistSampleCount(t, reg, "kconmon_ng_zone_icmp_rtt_seconds"); got != 1 {
		t.Errorf("zone icmp rtt observations = %d, want 1: only the answered echo has a round trip", got)
	}
	// All three probes are results, whatever happened to their packets.
	if got := testutil.ToFloat64(m.ZoneICMPResults.WithLabelValues("zone-a", "zone-b", "fail")); got != 2 {
		t.Errorf("zone icmp fail = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.ZoneICMPResults.WithLabelValues("zone-a", "zone-b", "success")); got != 1 {
		t.Errorf("zone icmp success = %v, want 1", got)
	}
}

// An agent without a zone labels its per-pair series with source_zone="" — the zone family mirrors
// that verbatim rather than minting a placeholder the per-pair family does not use.
func TestResultHandlerZoneFamilyKeepsEmptyZoneVerbatim(t *testing.T) {
	_, m, handle := newZoneTestHandler(t, "")
	handle(model.CheckResult{
		Type: model.CheckTCP, Source: "node-a", Destination: "node-b",
		SourceZone: "", DestZone: "", Success: true,
	})
	if got := testutil.ToFloat64(m.ZoneTCPResults.WithLabelValues("", "", "success")); got != 1 {
		t.Errorf("zone tcp success with empty zones = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.TCPResults.WithLabelValues("node-a", "node-b", "", "", "success")); got != 1 {
		t.Errorf("per-pair tcp success with empty zones = %v, want 1", got)
	}
}

// Zone series are keyed per zone PAIR, not per peer: three peers in two zones preinitialize two
// zone pairs, each with both outcomes and — for the loss-capable types — both packet counters at 0.
func TestPreinitZoneResultsCreatesSeriesPerZonePair(t *testing.T) {
	m := metrics.NewPrometheusMetrics("test_zone_preinit", prometheus.NewRegistry())
	source := checker.Target{NodeName: "node-1", Zone: "zone-a"}
	peers := []checker.Target{
		{NodeName: "node-2", Zone: "zone-a"},
		{NodeName: "node-3", Zone: "zone-b"},
		{NodeName: "node-4", Zone: "zone-b"},
	}
	enabled := map[model.CheckType]checker.Checker{
		model.CheckTCP:  nil,
		model.CheckUDP:  nil,
		model.CheckICMP: nil,
	}

	preinitZoneResults(m, source, peers, enabled)

	// Two destination zones × two outcomes.
	for name, vec := range map[string]*prometheus.CounterVec{
		"zone_tcp_results_total":  m.ZoneTCPResults,
		"zone_udp_results_total":  m.ZoneUDPResults,
		"zone_icmp_results_total": m.ZoneICMPResults,
	} {
		if got := testutil.CollectAndCount(vec); got != 4 {
			t.Errorf("%s series = %d, want 4 (2 zone pairs x 2 outcomes)", name, got)
		}
	}
	// Packet counters per zone pair, reading 0 — so loss expressions return data from scrape one.
	for name, vec := range map[string]*prometheus.CounterVec{
		"zone_udp_packets_sent_total":      m.ZoneUDPPacketsSent,
		"zone_udp_packets_received_total":  m.ZoneUDPPacketsReceived,
		"zone_icmp_packets_sent_total":     m.ZoneICMPPacketsSent,
		"zone_icmp_packets_received_total": m.ZoneICMPPacketsReceived,
	} {
		if got := testutil.CollectAndCount(vec); got != 2 {
			t.Errorf("%s series = %d, want 2 zone pairs", name, got)
		}
	}
	if got := testutil.ToFloat64(m.ZoneUDPPacketsSent.WithLabelValues("zone-a", "zone-b")); got != 0 {
		t.Errorf("preinitialized packets sent = %v, want 0", got)
	}

	// Idempotent: preinit is not an observation and repeating it moves nothing.
	m.ZoneUDPResults.WithLabelValues("zone-a", "zone-b", "fail").Inc()
	m.ZoneICMPPacketsSent.WithLabelValues("zone-a", "zone-b").Inc()
	preinitZoneResults(m, source, peers, enabled)
	if got := testutil.ToFloat64(m.ZoneUDPResults.WithLabelValues("zone-a", "zone-b", "fail")); got != 1 {
		t.Errorf("zone fail counter = %v after re-preinit, want 1", got)
	}
	if got := testutil.ToFloat64(m.ZoneICMPPacketsSent.WithLabelValues("zone-a", "zone-b")); got != 1 {
		t.Errorf("zone packets sent = %v after re-preinit, want 1", got)
	}
}

// A checker with no zone counters (dns, http) must not panic the preinit and must create nothing.
func TestPreinitZoneResultsIgnoresNonPeerCheckers(t *testing.T) {
	m := metrics.NewPrometheusMetrics("test_zone_preinit_skip", prometheus.NewRegistry())
	source := checker.Target{NodeName: "node-1", Zone: "zone-a"}
	peers := []checker.Target{{NodeName: "node-2", Zone: "zone-b"}}
	enabled := map[model.CheckType]checker.Checker{
		model.CheckDNS:  nil,
		model.CheckHTTP: nil,
	}
	preinitZoneResults(m, source, peers, enabled)
	if got := testutil.CollectAndCount(m.ZoneTCPResults); got != 0 {
		t.Errorf("dns/http preinit created %d zone tcp series", got)
	}
}

// markProbeIntended publishes the plan for THIS agent: one series at 1 per assigned peer,
// source_node always self. Full mesh is simply "every peer the controller sent", so the same
// function covers both topology modes without the agent knowing which one it lives under.
func TestMarkProbeIntendedCoversEveryAssignedPeer(t *testing.T) {
	m := metrics.NewPrometheusMetrics("kconmon_ng", prometheus.NewRegistry())
	source := checker.Target{NodeName: "node-a", Zone: "zone-a"}
	peers := []checker.Target{
		{NodeName: "node-b", Zone: "zone-a"},
		{NodeName: "node-c", Zone: "zone-b"},
	}

	markProbeIntended(m, source, peers)

	if got := testutil.CollectAndCount(m.ProbeIntended); got != 2 {
		t.Fatalf("probe_intended has %d series, want one per assigned peer (2)", got)
	}
	for _, peer := range []string{"node-b", "node-c"} {
		if got := testutil.ToFloat64(m.ProbeIntended.WithLabelValues("node-a", peer)); got != 1 {
			t.Errorf("probe_intended{source_node=node-a,destination_node=%s} = %v, want 1", peer, got)
		}
	}
}

// A peer-list change must leave the family describing exactly the NEW plan: series for peers no
// longer assigned are deleted, not left at a stale 1 — PairWentSilent reads this family as the
// plan, and a stale 1 would keep the alert armed for a pair nothing probes any more.
func TestSyncPeerMetricsRetiresStaleProbeIntended(t *testing.T) {
	m := metrics.NewPrometheusMetrics("kconmon_ng", prometheus.NewRegistry())
	// AgentIDs are distinct as in production: the scheduler's self-filter compares them, and an
	// all-empty test fleet would read every peer as self.
	a := &Agent{
		metrics:   m,
		scheduler: NewScheduler(checker.Target{AgentID: "id-a", NodeName: "node-a", Zone: "zone-a"}, nil),
		info:      model.AgentInfo{ID: "id-a", NodeName: "node-a", Zone: "zone-a"},
		checkers:  map[model.CheckType]checker.Checker{},
	}

	first := []checker.Target{
		{AgentID: "id-b", NodeName: "node-b", Zone: "zone-a"},
		{AgentID: "id-c", NodeName: "node-c", Zone: "zone-b"},
	}
	a.scheduler.UpdatePeers(first)
	a.syncPeerMetrics()
	if got := testutil.CollectAndCount(m.ProbeIntended); got != 2 {
		t.Fatalf("probe_intended has %d series after registration, want 2", got)
	}

	// The next plan drops node-c: same sequence the peer-update callback runs.
	next := []checker.Target{{AgentID: "id-b", NodeName: "node-b", Zone: "zone-a"}}
	a.forgetDepartedPeers(next)
	a.scheduler.UpdatePeers(next)
	a.syncPeerMetrics()

	if got := testutil.CollectAndCount(m.ProbeIntended); got != 1 {
		t.Errorf("probe_intended has %d series after the plan shrank, want 1", got)
	}
	if got := testutil.ToFloat64(m.ProbeIntended.WithLabelValues("node-a", "node-b")); got != 1 {
		t.Errorf("the still-assigned pair reads %v, want 1", got)
	}
}
