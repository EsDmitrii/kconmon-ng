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
	dto "github.com/prometheus/client_model/go"
)

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
	a, err := New(config.DefaultConfig())
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
	cfg := config.DefaultConfig()
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

	bad := config.DefaultConfig()
	bad.Checkers.External.Enabled = true
	bad.Checkers.External.AllowedCIDRs = []string{"not-a-cidr"}
	if _, badErr := New(bad); badErr == nil {
		t.Error("New must fail on a malformed allowed CIDR rather than start with a partial allowlist")
	}
}

// The continuous external checker exists only for an opted-in agent: that is
// what decides whether it subscribes to WatchExternalChecks at all.
func TestNewAgentExternalCheckerGatedOnEnabled(t *testing.T) {
	if a, err := New(config.DefaultConfig()); err != nil {
		t.Fatalf("New with default config failed: %v", err)
	} else if a.externalChecker != nil {
		t.Error("an opted-out agent must not build a continuous external checker")
	}

	cfg := config.DefaultConfig()
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
	cfg := config.DefaultConfig()
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
		"kconmon_ng_external_rtt_seconds{source_node=node-a,source_zone=zone-a,target=ping-host,target_kind=host,}",
		"kconmon_ng_external_packet_loss_ratio{source_node=node-a,source_zone=zone-a,target=ping-host,target_kind=host,}",
		"kconmon_ng_external_http_status_code{source_node=node-a,source_zone=zone-a,target=portal,target_kind=url,}",
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
