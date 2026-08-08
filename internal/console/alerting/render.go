// Package alerting turns console-managed alert-rule rows into deterministic
// PromQL expressions and into ONE PrometheusRule bundle object.
//
// # Constants in code ARE the documentation
//
// Following the M6 investigation.ts precedent: every number, window, metric
// family and label name this package renders is a named constant declared
// below, and the golden tests in render_test.go / bundle_test.go pin the exact
// output bytes. ALERTING.md restates these; it never becomes the source of
// truth. If a constant here and a sentence there disagree, the constant wins
// and the doc is stale.
//
// The constants that are NOT obvious, stated once:
//
//   - MetricPrefix is "kconmon_ng" — the DEFAULT of config.metricsPrefix, and
//     only the default. Rendering hangs off a Renderer VALUE carrying the
//     prefix (NewRenderer), because config.metricsPrefix is operator-settable
//     and a rule rendered against the wrong family name is a rule that can
//     never fire — the exact failure the Grafana dashboards in dashboards/
//     still have (docs/metrics.md "Default alerting rules"). There is no
//     package-level Render: a free function would have to pick a prefix for
//     the caller, and picking the default silently is the bug.
//   - RateWindow is "5m" for every rate()/histogram_quantile template. It is
//     not a builder param: a per-rule window is a second knob that changes what
//     the threshold MEANS, and the plan's builder model has one threshold.
//   - TTFBQuantile is 0.95 for http-ttfb. zone-latency takes a quantile param
//     because a latency SLO is quantile-shaped; TTFB alerting is not.
//   - Loss thresholds are PERCENT in the builder and the loss metrics are
//     RATIOS (0.0-1.0), so the gauge is multiplied by 100 here. Latency
//     thresholds are MILLISECONDS and the histograms are in SECONDS, so the
//     quantile is multiplied by 1000 here. Both conversions happen at render
//     time so the stored params stay in operator units.
//
// # Metric families (every name verified against the exporter, not from memory)
//
//	kconmon_ng_udp_packet_loss_ratio         internal/metrics/prometheus.go:115  gauge, peer labels
//	kconmon_ng_icmp_packet_loss_ratio        internal/metrics/prometheus.go:130  gauge, peer labels
//	kconmon_ng_tcp_results_total             internal/metrics/prometheus.go:101  counter, peer + result
//	kconmon_ng_udp_rtt_seconds               internal/metrics/prometheus.go:106  histogram, peer labels
//	kconmon_ng_icmp_rtt_seconds              internal/metrics/prometheus.go:124  histogram, peer labels
//	kconmon_ng_tcp_total_duration_seconds    internal/metrics/prometheus.go:96   histogram, peer labels
//	kconmon_ng_dns_results_total             internal/metrics/prometheus.go:143  counter, host/resolver/source_node/source_zone + result
//	kconmon_ng_http_ttfb_seconds             internal/metrics/prometheus.go:163  histogram, url/source_node/source_zone
//	kconmon_ng_external_results_total        internal/metrics/prometheus.go:196  counter, external labels + result
//	kconmon_ng_controller_registered_agents  internal/metrics/prometheus.go:222  gauge, unlabelled
//	kconmon_ng_controller_expected_agents    internal/metrics/prometheus.go:226  gauge, unlabelled
//
// Peer label set (internal/metrics/prometheus.go:75): source_node,
// destination_node, source_zone, destination_zone. External label set
// (internal/metrics/prometheus.go:82): source_node, source_zone, target,
// target_kind — deliberately NOT the peer set (docs/metrics.md:7-14).
//
// # Two template kinds deviate from the brief, on evidence
//
//   - cert-expiry is DROPPED. There is no certificate-expiry metric family in
//     this codebase: no exporter declares one (internal/metrics/prometheus.go
//     is the whole agent/controller surface, internal/console/metrics is the
//     console surface) and docs/metrics.md lists none. Rendering a rule over an
//     invented series would ship an alert that can never fire. Pinned by
//     TestEveryKnownKindHasAGolden.
//   - agent-missing renders the CONTROLLER-side count comparison
//     (registered < expected), not a per-node absence expression. There is no
//     per-node agent up/heartbeat family: the agent's own liveness is only
//     visible through the peer probe families, which are keyed by
//     source_node/destination_node and therefore go silent for a node that
//     never registered — absent() over them cannot enumerate the nodes that
//     SHOULD exist. The controller derives expected_agents from its node
//     informer, so it is the only series in the system that knows the
//     denominator. This is the same expression the shipped KconmonAgentsMissing
//     default rule uses (docs/metrics.md "Default alerting rules").
package alerting

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Metric-name and expression constants. See the package doc block.
const (
	// MetricPrefix is the DEFAULT config.metricsPrefix (internal/config/defaults.go:8,
	// internal/console/config/config.go:484).
	MetricPrefix = "kconmon_ng"

	// RateWindow is the range-vector window every rate() template uses.
	RateWindow = "5m"

	// TTFBQuantile is the fixed quantile of the http-ttfb template.
	TTFBQuantile = 0.95

	// The metric-family SUFFIXES. Everything after the configurable prefix is
	// fixed by the exporter, so these are constants and the prefix is not.
	suffixUDPLoss        = "_udp_packet_loss_ratio"
	suffixICMPLoss       = "_icmp_packet_loss_ratio"
	suffixTCPResults     = "_tcp_results_total"
	suffixUDPRTT         = "_udp_rtt_seconds"
	suffixICMPRTT        = "_icmp_rtt_seconds"
	suffixTCPDuration    = "_tcp_total_duration_seconds"
	suffixDNSResults     = "_dns_results_total"
	suffixHTTPTTFB       = "_http_ttfb_seconds"
	suffixExternalResult = "_external_results_total"
	suffixRegisteredAgts = "_controller_registered_agents"
	suffixExpectedAgts   = "_controller_expected_agents"
)

// Renderer renders alert rules against ONE metric prefix.
//
// It is a value, not a package-level function set, for exactly one reason:
// config.metricsPrefix is operator-settable and every expression this package
// emits names a metric family. A renderer built from the wrong prefix produces
// syntactically perfect PromQL over series that do not exist — an alert that
// can never fire and never errors. Making the prefix a constructor argument
// forces every caller to say which deployment it is rendering for.
//
// The type is immutable and carries no state beyond the prefix, so one value
// is shared by the reconciler loop and every HTTP request without locking.
type Renderer struct {
	// prefix is the resolved metric prefix, never empty (NewRenderer folds ""
	// into MetricPrefix).
	prefix string
}

// NewRenderer builds a Renderer for prefix. An empty or whitespace-only prefix
// means MetricPrefix — the config package already defaults metricsPrefix and
// refuses an empty one, so this is a repair for a hand-built caller, not a
// supported configuration.
func NewRenderer(prefix string) Renderer {
	p := strings.TrimSpace(prefix)
	if p == "" {
		p = MetricPrefix
	}
	return Renderer{prefix: p}
}

// Prefix reports the metric prefix this renderer emits, so cmd/console can log
// what it actually settled on.
func (r Renderer) Prefix() string { return r.prefix }

// metric joins the renderer's prefix to a family suffix.
func (r Renderer) metric(suffix string) string { return r.prefix + suffix }

// Bundle-object constants. See Decision 4 (ownership) and the single-bundle
// strategy in Task 2 of the M7 plan.
const (
	BundleAPIVersion = "monitoring.coreos.com/v1"
	BundleKind       = "PrometheusRule"

	// GroupName is the ONE group every managed rule lands in.
	GroupName = "kconmon-ng-console"

	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "kconmon-ng-console"

	// RuleIDsAnnotation carries the comma-joined sorted uuids of the rules the
	// bundle was rendered from; the sync layer diffs on it without parsing the
	// spec.
	RuleIDsAnnotation = "kconmon-ng.io/rule-ids"

	// RuleIDLabel and SeverityLabel are added to every rule entry and are
	// therefore RESERVED: a user label of either name is an error, never a
	// silent override.
	RuleIDLabel   = "kconmon_ng_rule_id"
	SeverityLabel = "severity"
)

// Closed sets.
const (
	KindPairLoss           = "pair-loss"
	KindZoneLatency        = "zone-latency"
	KindDNSFailures        = "dns-failures"
	KindHTTPTTFB           = "http-ttfb"
	KindAgentMissing       = "agent-missing"
	KindExternalTargetDown = "external-target-down"
	KindRaw                = "raw"
)

var (
	validProtocols  = []string{"tcp", "udp", "icmp"}
	validQuantiles  = []float64{0.5, 0.95, 0.99}
	validSeverities = []string{"info", "warning", "critical"}

	// peerGroupBy / dnsGroupBy / externalGroupBy are the by() clauses of the
	// aggregating templates. They are the metric's own label set minus the
	// aggregated-away ones, written out so the alert carries the labels an
	// operator needs to act.
	peerGroupBy     = []string{"source_node", "destination_node", "source_zone", "destination_zone"}
	zoneGroupBy     = []string{"le", "source_zone", "destination_zone"}
	dnsGroupBy      = []string{"host", "resolver", "source_node", "source_zone"}
	httpGroupBy     = []string{"le", "url", "source_node", "source_zone"}
	externalGroupBy = []string{"target", "target_kind", "source_node", "source_zone"}
)

// Rule is the render engine's input. It is deliberately NOT the store's row
// type: this package imports no pgx and no store package, and Task 1's
// alert_rules row satisfies this shape field-for-field.
type Rule struct {
	ID          string
	Name        string
	Kind        string
	Params      map[string]any
	Severity    string
	ForNS       int64
	Labels      map[string]string
	Annotations map[string]string
	Enabled     bool
}

// ---------------------------------------------------------------------------
// Param schemas — closed. An unknown key is an error, never a default.
// ---------------------------------------------------------------------------

type paramType int

const (
	typeNumber paramType = iota
	typeString
	typeObject
)

func (t paramType) String() string {
	switch t {
	case typeNumber:
		return "a number"
	case typeString:
		return "a string"
	case typeObject:
		return "an object"
	default:
		return "a value"
	}
}

type paramSpec struct {
	Type     paramType
	Required bool
}

var scopeSchema = map[string]paramSpec{
	"sourceNode": {Type: typeString},
	"destNode":   {Type: typeString},
}

var kindSchemas = map[string]map[string]paramSpec{
	KindPairLoss: {
		"protocol":         {Type: typeString, Required: true},
		"thresholdPercent": {Type: typeNumber, Required: true},
		"scope":            {Type: typeObject},
	},
	KindZoneLatency: {
		"protocol":    {Type: typeString, Required: true},
		"quantile":    {Type: typeNumber, Required: true},
		"thresholdMs": {Type: typeNumber, Required: true},
		"sourceZone":  {Type: typeString},
		"destZone":    {Type: typeString},
	},
	KindDNSFailures: {
		"thresholdPercent": {Type: typeNumber, Required: true},
	},
	KindHTTPTTFB: {
		"thresholdMs": {Type: typeNumber, Required: true},
		"url":         {Type: typeString},
	},
	// agent-missing takes NO params. forMinutes lives in Rule.ForNS, which the
	// builder owns, so accepting it here would create two places that mean
	// "how long before this fires".
	KindAgentMissing:       {},
	KindExternalTargetDown: {"targetName": {Type: typeString}},
	KindRaw:                {"expr": {Type: typeString, Required: true}},
}

// KnownKinds returns the closed set of template kinds, sorted.
func KnownKinds() []string {
	return slices.Sorted(maps.Keys(kindSchemas))
}

// ValidKind reports whether kind is in the closed set.
func ValidKind(kind string) bool {
	_, ok := kindSchemas[kind]
	return ok
}

// ---------------------------------------------------------------------------
// Render
// ---------------------------------------------------------------------------

// Render turns one rule into a PromQL expression. It is pure and total: the
// same input renders the same bytes forever, and every rejection names the
// param that caused it.
//
//nolint:gocritic // hugeParam: the value receiver is the signature pinned by the M7 Task 2 brief.
func (r Renderer) Render(rule Rule) (expr string, err error) {
	schema, ok := kindSchemas[rule.Kind]
	if !ok {
		return "", fmt.Errorf("unknown kind %q (known: %s)", rule.Kind, strings.Join(KnownKinds(), ", "))
	}
	if err := checkSchema(rule.Kind, rule.Params, schema); err != nil {
		return "", err
	}

	switch rule.Kind {
	case KindPairLoss:
		return r.renderPairLoss(rule.Params)
	case KindZoneLatency:
		return r.renderZoneLatency(rule.Params)
	case KindDNSFailures:
		return r.renderDNSFailures(rule.Params)
	case KindHTTPTTFB:
		return r.renderHTTPTTFB(rule.Params)
	case KindAgentMissing:
		return r.metric(suffixRegisteredAgts) + " < " + r.metric(suffixExpectedAgts), nil
	case KindExternalTargetDown:
		return r.renderExternalTargetDown(rule.Params)
	case KindRaw:
		return renderRaw(rule.Params)
	default:
		// Unreachable: kindSchemas is the closed set and it was checked above.
		return "", fmt.Errorf("unknown kind %q", rule.Kind)
	}
}

func (r Renderer) renderPairLoss(params map[string]any) (expr string, err error) {
	protocol, err := enumParam(KindPairLoss, params, "protocol", validProtocols)
	if err != nil {
		return "", err
	}
	threshold, err := percentParam(KindPairLoss, params, "thresholdPercent")
	if err != nil {
		return "", err
	}
	scope, err := scopeMatchers(params)
	if err != nil {
		return "", err
	}

	// TCP has no packet-loss gauge (it is a connect probe, not a packet
	// stream): its loss is the failure share of the results counter. UDP and
	// ICMP publish a ratio gauge and are read directly.
	if protocol == "tcp" {
		return failureRatioExpr(r.metric(suffixTCPResults), peerGroupBy, scope, threshold), nil
	}
	suffix := suffixUDPLoss
	if protocol == "icmp" {
		suffix = suffixICMPLoss
	}
	return r.metric(suffix) + selector(scope) + " * 100 > " + formatNumber(threshold), nil
}

func (r Renderer) renderZoneLatency(params map[string]any) (expr string, err error) {
	protocol, err := enumParam(KindZoneLatency, params, "protocol", validProtocols)
	if err != nil {
		return "", err
	}
	quantile, err := numberParam(KindZoneLatency, params, "quantile")
	if err != nil {
		return "", err
	}
	if !slices.Contains(validQuantiles, quantile) {
		return "", fmt.Errorf("%s: param %q must be one of %s, got %s",
			KindZoneLatency, "quantile", formatNumberList(validQuantiles), formatNumber(quantile))
	}
	thresholdMs, err := positiveParam(KindZoneLatency, params, "thresholdMs")
	if err != nil {
		return "", err
	}

	var histogram string
	switch protocol {
	case "tcp":
		histogram = r.metric(suffixTCPDuration)
	case "udp":
		histogram = r.metric(suffixUDPRTT)
	default:
		histogram = r.metric(suffixICMPRTT)
	}

	matchers := make([]matcher, 0, 2)
	for _, p := range []struct{ param, label string }{
		{"sourceZone", "source_zone"},
		{"destZone", "destination_zone"},
	} {
		v, ok, err := optionalStringParam(KindZoneLatency, params, p.param)
		if err != nil {
			return "", err
		}
		if ok {
			matchers = append(matchers, matcher{p.label, v})
		}
	}

	return quantileExpr(quantile, histogram, zoneGroupBy, matchers, thresholdMs), nil
}

func (r Renderer) renderDNSFailures(params map[string]any) (expr string, err error) {
	threshold, err := percentParam(KindDNSFailures, params, "thresholdPercent")
	if err != nil {
		return "", err
	}
	return failureRatioExpr(r.metric(suffixDNSResults), dnsGroupBy, nil, threshold), nil
}

func (r Renderer) renderHTTPTTFB(params map[string]any) (expr string, err error) {
	thresholdMs, err := positiveParam(KindHTTPTTFB, params, "thresholdMs")
	if err != nil {
		return "", err
	}
	var matchers []matcher
	url, ok, err := optionalStringParam(KindHTTPTTFB, params, "url")
	if err != nil {
		return "", err
	}
	if ok {
		matchers = append(matchers, matcher{"url", url})
	}
	return quantileExpr(TTFBQuantile, r.metric(suffixHTTPTTFB), httpGroupBy, matchers, thresholdMs), nil
}

func (r Renderer) renderExternalTargetDown(params map[string]any) (expr string, err error) {
	var matchers []matcher
	target, ok, err := optionalStringParam(KindExternalTargetDown, params, "targetName")
	if err != nil {
		return "", err
	}
	if ok {
		matchers = append(matchers, matcher{"target", target})
	}
	// A refused probe increments external_denied_total and NOT
	// external_results_total (docs/metrics.md:88-93), so "down" is a non-zero
	// failure rate rather than a missing success series: an all-failing target
	// has no result="success" series at all, and == 0 over a series that does
	// not exist never fires.
	failing := append(slices.Clone(matchers), matcher{"result", "fail"})
	return "sum by (" + strings.Join(externalGroupBy, ", ") + ") (rate(" +
		r.metric(suffixExternalResult) + selector(failing) + "[" + RateWindow + "])) > 0", nil
}

func renderRaw(params map[string]any) (expr string, err error) {
	raw, _ := params["expr"].(string)
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("%s: param %q must not be empty", KindRaw, "expr")
	}
	// Verbatim, including the operator's whitespace: there is no Prometheus
	// parser in this module (Decision 2), so the console must not pretend to
	// normalise an expression it cannot read.
	return raw, nil
}

// failureRatioExpr builds "failed share of a results counter, in percent".
func failureRatioExpr(metric string, groupBy []string, scope []matcher, thresholdPercent float64) string {
	by := "sum by (" + strings.Join(groupBy, ", ") + ") "
	failing := append(slices.Clone(scope), matcher{"result", "fail"})
	return "100 * " + by + "(rate(" + metric + selector(failing) + "[" + RateWindow + "])) / " +
		by + "(rate(" + metric + selector(scope) + "[" + RateWindow + "])) > " +
		formatNumber(thresholdPercent)
}

// quantileExpr builds a histogram_quantile over a *_bucket series, converted
// from seconds to milliseconds so the threshold stays in operator units.
func quantileExpr(quantile float64, histogram string, groupBy []string, scope []matcher, thresholdMs float64) string {
	return "histogram_quantile(" + formatNumber(quantile) + ", sum by (" +
		strings.Join(groupBy, ", ") + ") (rate(" + histogram + "_bucket" + selector(scope) +
		"[" + RateWindow + "]))) * 1000 > " + formatNumber(thresholdMs)
}

// ---------------------------------------------------------------------------
// Param plumbing
// ---------------------------------------------------------------------------

type matcher struct {
	name  string
	value string
}

func selector(matchers []matcher) string {
	if len(matchers) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, m := range matchers {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(m.name)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(m.value))
		b.WriteString(`"`)
	}
	b.WriteByte('}')
	return b.String()
}

var labelValueEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escapeLabelValue(v string) string { return labelValueEscaper.Replace(v) }

// formatNumber pins %g formatting: 5 renders "5", 0.95 renders "0.95".
func formatNumber(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

func formatNumberList(vs []float64) string {
	parts := make([]string, 0, len(vs))
	for _, v := range vs {
		parts = append(parts, formatNumber(v))
	}
	return strings.Join(parts, ", ")
}

// checkSchema enforces the closed schema: unknown keys rejected, required keys
// present, declared types honoured. Every message names the param, and keys are
// walked in sorted order so the same bad input always produces the same error.
func checkSchema(context string, params map[string]any, schema map[string]paramSpec) error {
	for _, key := range slices.Sorted(maps.Keys(params)) {
		spec, known := schema[key]
		if !known {
			return fmt.Errorf("%s: unknown param %q (known: %s)",
				context, key, strings.Join(slices.Sorted(maps.Keys(schema)), ", "))
		}
		if !matchesType(params[key], spec.Type) {
			return fmt.Errorf("%s: param %q must be %s", context, key, spec.Type)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(schema)) {
		if !schema[key].Required {
			continue
		}
		if _, present := params[key]; !present {
			return fmt.Errorf("%s: param %q is required", context, key)
		}
	}
	return nil
}

func matchesType(v any, want paramType) bool {
	switch want {
	case typeString:
		_, ok := v.(string)
		return ok
	case typeObject:
		_, ok := v.(map[string]any)
		return ok
	case typeNumber:
		_, ok := asFloat(v)
		return ok
	default:
		return false
	}
}

// asFloat accepts every numeric shape a JSONB round-trip can produce.
func asFloat(v any) (f float64, ok bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		parsed, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func numberParam(context string, params map[string]any, name string) (value float64, err error) {
	v, ok := asFloat(params[name])
	if !ok {
		return 0, fmt.Errorf("%s: param %q must be a number", context, name)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("%s: param %q must be a finite number", context, name)
	}
	return v, nil
}

func percentParam(context string, params map[string]any, name string) (value float64, err error) {
	v, err := numberParam(context, params, name)
	if err != nil {
		return 0, err
	}
	if v < 0 || v > 100 {
		return 0, fmt.Errorf("%s: param %q must be between 0 and 100, got %s", context, name, formatNumber(v))
	}
	return v, nil
}

func positiveParam(context string, params map[string]any, name string) (value float64, err error) {
	v, err := numberParam(context, params, name)
	if err != nil {
		return 0, err
	}
	if v <= 0 {
		return 0, fmt.Errorf("%s: param %q must be greater than 0, got %s", context, name, formatNumber(v))
	}
	return v, nil
}

func enumParam(context string, params map[string]any, name string, allowed []string) (value string, err error) {
	v, _ := params[name].(string)
	if !slices.Contains(allowed, v) {
		return "", fmt.Errorf("%s: param %q must be one of %s, got %q",
			context, name, strings.Join(allowed, ", "), v)
	}
	return v, nil
}

// optionalStringParam returns ok=false when the key is absent, and an error
// when it is present but blank — an operator who typed a scope and then cleared
// it must not silently get an unscoped rule.
func optionalStringParam(context string, params map[string]any, name string) (value string, present bool, err error) {
	raw, ok := params[name]
	if !ok {
		return "", false, nil
	}
	v, _ := raw.(string)
	if strings.TrimSpace(v) == "" {
		return "", false, fmt.Errorf("%s: param %q must not be empty", context, name)
	}
	return v, true, nil
}

func scopeMatchers(params map[string]any) (matchers []matcher, err error) {
	raw, ok := params["scope"]
	if !ok {
		return nil, nil
	}
	scope, _ := raw.(map[string]any)
	const context = KindPairLoss + ": scope"
	if err := checkSchema(context, scope, scopeSchema); err != nil {
		return nil, err
	}
	// Fixed order, never map order: source first, then destination.
	out := make([]matcher, 0, 2)
	for _, p := range []struct{ param, label string }{
		{"sourceNode", "source_node"},
		{"destNode", "destination_node"},
	} {
		v, present, err := optionalStringParam(context, scope, p.param)
		if err != nil {
			return nil, err
		}
		if present {
			out = append(out, matcher{p.label, v})
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// FormatPromDuration
// ---------------------------------------------------------------------------

// FormatPromDuration renders a nanosecond duration as a Prometheus duration
// string, with no external dependency.
//
// The rule is "largest unit that divides evenly, otherwise the next one down":
// 300s renders "5m" and 90s renders "90s" rather than "1m30s". A single-unit
// string is what an operator typed into the builder in the first place, and a
// compound string would make two equal durations render differently depending
// on which unit the UI happened to use.
//
// Units stop at days: weeks would turn a 7-day window into "1w", which reads
// as a different setting than the one that was entered. Sub-millisecond
// precision is an error rather than a rounding: a `for` of 1.5ms is a bug in
// the caller, not an intent to alert instantly.
func FormatPromDuration(ns int64) (duration string, err error) {
	const (
		millisecond = int64(1_000_000)
		second      = 1000 * millisecond
		minute      = 60 * second
		hour        = 60 * minute
		day         = 24 * hour
	)
	switch {
	case ns < 0:
		return "", fmt.Errorf("duration must not be negative, got %dns", ns)
	case ns == 0:
		return "0s", nil
	}
	for _, u := range []struct {
		size int64
		unit string
	}{
		{day, "d"}, {hour, "h"}, {minute, "m"}, {second, "s"}, {millisecond, "ms"},
	} {
		if ns%u.size == 0 {
			return strconv.FormatInt(ns/u.size, 10) + u.unit, nil
		}
	}
	return "", fmt.Errorf("duration %dns is not a whole number of milliseconds", ns)
}

// ---------------------------------------------------------------------------
// ParsePromDuration
// ---------------------------------------------------------------------------

// promDurationUnit is one entry of the Prometheus duration grammar, in the
// DESCENDING order the grammar itself requires. The order is load-bearing
// twice: it is the legality check for a composite string (a unit may only be
// followed by a smaller one), and it is the reason "ms" can sit next to "m"
// without ambiguity -- the scanner reads a whole letter run, so "500ms" yields
// the unit "ms" and never "m" followed by a stray "s".
//
// y and w are 365d and 7d, Prometheus's own definitions. They are DELIBERATELY
// absent from FormatPromDuration's output side (see its doc comment: a 7-day
// window rendered as "1w" reads as a different setting than the one entered),
// which is why the two functions are inverses in one direction only -- pinned
// by TestParsePromDurationIsNotOnto.
var promDurationUnits = []struct {
	name string
	size int64
}{
	{"y", 365 * 24 * int64(time.Hour)},
	{"w", 7 * 24 * int64(time.Hour)},
	{"d", 24 * int64(time.Hour)},
	{"h", int64(time.Hour)},
	{"m", int64(time.Minute)},
	{"s", int64(time.Second)},
	{"ms", int64(time.Millisecond)},
}

// ParsePromDuration reads a Prometheus duration string and returns
// nanoseconds. It is FormatPromDuration's inverse, and it exists for exactly
// one caller: adopting a FOREIGN PrometheusRule (M7 Decision 4), whose `for:`
// was written by a human in whatever spelling they liked.
//
// It is therefore WIDER than the formatter on purpose:
//
//   - COMPOSITE strings are accepted ("1h30m", "1y2w3d4h5m6s7ms"). The
//     formatter never emits one, but a hand-written rule routinely is one, and
//     refusing it would drop somebody's rule on the floor over a spelling.
//   - y and w are accepted, as 365d and 7d. The formatter stops at days.
//
// It is STRICT everywhere else, because a misread duration is silent: a `for`
// of 5m that parses as 0 turns a rule that waits five minutes into one that
// fires instantly, and nobody reviews an import for that. So the grammar is
// the whole grammar and nothing beside it -- decimal digits only (no sign, no
// decimal point: "1.5h" is an error rather than 90m, matching Prometheus,
// which has no fractional durations), units strictly descending and each used
// at most once, no whitespace anywhere, the entire string consumed, and an
// int64-nanosecond overflow reported rather than wrapped.
//
// The ONE special case is Prometheus's own: the bare string "0" is zero. Every
// other unit-less number is an error.
func ParsePromDuration(s string) (ns int64, err error) {
	if s == "" {
		return 0, errors.New("duration must not be empty")
	}
	if s == "0" {
		return 0, nil
	}

	var (
		total int64
		next  int // the first promDurationUnits index the next component may use
	)
	for i := 0; i < len(s); {
		digits, unit, rest, err := scanPromDurationComponent(s, i)
		if err != nil {
			return 0, err
		}
		i = rest

		idx := slices.IndexFunc(promDurationUnits, func(u struct {
			name string
			size int64
		},
		) bool {
			return u.name == unit
		})
		if idx < 0 {
			return 0, fmt.Errorf("duration %q: unknown unit %q, want one of ms, s, m, h, d, w, y", s, unit)
		}
		if idx < next {
			return 0, fmt.Errorf("duration %q: unit %q is repeated or out of order; "+
				"units must descend through y, w, d, h, m, s, ms and appear at most once", s, unit)
		}
		next = idx + 1

		value, convErr := strconv.ParseInt(digits, 10, 64)
		if convErr != nil {
			return 0, fmt.Errorf("duration %q: %s%s does not fit in an int64", s, digits, unit)
		}
		size := promDurationUnits[idx].size
		part := value * size
		if value != 0 && part/size != value {
			return 0, fmt.Errorf("duration %q overflows int64 nanoseconds", s)
		}
		if total > math.MaxInt64-part {
			return 0, fmt.Errorf("duration %q overflows int64 nanoseconds", s)
		}
		total += part
	}
	return total, nil
}

// scanPromDurationComponent reads ONE number+unit pair starting at from,
// returning the digits, the unit and the offset just past them. Both halves
// are required: a number with no unit and a unit with no number are each an
// error naming what is missing, because "5" meaning five of something
// unstated is exactly the guess this parser refuses to make.
func scanPromDurationComponent(s string, from int) (digits, unit string, rest int, err error) {
	i := from
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == from {
		return "", "", 0, fmt.Errorf("duration %q: expected a number at offset %d, got %q", s, from, s[from:])
	}
	digits = s[from:i]

	unitStart := i
	for i < len(s) && s[i] >= 'a' && s[i] <= 'z' {
		i++
	}
	if i == unitStart {
		return "", "", 0, fmt.Errorf("duration %q: %q has no unit, want one of ms, s, m, h, d, w, y "+
			`(only the bare string "0" may omit one)`, s, digits)
	}
	return digits, s[unitStart:i], i, nil
}

// ---------------------------------------------------------------------------
// SanitizeAlertName
// ---------------------------------------------------------------------------

// SanitizeAlertName turns an operator-typed rule name into a Prometheus alert
// name.
//
// The rules, pinned by TestSanitizeAlertName:
//
//   - Only [a-zA-Z0-9_] survives. Everything else — spaces, punctuation,
//     non-ASCII letters — is DROPPED, and dropping it upper-cases the next
//     surviving character. "zone a -> zone b loss" becomes "ZoneAZoneBLoss".
//   - Underscores survive as themselves and do NOT upper-case the next
//     character, so "already_snake_case" stays readable as
//     "Already_snake_case".
//   - The first surviving character is upper-cased.
//   - The result must start with a LETTER. "5xx rate" is an error rather than
//     a silently prefixed name: a rule an operator cannot find by its own name
//     is worse than a rejected one.
func SanitizeAlertName(name string) (alert string, err error) {
	var b strings.Builder
	b.Grow(len(name))
	upperNext := true
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			if upperNext {
				r -= 'a' - 'A'
			}
			b.WriteRune(r)
			upperNext = false
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_':
			b.WriteRune(r)
			upperNext = false
		default:
			upperNext = true
		}
	}
	out := b.String()
	if out == "" {
		return "", fmt.Errorf("alert name %q sanitizes to an empty string: it has no [a-zA-Z0-9_] characters", name)
	}
	if c := out[0]; (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
		return "", fmt.Errorf("alert name %q sanitizes to %q, which does not start with a letter", name, out)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// RenderBundle
// ---------------------------------------------------------------------------

// RenderBundle renders every ENABLED rule into ONE PrometheusRule object.
//
// One object, not one per rule: it is a single server-side-apply target, so
// drift is one comparison and a partial apply is impossible. Disabled rules are
// dropped entirely rather than rendered and commented out — Prometheus has no
// notion of a disabled rule, and a rule present in the CRD is a rule that
// evaluates.
//
// Determinism is load-bearing (the sync layer diffs rendered bytes against the
// live object): rules are ordered by lower(Name), ties broken by ID, and every
// map that reaches the object is serialized by a key-sorting marshaller. Map
// iteration never reaches the output.
func (r Renderer) RenderBundle(rules []Rule, namespace, bundleName string) (*unstructured.Unstructured, error) {
	if strings.TrimSpace(namespace) == "" {
		return nil, errors.New("RenderBundle: namespace must not be empty")
	}
	if strings.TrimSpace(bundleName) == "" {
		return nil, errors.New("RenderBundle: bundleName must not be empty")
	}

	enabled := make([]Rule, 0, len(rules))
	for _, r := range rules {
		if r.Enabled {
			enabled = append(enabled, r)
		}
	}
	slices.SortStableFunc(enabled, func(a, b Rule) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})

	entries := make([]any, 0, len(enabled))
	ids := make([]string, 0, len(enabled))
	seen := make(map[string]string, len(enabled))
	for i := range enabled {
		rule := &enabled[i]
		entry, alert, err := r.ruleEntry(rule)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[alert]; dup {
			return nil, fmt.Errorf("alert name collision: %q and %q both sanitize to %q", prev, rule.Name, alert)
		}
		seen[alert] = rule.Name
		entries = append(entries, entry)
		ids = append(ids, rule.ID)
	}
	slices.Sort(ids)

	// No enabled rules means no group at all. A group with an empty rules list
	// is rejected by the operator's schema, and asserting one would turn "the
	// operator disabled everything" into a sync error.
	groups := []any{}
	if len(entries) > 0 {
		groups = append(groups, map[string]any{
			"name":  GroupName,
			"rules": entries,
		})
	}

	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": BundleAPIVersion,
		"kind":       BundleKind,
		"metadata": map[string]any{
			"name":      bundleName,
			"namespace": namespace,
			"labels": map[string]any{
				ManagedByLabel: ManagedByValue,
			},
			"annotations": map[string]any{
				RuleIDsAnnotation: strings.Join(ids, ","),
			},
		},
		"spec": map[string]any{
			"groups": groups,
		},
	}}, nil
}

func (rr Renderer) ruleEntry(r *Rule) (entry map[string]any, alert string, err error) {
	if strings.TrimSpace(r.ID) == "" {
		return nil, "", fmt.Errorf("alert rule %q: ID must not be empty", r.Name)
	}
	if strings.TrimSpace(r.Name) == "" {
		return nil, "", fmt.Errorf("alert rule %q: Name must not be empty", r.Name)
	}
	if !slices.Contains(validSeverities, r.Severity) {
		return nil, "", fmt.Errorf("alert rule %q: severity must be one of %s, got %q",
			r.Name, strings.Join(validSeverities, ", "), r.Severity)
	}

	alert, err = SanitizeAlertName(r.Name)
	if err != nil {
		return nil, "", fmt.Errorf("alert rule %q: %w", r.Name, err)
	}

	expr, err := rr.Render(*r)
	if err != nil {
		return nil, "", fmt.Errorf("alert rule %q: %w", r.Name, err)
	}

	labels := make(map[string]any, len(r.Labels)+2)
	for _, key := range slices.Sorted(maps.Keys(r.Labels)) {
		if key == SeverityLabel || key == RuleIDLabel {
			return nil, "", fmt.Errorf("alert rule %q: label %q is reserved by the console", r.Name, key)
		}
		if !validLabelName(key) {
			return nil, "", fmt.Errorf("alert rule %q: invalid label name %q", r.Name, key)
		}
		labels[key] = r.Labels[key]
	}
	labels[SeverityLabel] = r.Severity
	labels[RuleIDLabel] = r.ID

	entry = map[string]any{
		"alert":  alert,
		"expr":   expr,
		"labels": labels,
	}

	// for is omitted at zero rather than written as "0s": a live object will
	// not carry the key either, so drift stays quiet.
	if r.ForNS != 0 {
		forStr, err := FormatPromDuration(r.ForNS)
		if err != nil {
			return nil, "", fmt.Errorf("alert rule %q: for: %w", r.Name, err)
		}
		entry["for"] = forStr
	}

	if len(r.Annotations) > 0 {
		annotations := make(map[string]any, len(r.Annotations))
		for _, key := range slices.Sorted(maps.Keys(r.Annotations)) {
			if !validLabelName(key) {
				return nil, "", fmt.Errorf("alert rule %q: invalid annotation name %q", r.Name, key)
			}
			annotations[key] = r.Annotations[key]
		}
		entry["annotations"] = annotations
	}

	return entry, alert, nil
}

// validLabelName implements the Prometheus label-name grammar
// [a-zA-Z_][a-zA-Z0-9_]*. Annotation names obey the same grammar.
func validLabelName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
