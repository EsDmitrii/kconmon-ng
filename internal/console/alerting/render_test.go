package alerting

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// defaultRenderer is the MetricPrefix renderer every pre-existing golden below was written against.
var defaultRenderer = NewRenderer("")

// Render — golden expressions, one per kind x representative params.

// TestRenderHonoursACustomMetricPrefix is the one golden that proves the prefix is CARRIED rather
// than baked in.
func TestRenderHonoursACustomMetricPrefix(t *testing.T) {
	r := NewRenderer("acme_net")
	if r.Prefix() != "acme_net" {
		t.Fatalf("Prefix() = %q, want acme_net", r.Prefix())
	}
	for _, tt := range []struct {
		name   string
		rule   Rule
		golden string
	}{
		{
			name:   "gauge family",
			rule:   Rule{Kind: KindPairLoss, Params: map[string]any{"protocol": "udp", "thresholdPercent": 5.0}},
			golden: `acme_net_udp_packet_loss_ratio * 100 > 5`,
		},
		{
			name:   "two families in one comparison",
			rule:   Rule{Kind: KindAgentMissing},
			golden: `acme_net_controller_registered_agents < acme_net_controller_expected_agents`,
		},
		{
			name: "family inside a histogram_quantile",
			rule: Rule{Kind: KindHTTPTTFB, Params: map[string]any{"thresholdMs": 500.0}},
			golden: `histogram_quantile(0.95, sum by (le, url, source_node, source_zone) ` +
				`(rate(acme_net_http_ttfb_seconds_bucket[5m]))) * 1000 > 500`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.Render(tt.rule)
			if err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			if got != tt.golden {
				t.Errorf("Render()\n got: %s\nwant: %s", got, tt.golden)
			}
			if strings.Contains(got, MetricPrefix) {
				t.Errorf("Render() leaked the default prefix %q into %s", MetricPrefix, got)
			}
		})
	}
}

// TestNewRendererFoldsAnEmptyPrefix pins the repair: a hand-built caller with
// no prefix gets the shipped default, never a family named "_udp_...".
func TestNewRendererFoldsAnEmptyPrefix(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\n"} {
		if got := NewRenderer(in).Prefix(); got != MetricPrefix {
			t.Errorf("NewRenderer(%q).Prefix() = %q, want %q", in, got, MetricPrefix)
		}
	}
}

func TestRenderGoldenExpressions(t *testing.T) {
	tests := []struct {
		name   string
		rule   Rule
		golden string
	}{
		{
			name: "pair-loss udp no scope",
			rule: Rule{Kind: KindPairLoss, Params: map[string]any{
				"protocol": "udp", "thresholdPercent": 5.0,
			}},
			golden: `kconmon_ng_udp_packet_loss_ratio * 100 > 5`,
		},
		{
			name: "pair-loss udp scope both ends",
			rule: Rule{Kind: KindPairLoss, Params: map[string]any{
				"protocol": "udp", "thresholdPercent": 0.5,
				"scope": map[string]any{"sourceNode": "node-a", "destNode": "node-b"},
			}},
			golden: `kconmon_ng_udp_packet_loss_ratio{source_node="node-a",destination_node="node-b"} * 100 > 0.5`,
		},
		{
			name: "pair-loss icmp source only",
			rule: Rule{Kind: KindPairLoss, Params: map[string]any{
				"protocol": "icmp", "thresholdPercent": 25.0,
				"scope": map[string]any{"sourceNode": "node-a"},
			}},
			golden: `kconmon_ng_icmp_packet_loss_ratio{source_node="node-a"} * 100 > 25`,
		},
		{
			name: "pair-loss tcp derives loss from the results counter",
			rule: Rule{Kind: KindPairLoss, Params: map[string]any{
				"protocol": "tcp", "thresholdPercent": 10.0,
			}},
			golden: `100 * sum by (source_node, destination_node, source_zone, destination_zone) ` +
				`(rate(kconmon_ng_tcp_results_total{result="fail"}[5m])) / ` +
				`sum by (source_node, destination_node, source_zone, destination_zone) ` +
				`(rate(kconmon_ng_tcp_results_total[5m])) > 10`,
		},
		{
			name: "pair-loss tcp scoped",
			rule: Rule{Kind: KindPairLoss, Params: map[string]any{
				"protocol": "tcp", "thresholdPercent": 1.0,
				"scope": map[string]any{"sourceNode": "n1", "destNode": "n2"},
			}},
			golden: `100 * sum by (source_node, destination_node, source_zone, destination_zone) ` +
				`(rate(kconmon_ng_tcp_results_total{source_node="n1",destination_node="n2",result="fail"}[5m])) / ` +
				`sum by (source_node, destination_node, source_zone, destination_zone) ` +
				`(rate(kconmon_ng_tcp_results_total{source_node="n1",destination_node="n2"}[5m])) > 1`,
		},
		{
			name: "zone-latency udp p95 both zones",
			rule: Rule{Kind: KindZoneLatency, Params: map[string]any{
				"protocol": "udp", "quantile": 0.95, "thresholdMs": 25.0,
				"sourceZone": "zone-a", "destZone": "zone-b",
			}},
			golden: `histogram_quantile(0.95, sum by (le, source_zone, destination_zone) ` +
				`(rate(kconmon_ng_udp_rtt_seconds_bucket{source_zone="zone-a",destination_zone="zone-b"}[5m])))` +
				` * 1000 > 25`,
		},
		{
			name: "zone-latency tcp p99 unscoped uses the total-duration histogram",
			rule: Rule{Kind: KindZoneLatency, Params: map[string]any{
				"protocol": "tcp", "quantile": 0.99, "thresholdMs": 100.0,
			}},
			golden: `histogram_quantile(0.99, sum by (le, source_zone, destination_zone) ` +
				`(rate(kconmon_ng_tcp_total_duration_seconds_bucket[5m]))) * 1000 > 100`,
		},
		{
			name: "zone-latency icmp p50 dest zone only",
			rule: Rule{Kind: KindZoneLatency, Params: map[string]any{
				"protocol": "icmp", "quantile": 0.5, "thresholdMs": 7.5,
				"destZone": "zone-b",
			}},
			golden: `histogram_quantile(0.5, sum by (le, source_zone, destination_zone) ` +
				`(rate(kconmon_ng_icmp_rtt_seconds_bucket{destination_zone="zone-b"}[5m]))) * 1000 > 7.5`,
		},
		{
			name: "dns-failures",
			rule: Rule{Kind: KindDNSFailures, Params: map[string]any{
				"thresholdPercent": 1.0,
			}},
			golden: `100 * sum by (host, resolver, source_node, source_zone) ` +
				`(rate(kconmon_ng_dns_results_total{result="fail"}[5m])) / ` +
				`sum by (host, resolver, source_node, source_zone) ` +
				`(rate(kconmon_ng_dns_results_total[5m])) > 1`,
		},
		{
			name: "http-ttfb unscoped",
			rule: Rule{Kind: KindHTTPTTFB, Params: map[string]any{
				"thresholdMs": 500.0,
			}},
			golden: `histogram_quantile(0.95, sum by (le, url, source_node, source_zone) ` +
				`(rate(kconmon_ng_http_ttfb_seconds_bucket[5m]))) * 1000 > 500`,
		},
		{
			name: "http-ttfb pinned to one url",
			rule: Rule{Kind: KindHTTPTTFB, Params: map[string]any{
				"thresholdMs": 250.0, "url": "https://example.com/health",
			}},
			golden: `histogram_quantile(0.95, sum by (le, url, source_node, source_zone) ` +
				`(rate(kconmon_ng_http_ttfb_seconds_bucket{url="https://example.com/health"}[5m])))` +
				` * 1000 > 250`,
		},
		{
			name:   "agent-missing takes no params",
			rule:   Rule{Kind: KindAgentMissing},
			golden: `kconmon_ng_controller_registered_agents < kconmon_ng_controller_expected_agents`,
		},
		{
			name:   "agent-missing tolerates an empty params map",
			rule:   Rule{Kind: KindAgentMissing, Params: map[string]any{}},
			golden: `kconmon_ng_controller_registered_agents < kconmon_ng_controller_expected_agents`,
		},
		{
			name: "external-target-down all targets",
			rule: Rule{Kind: KindExternalTargetDown},
			golden: `sum by (target, target_kind, source_node, source_zone) ` +
				`(rate(kconmon_ng_external_results_total{result="fail"}[5m])) > 0`,
		},
		{
			name: "external-target-down one target",
			rule: Rule{Kind: KindExternalTargetDown, Params: map[string]any{
				"targetName": "public-dns",
			}},
			golden: `sum by (target, target_kind, source_node, source_zone) ` +
				`(rate(kconmon_ng_external_results_total{target="public-dns",result="fail"}[5m])) > 0`,
		},
		{
			name: "raw passes through verbatim",
			rule: Rule{Kind: KindRaw, Params: map[string]any{
				"expr": `up{job="anything"} == 0`,
			}},
			golden: `up{job="anything"} == 0`,
		},
		{
			name: "raw keeps operator whitespace verbatim",
			rule: Rule{Kind: KindRaw, Params: map[string]any{
				"expr": "  vector(1)  ",
			}},
			golden: "  vector(1)  ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := defaultRenderer.Render(tt.rule)
			if err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			if got != tt.golden {
				t.Errorf("expr mismatch\n got: %s\nwant: %s", got, tt.golden)
			}
		})
	}
}

// Every kind in KnownKinds must have a golden above; a new kind cannot ship
// without one.
func TestEveryKnownKindHasAGolden(t *testing.T) {
	want := map[string]bool{
		KindPairLoss: true, KindZoneLatency: true, KindDNSFailures: true,
		KindHTTPTTFB: true, KindAgentMissing: true, KindExternalTargetDown: true,
		KindRaw: true,
	}
	kinds := KnownKinds()
	if len(kinds) != len(want) {
		t.Fatalf("KnownKinds() = %v, want %d kinds", kinds, len(want))
	}
	for _, k := range kinds {
		if !want[k] {
			t.Errorf("KnownKinds() returned unexpected kind %q", k)
		}
		if !ValidKind(k) {
			t.Errorf("ValidKind(%q) = false", k)
		}
	}
	// Sorted, so the closed set reads the same everywhere it is printed.
	for i := 1; i < len(kinds); i++ {
		if kinds[i-1] >= kinds[i] {
			t.Fatalf("KnownKinds() not sorted: %v", kinds)
		}
	}
	// cert-expiry was DROPPED: no certificate-expiry metric family exists.
	if ValidKind("cert-expiry") {
		t.Error("cert-expiry must not be a valid kind: no cert metric family exists in this codebase")
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	r := Rule{Kind: KindPairLoss, Params: map[string]any{
		"protocol": "tcp", "thresholdPercent": 3.0,
		"scope": map[string]any{"sourceNode": "a", "destNode": "b"},
	}}
	first, err := defaultRenderer.Render(r)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	for i := range 50 {
		got, err := defaultRenderer.Render(r)
		if err != nil {
			t.Fatalf("Render() iteration %d error = %v", i, err)
		}
		if got != first {
			t.Fatalf("Render() not deterministic on iteration %d:\n got: %s\nwant: %s", i, got, first)
		}
	}
}

// JSONB round-trips numbers as float64 and json.Number; both must render the
// same bytes as a native Go float.
func TestRenderAcceptsJSONNumberShapes(t *testing.T) {
	golden := `kconmon_ng_udp_packet_loss_ratio * 100 > 5`

	var viaJSON map[string]any
	if err := json.Unmarshal([]byte(`{"protocol":"udp","thresholdPercent":5}`), &viaJSON); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	dec := json.NewDecoder(strings.NewReader(`{"protocol":"udp","thresholdPercent":5}`))
	dec.UseNumber()
	var viaNumber map[string]any
	if err := dec.Decode(&viaNumber); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for name, params := range map[string]map[string]any{
		"float64":     {"protocol": "udp", "thresholdPercent": 5.0},
		"int":         {"protocol": "udp", "thresholdPercent": 5},
		"int64":       {"protocol": "udp", "thresholdPercent": int64(5)},
		"json float":  viaJSON,
		"json.Number": viaNumber,
	} {
		got, err := defaultRenderer.Render(Rule{Kind: KindPairLoss, Params: params})
		if err != nil {
			t.Fatalf("%s: Render() error = %v", name, err)
		}
		if got != golden {
			t.Errorf("%s: got %s, want %s", name, got, golden)
		}
	}
}

func TestRenderEscapesLabelValues(t *testing.T) {
	got, err := defaultRenderer.Render(Rule{Kind: KindExternalTargetDown, Params: map[string]any{
		"targetName": `he said "hi"\n`,
	}})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	want := `sum by (target, target_kind, source_node, source_zone) ` +
		`(rate(kconmon_ng_external_results_total{target="he said \"hi\"\\n",result="fail"}[5m])) > 0`
	if got != want {
		t.Errorf("escaping mismatch\n got: %s\nwant: %s", got, want)
	}
}

// ---------------------------------------------------------------------------
// Param validation matrix — closed schemas. Every error names the param.
// ---------------------------------------------------------------------------

func TestRenderParamValidation(t *testing.T) {
	tests := []struct {
		name    string
		rule    Rule
		wantSub []string
	}{
		{
			name:    "unknown kind",
			rule:    Rule{Kind: "pair-latency"},
			wantSub: []string{"pair-latency", "unknown kind"},
		},
		{
			name:    "empty kind",
			rule:    Rule{Kind: ""},
			wantSub: []string{"unknown kind"},
		},
		{
			name:    "pair-loss missing protocol",
			rule:    Rule{Kind: KindPairLoss, Params: map[string]any{"thresholdPercent": 5.0}},
			wantSub: []string{"pair-loss", "protocol", "required"},
		},
		{
			name:    "pair-loss missing thresholdPercent",
			rule:    Rule{Kind: KindPairLoss, Params: map[string]any{"protocol": "udp"}},
			wantSub: []string{"pair-loss", "thresholdPercent", "required"},
		},
		{
			name: "pair-loss unknown param is rejected, not defaulted",
			rule: Rule{Kind: KindPairLoss, Params: map[string]any{
				"protocol": "udp", "thresholdPercent": 5.0, "thresholdPercnt": 9.0,
			}},
			wantSub: []string{"pair-loss", "thresholdPercnt", "unknown"},
		},
		{
			name: "pair-loss bad protocol",
			rule: Rule{Kind: KindPairLoss, Params: map[string]any{
				"protocol": "quic", "thresholdPercent": 5.0,
			}},
			wantSub: []string{"protocol", "quic", "tcp"},
		},
		{
			name: "pair-loss protocol wrong type",
			rule: Rule{Kind: KindPairLoss, Params: map[string]any{
				"protocol": 4.0, "thresholdPercent": 5.0,
			}},
			wantSub: []string{"protocol", "string"},
		},
		{
			name: "pair-loss threshold above 100",
			rule: Rule{Kind: KindPairLoss, Params: map[string]any{
				"protocol": "udp", "thresholdPercent": 100.1,
			}},
			wantSub: []string{"thresholdPercent", "0", "100"},
		},
		{
			name: "pair-loss threshold below 0",
			rule: Rule{Kind: KindPairLoss, Params: map[string]any{
				"protocol": "udp", "thresholdPercent": -1.0,
			}},
			wantSub: []string{"thresholdPercent", "0", "100"},
		},
		{
			name: "pair-loss threshold wrong type",
			rule: Rule{Kind: KindPairLoss, Params: map[string]any{
				"protocol": "udp", "thresholdPercent": "5",
			}},
			wantSub: []string{"thresholdPercent", "number"},
		},
		{
			name: "pair-loss scope wrong type",
			rule: Rule{Kind: KindPairLoss, Params: map[string]any{
				"protocol": "udp", "thresholdPercent": 5.0, "scope": "node-a",
			}},
			wantSub: []string{"scope", "object"},
		},
		{
			name: "pair-loss scope unknown key",
			rule: Rule{Kind: KindPairLoss, Params: map[string]any{
				"protocol": "udp", "thresholdPercent": 5.0,
				"scope": map[string]any{"srcNode": "a"},
			}},
			wantSub: []string{"scope", "srcNode", "unknown"},
		},
		{
			name: "pair-loss scope empty value",
			rule: Rule{Kind: KindPairLoss, Params: map[string]any{
				"protocol": "udp", "thresholdPercent": 5.0,
				"scope": map[string]any{"sourceNode": ""},
			}},
			wantSub: []string{"sourceNode", "empty"},
		},
		{
			name: "zone-latency missing quantile",
			rule: Rule{Kind: KindZoneLatency, Params: map[string]any{
				"protocol": "udp", "thresholdMs": 10.0,
			}},
			wantSub: []string{"zone-latency", "quantile", "required"},
		},
		{
			name: "zone-latency quantile outside the closed set",
			rule: Rule{Kind: KindZoneLatency, Params: map[string]any{
				"protocol": "udp", "quantile": 0.9, "thresholdMs": 10.0,
			}},
			wantSub: []string{"quantile", "0.9", "0.95"},
		},
		{
			name: "zone-latency thresholdMs must be positive",
			rule: Rule{Kind: KindZoneLatency, Params: map[string]any{
				"protocol": "udp", "quantile": 0.95, "thresholdMs": 0.0,
			}},
			wantSub: []string{"thresholdMs", "greater than 0"},
		},
		{
			name: "zone-latency missing protocol",
			rule: Rule{Kind: KindZoneLatency, Params: map[string]any{
				"quantile": 0.95, "thresholdMs": 10.0,
			}},
			wantSub: []string{"protocol", "required"},
		},
		{
			name:    "dns-failures missing threshold",
			rule:    Rule{Kind: KindDNSFailures, Params: map[string]any{}},
			wantSub: []string{"dns-failures", "thresholdPercent", "required"},
		},
		{
			name: "dns-failures unknown param",
			rule: Rule{Kind: KindDNSFailures, Params: map[string]any{
				"thresholdPercent": 1.0, "resolver": "10.96.0.10",
			}},
			wantSub: []string{"dns-failures", "resolver", "unknown"},
		},
		{
			name:    "http-ttfb missing threshold",
			rule:    Rule{Kind: KindHTTPTTFB, Params: nil},
			wantSub: []string{"http-ttfb", "thresholdMs", "required"},
		},
		{
			name: "http-ttfb empty url",
			rule: Rule{Kind: KindHTTPTTFB, Params: map[string]any{
				"thresholdMs": 100.0, "url": "",
			}},
			wantSub: []string{"url", "empty"},
		},
		{
			name: "http-ttfb rejects a quantile param it does not have",
			rule: Rule{Kind: KindHTTPTTFB, Params: map[string]any{
				"thresholdMs": 100.0, "quantile": 0.99,
			}},
			wantSub: []string{"http-ttfb", "quantile", "unknown"},
		},
		{
			name: "agent-missing rejects forMinutes (it lives in ForNS)",
			rule: Rule{Kind: KindAgentMissing, Params: map[string]any{
				"forMinutes": 10.0,
			}},
			wantSub: []string{"agent-missing", "forMinutes", "unknown"},
		},
		{
			name: "external-target-down unknown param",
			rule: Rule{Kind: KindExternalTargetDown, Params: map[string]any{
				"target": "public-dns",
			}},
			wantSub: []string{"external-target-down", "target", "unknown"},
		},
		{
			name:    "raw missing expr",
			rule:    Rule{Kind: KindRaw, Params: map[string]any{}},
			wantSub: []string{"raw", "expr", "required"},
		},
		{
			name:    "raw blank expr",
			rule:    Rule{Kind: KindRaw, Params: map[string]any{"expr": "   \t\n"}},
			wantSub: []string{"expr", "empty"},
		},
		{
			name:    "raw expr wrong type",
			rule:    Rule{Kind: KindRaw, Params: map[string]any{"expr": 1.0}},
			wantSub: []string{"expr", "string"},
		},
		{
			name: "NaN threshold",
			rule: Rule{Kind: KindHTTPTTFB, Params: map[string]any{
				"thresholdMs": math.NaN(),
			}},
			wantSub: []string{"thresholdMs", "finite"},
		},
		{
			name: "Inf threshold",
			rule: Rule{Kind: KindHTTPTTFB, Params: map[string]any{
				"thresholdMs": math.Inf(1),
			}},
			wantSub: []string{"thresholdMs", "finite"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := defaultRenderer.Render(tt.rule)
			if err == nil {
				t.Fatalf("Render() returned %q, want error", got)
			}
			for _, sub := range tt.wantSub {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error %q does not mention %q", err.Error(), sub)
				}
			}
		})
	}
}

// Multiple unknown params must produce the SAME error every time — no map
// iteration leaking into an error string.
func TestUnknownParamErrorIsDeterministic(t *testing.T) {
	r := Rule{Kind: KindDNSFailures, Params: map[string]any{
		"thresholdPercent": 1.0, "zzz": 1, "aaa": 2, "mmm": 3,
	}}
	_, err := defaultRenderer.Render(r)
	if err == nil {
		t.Fatal("want error")
	}
	first := err.Error()
	if !strings.Contains(first, `"aaa"`) {
		t.Errorf("expected the alphabetically first unknown param in %q", first)
	}
	for range 50 {
		_, err := defaultRenderer.Render(r)
		if err == nil || err.Error() != first {
			t.Fatalf("non-deterministic error: %v vs %q", err, first)
		}
	}
}

// ---------------------------------------------------------------------------
// FormatPromDuration
// ---------------------------------------------------------------------------

func TestFormatPromDuration(t *testing.T) {
	const (
		ms = int64(1_000_000)
		s  = 1000 * ms
		m  = 60 * s
		h  = 60 * m
		d  = 24 * h
	)
	tests := []struct {
		ns   int64
		want string
	}{
		{0, "0s"},
		{5 * m, "5m"},
		{90 * s, "90s"},
		{30 * s, "30s"},
		{1 * s, "1s"},
		{2 * h, "2h"},
		{90 * m, "90m"},
		{d, "1d"},
		{7 * d, "7d"},
		{36 * h, "36h"},
		{500 * ms, "500ms"},
		{1500 * ms, "1500ms"},
		{10 * m, "10m"},
		{3600 * s, "1h"},
	}
	for _, tt := range tests {
		got, err := FormatPromDuration(tt.ns)
		if err != nil {
			t.Errorf("FormatPromDuration(%d) error = %v", tt.ns, err)
			continue
		}
		if got != tt.want {
			t.Errorf("FormatPromDuration(%d) = %q, want %q", tt.ns, got, tt.want)
		}
	}
}

func TestFormatPromDurationErrors(t *testing.T) {
	if _, err := FormatPromDuration(-1); err == nil {
		t.Error("negative duration must error")
	}
	if _, err := FormatPromDuration(1234); err == nil {
		t.Error("sub-millisecond duration must error")
	}
	if _, err := FormatPromDuration(1_000_001); err == nil {
		t.Error("non-millisecond-aligned duration must error")
	}
}

// ---------------------------------------------------------------------------
// ParsePromDuration
// ---------------------------------------------------------------------------

// promDurationUnitsForTest mirrors the parser's own table. Declared in the test
// so a silent change to a unit size (a "y" that stops being 365d) fails here
// rather than quietly re-interpreting every adopted foreign rule.
const (
	testMS = int64(1_000_000)
	testS  = 1000 * testMS
	testM  = 60 * testS
	testH  = 60 * testM
	testD  = 24 * testH
	testW  = 7 * testD
	testY  = 365 * testD
)

func TestParsePromDuration(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		// Every unit, alone.
		{"0", 0},
		{"0s", 0},
		{"500ms", 500 * testMS},
		{"30s", 30 * testS},
		{"5m", 5 * testM},
		{"2h", 2 * testH},
		{"1d", testD},
		{"1w", testW},
		{"1y", testY},
		// COMPOSITE, which is the whole reason this exists: a foreign
		// PrometheusRule is written by a human and "1h30m" is what humans type.
		{"1h30m", testH + 30*testM},
		{"1m30s", testM + 30*testS},
		{"2w3d", 2*testW + 3*testD},
		{"1h30m10s500ms", testH + 30*testM + 10*testS + 500*testMS},
		{"1y2w3d4h5m6s7ms", testY + 2*testW + 3*testD + 4*testH + 5*testM + 6*testS + 7*testMS},
		// A zero component is legal and contributes nothing.
		{"1h0m", testH},
		// Multi-digit values in a non-canonical unit stay verbatim, never
		// normalised: 90m is 90m, not 1h30m.
		{"90m", 90 * testM},
		{"1500ms", 1500 * testMS},
	}
	for _, tt := range tests {
		got, err := ParsePromDuration(tt.in)
		if err != nil {
			t.Errorf("ParsePromDuration(%q) error = %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParsePromDuration(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestParsePromDurationErrors(t *testing.T) {
	tests := []struct {
		in  string
		why string
	}{
		{"", "empty"},
		{"   ", "whitespace only"},
		{"1.5h", "fractional values are not a Prometheus duration"},
		{"-5m", "negative"},
		{"+5m", "signed"},
		{"5", "a bare number with no unit (only \"0\" is legal)"},
		{"m", "a unit with no number"},
		{"5x", "unknown unit"},
		{"5 m", "internal whitespace"},
		{"1h ", "trailing whitespace"},
		{" 1h", "leading whitespace"},
		{"abc", "garbage"},
		{"30s1m", "units ascending"},
		{"1m1m", "a repeated unit"},
		{"1h30m10s5m", "a unit that reappears later"},
		{"1H", "upper-case unit"},
		{"9223372036854775808ms", "a value that does not fit in an int64"},
		{"100000y", "a duration that overflows int64 nanoseconds"},
		{"1y1y", "y repeated"},
	}
	for _, tt := range tests {
		if got, err := ParsePromDuration(tt.in); err == nil {
			t.Errorf("ParsePromDuration(%q) = %d, want an error (%s)", tt.in, got, tt.why)
		}
	}
}

// The two functions are inverses, and this is the assertion that keeps them that way.
func TestPromDurationRoundTrips(t *testing.T) {
	for _, ns := range []int64{
		0,
		testMS, 500 * testMS, 1500 * testMS,
		testS, 30 * testS, 90 * testS,
		testM, 5 * testM, 90 * testM,
		testH, 2 * testH, 36 * testH,
		testD, 7 * testD, 365 * testD,
	} {
		formatted, err := FormatPromDuration(ns)
		if err != nil {
			t.Errorf("FormatPromDuration(%d) error = %v", ns, err)
			continue
		}
		back, err := ParsePromDuration(formatted)
		if err != nil {
			t.Errorf("ParsePromDuration(FormatPromDuration(%d)=%q) error = %v", ns, formatted, err)
			continue
		}
		if back != ns {
			t.Errorf("round trip %d -> %q -> %d", ns, formatted, back)
		}
		again, err := FormatPromDuration(back)
		if err != nil || again != formatted {
			t.Errorf("FormatPromDuration(ParsePromDuration(%q)) = %q, %v; want %q", formatted, again, err, formatted)
		}
	}
}

// The inverse direction is NOT total, and this pins which way it fails.
func TestParsePromDurationIsNotOnto(t *testing.T) {
	ns, err := ParsePromDuration("1h30m")
	if err != nil {
		t.Fatalf("ParsePromDuration: %v", err)
	}
	got, err := FormatPromDuration(ns)
	if err != nil {
		t.Fatalf("FormatPromDuration: %v", err)
	}
	if got != "90m" {
		t.Errorf("FormatPromDuration(ParsePromDuration(%q)) = %q, want %q", "1h30m", got, "90m")
	}
}

// ---------------------------------------------------------------------------
// SanitizeAlertName
// ---------------------------------------------------------------------------

func TestSanitizeAlertName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"UDPLossHigh", "UDPLossHigh"},
		{"udp loss high", "UdpLossHigh"},
		{"zone a -> zone b loss", "ZoneAZoneBLoss"},
		{"dns failures (prod)", "DnsFailuresProd"},
		{"already_snake_case", "Already_snake_case"},
		{"tcp/udp mix", "TcpUdpMix"},
		{"  padded  ", "Padded"},
		{"a", "A"},
		{"node-1 down", "Node1Down"},
		{"ttfb > 500ms", "Ttfb500ms"},
		{"кириллица latency", "Latency"},
	}
	for _, tt := range tests {
		got, err := SanitizeAlertName(tt.in)
		if err != nil {
			t.Errorf("SanitizeAlertName(%q) error = %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("SanitizeAlertName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSanitizeAlertNameErrors(t *testing.T) {
	for _, in := range []string{"", "   ", "5xx rate", "___", "42", "-> <-", "кириллица"} {
		if got, err := SanitizeAlertName(in); err == nil {
			t.Errorf("SanitizeAlertName(%q) = %q, want error", in, got)
		}
	}
}
