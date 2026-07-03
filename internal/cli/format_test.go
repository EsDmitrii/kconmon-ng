package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

func TestHumanizeDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{-5, "0s"},
		{500 * time.Nanosecond, "500ns"},
		{1500 * time.Nanosecond, "1.5µs"},
		{2 * time.Millisecond, "2ms"},
		{1500 * time.Microsecond, "1.5ms"},
		{1200 * time.Millisecond, "1.2s"},
		{3 * time.Second, "3s"},
	}
	for _, c := range cases {
		if got := humanizeDuration(c.in); got != c.want {
			t.Errorf("humanizeDuration(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHumanizePct(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0%"},
		{0.5, "50%"},
		{1, "100%"},
		{0.125, "12.5%"},
		{-1, "-"},
	}
	for _, c := range cases {
		if got := humanizePct(c.in); got != c.want {
			t.Errorf("humanizePct(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func sampleTopology() *model.TopologySnapshot {
	now := time.Now()
	return &model.TopologySnapshot{
		Nodes: []model.NodeInfo{
			{Name: "node-1", Zone: "us-east-1a", Ready: true},
			{Name: "node-2", Zone: "us-east-1b", Ready: true},
			{Name: "node-3", Zone: "us-east-1c", Ready: false},
		},
		Agents: []model.AgentInfo{
			{ID: "node-1-kconmon-ng-agent-aaaaa", NodeName: "node-1", PodIP: "10.0.0.1", Zone: "us-east-1a", LastSeen: now.Add(-5 * time.Second)},
			{ID: "node-2-kconmon-ng-agent-bbbbb", NodeName: "node-2", PodIP: "10.0.0.2", Zone: "us-east-1b", LastSeen: now.Add(-2 * time.Second)},
		},
		Timestamp: now,
	}
}

func TestFormatTopology(t *testing.T) {
	var buf bytes.Buffer
	if err := formatTopology(&buf, sampleTopology()); err != nil {
		t.Fatalf("formatTopology: %v", err)
	}
	out := buf.String()

	for _, want := range []string{"NODE", "ZONE", "READY", "AGENT", "AGENT IP"} {
		if !strings.Contains(out, want) {
			t.Errorf("header missing %q in:\n%s", want, out)
		}
	}
	// node-1 has an agent
	if !strings.Contains(out, "node-1-kconmon-ng-agent-aaaaa") || !strings.Contains(out, "10.0.0.1") {
		t.Errorf("expected node-1 agent row, got:\n%s", out)
	}
	// node-3 has no agent => dashes
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var node3 string
	for _, l := range lines {
		if strings.HasPrefix(l, "node-3") {
			node3 = l
		}
	}
	if node3 == "" {
		t.Fatalf("node-3 row missing:\n%s", out)
	}
	if !strings.Contains(node3, "no") {
		t.Errorf("node-3 should be not ready: %q", node3)
	}
	if !strings.Contains(node3, "-") {
		t.Errorf("node-3 should have dash for absent agent: %q", node3)
	}
}

func TestFormatAgents(t *testing.T) {
	var buf bytes.Buffer
	if err := formatAgents(&buf, sampleTopology()); err != nil {
		t.Fatalf("formatAgents: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"ID", "NODE", "POD IP", "ZONE", "LAST SEEN"} {
		if !strings.Contains(out, want) {
			t.Errorf("header missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "node-1-kconmon-ng-agent-aaaaa") {
		t.Errorf("expected agent id in output:\n%s", out)
	}
	if !strings.Contains(out, "ago") {
		t.Errorf("expected humanized last-seen in output:\n%s", out)
	}
}

func TestFormatAgentsEmpty(t *testing.T) {
	var buf bytes.Buffer
	snap := &model.TopologySnapshot{}
	if err := formatAgents(&buf, snap); err != nil {
		t.Fatalf("formatAgents empty: %v", err)
	}
	if !strings.Contains(buf.String(), "ID") {
		t.Errorf("expected header even when no agents:\n%s", buf.String())
	}
}

func TestFormatCheckICMPSuccess(t *testing.T) {
	res := &model.CheckResult{
		Type:        model.CheckICMP,
		Success:     true,
		Source:      "node-1",
		Destination: "node-2",
		SourceZone:  "us-east-1a",
		DestZone:    "us-east-1b",
		Duration:    1500 * time.Microsecond,
		Details: map[string]any{
			"rtt":       float64(2 * time.Millisecond),
			"lossRatio": 0.0,
		},
	}
	var buf bytes.Buffer
	if err := formatCheck(&buf, res); err != nil {
		t.Fatalf("formatCheck: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "OK") {
		t.Errorf("expected OK status:\n%s", out)
	}
	if !strings.Contains(out, "node-1 -> node-2") {
		t.Errorf("expected route:\n%s", out)
	}
	if !strings.Contains(out, "rtt=2ms") {
		t.Errorf("expected rtt from details:\n%s", out)
	}
	if !strings.Contains(out, "loss=0%") {
		t.Errorf("expected loss from details:\n%s", out)
	}
}

// Every probe type must speak the same detail grammar: sent, recv, loss, rtt,
// then type-specific extras. This locks the uniform wording so a future
// formatter change cannot silently drift one type away from the others.
func TestFormatCheckUniformGrammar(t *testing.T) {
	cases := []struct {
		typ     model.CheckType
		success bool
		details map[string]any
		want    string
	}{
		{model.CheckICMP, true,
			map[string]any{"rtt": float64(2 * time.Millisecond), "lossRatio": 0.0},
			"sent=1 recv=1 loss=0% rtt=2ms"},
		{model.CheckICMP, false,
			map[string]any{"rtt": float64(time.Second), "lossRatio": 1.0},
			"sent=1 recv=0 loss=100% rtt=1s"},
		{model.CheckTCP, true,
			map[string]any{"connectTime": float64(time.Millisecond), "totalTime": float64(3 * time.Millisecond)},
			"sent=1 recv=1 loss=0% rtt=3ms connect=1ms"},
		{model.CheckTCP, false,
			map[string]any{"connectTime": float64(0), "totalTime": float64(time.Second)},
			"sent=1 recv=0 loss=100% rtt=1s connect=0s"},
		{model.CheckUDP, true,
			map[string]any{"packetsSent": 5, "packetsRecv": 5, "lossRatio": 0.0,
				"meanRtt": float64(time.Millisecond), "jitter": float64(200 * time.Microsecond)},
			"sent=5 recv=5 loss=0% rtt=1ms jitter=200µs"},
		{model.CheckDNS, true,
			map[string]any{"host": "example.com", "resolver": "system", "duration": float64(4 * time.Millisecond)},
			"sent=1 recv=1 loss=0% rtt=4ms host=example.com resolver=system"},
		{model.CheckHTTP, true,
			map[string]any{"statusCode": 200, "totalTime": float64(9 * time.Millisecond),
				"ttfbTime": float64(5 * time.Millisecond), "connectTime": float64(time.Millisecond)},
			"sent=1 recv=1 loss=0% rtt=9ms ttfb=5ms connect=1ms status=200"},
	}
	for _, tc := range cases {
		res := &model.CheckResult{
			Type: tc.typ, Success: tc.success,
			Source: "a", Destination: "b", Details: tc.details,
		}
		var buf bytes.Buffer
		if err := formatCheck(&buf, res); err != nil {
			t.Fatalf("%s: formatCheck: %v", tc.typ, err)
		}
		if !strings.Contains(buf.String(), tc.want) {
			t.Errorf("%s: want detail line %q in output:\n%s", tc.typ, tc.want, buf.String())
		}
	}
}

func TestFormatCheckFailure(t *testing.T) {
	res := &model.CheckResult{
		Type:        model.CheckTCP,
		Success:     false,
		Source:      "node-1",
		Destination: "node-2",
		Duration:    50 * time.Millisecond,
		Error:       "connection refused",
	}
	var buf bytes.Buffer
	if err := formatCheck(&buf, res); err != nil {
		t.Fatalf("formatCheck: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "FAIL") {
		t.Errorf("expected FAIL:\n%s", out)
	}
	if !strings.Contains(out, "connection refused") {
		t.Errorf("expected error line:\n%s", out)
	}
}

func TestFormatCheckNilDetails(t *testing.T) {
	// Defensive: absent details must not panic.
	res := &model.CheckResult{
		Type:        model.CheckICMP,
		Success:     true,
		Source:      "a",
		Destination: "b",
	}
	var buf bytes.Buffer
	if err := formatCheck(&buf, res); err != nil {
		t.Fatalf("formatCheck nil details: %v", err)
	}
	if !strings.Contains(buf.String(), "OK") {
		t.Errorf("expected OK line:\n%s", buf.String())
	}
}

func TestFormatMTR(t *testing.T) {
	res := &model.CheckResult{
		Type:    model.CheckMTR,
		Success: true,
		Details: map[string]any{
			"target": "10.0.0.2",
			"hops": []any{
				map[string]any{"number": 1, "ip": "10.0.0.254", "rtt": float64(500 * time.Microsecond), "lossRatio": 0.0},
				map[string]any{"number": 2, "ip": "*", "rtt": float64(0), "lossRatio": 1.0},
				map[string]any{"number": 3, "ip": "10.0.0.2", "rtt": float64(2 * time.Millisecond), "lossRatio": 0.0},
			},
		},
	}
	var buf bytes.Buffer
	if err := formatMTR(&buf, res); err != nil {
		t.Fatalf("formatMTR: %v", err)
	}
	out := buf.String()
	// Silent hops render as "no reply" with dashes (a probe timeout is not a
	// latency measurement), and a verdict line states whether the target was
	// reached — that is the actual answer of a trace.
	for _, want := range []string{
		"HOP", "IP", "RTT", "LOSS",
		"10.0.0.254", "10.0.0.2", "500µs",
		"no reply",
		"reached 10.0.0.2 at hop 3",
		"1 silent hop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in mtr output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "100%") {
		t.Errorf("silent hop must not render a fake 100%% loss figure:\n%s", out)
	}
}

func TestFormatMTRNoHops(t *testing.T) {
	res := &model.CheckResult{Type: model.CheckMTR, Success: false, Error: "MTR traceroute: timeout"}
	var buf bytes.Buffer
	if err := formatMTR(&buf, res); err != nil {
		t.Fatalf("formatMTR: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "timeout") {
		t.Errorf("expected error line:\n%s", out)
	}
	if !strings.Contains(out, "no hops") {
		t.Errorf("expected no-hops notice:\n%s", out)
	}
}

func TestWriteJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := writeJSON(&buf, []byte(`{"a":1,"b":[2,3]}`)); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "\"a\": 1") {
		t.Errorf("expected indented json:\n%s", out)
	}
}

func TestWriteJSONInvalidPassthrough(t *testing.T) {
	var buf bytes.Buffer
	if err := writeJSON(&buf, []byte(`not json`)); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	if !strings.Contains(buf.String(), "not json") {
		t.Errorf("expected verbatim passthrough:\n%s", buf.String())
	}
}
