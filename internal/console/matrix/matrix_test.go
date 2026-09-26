package matrix_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/matrix"
)

type fakeQuerier struct{ byContains map[string]string }

func (f *fakeQuerier) Query(_ context.Context, query string, _ time.Time) (json.RawMessage, error) {
	for substr, body := range f.byContains {
		if strings.Contains(query, substr) {
			return json.RawMessage(body), nil
		}
	}
	return json.RawMessage(`{"status":"success","data":{"resultType":"vector","result":[]}}`), nil
}

func vec(entries ...string) string {
	return `{"status":"success","data":{"resultType":"vector","result":[` + strings.Join(entries, ",") + `]}}`
}

func sample(src, dst, value string) string {
	return `{"metric":{"source_node":"` + src + `","destination_node":"` + dst + `"},"value":[1767225600,"` + value + `"]}`
}

func TestComputeTCP(t *testing.T) {
	q := &fakeQuerier{byContains: map[string]string{
		"results_total":              vec(sample("a", "b", "0.25"), sample("b", "a", "0")),
		"tcp_total_duration_seconds": vec(sample("a", "b", "0.002")),
	}}
	m, err := matrix.Compute(context.Background(), q, "kconmon_ng", "tcp")
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if m.Protocol != "tcp" {
		t.Errorf("protocol: %s", m.Protocol)
	}
	if len(m.Nodes) != 2 || m.Nodes[0] != "a" || m.Nodes[1] != "b" {
		t.Errorf("nodes: %v", m.Nodes)
	}
	var ab *matrix.Cell
	for i := range m.Cells {
		if m.Cells[i].Source == "a" && m.Cells[i].Destination == "b" {
			ab = &m.Cells[i]
		}
	}
	if ab == nil || ab.FailRatio == nil || *ab.FailRatio != 0.25 {
		t.Fatalf("a->b failRatio: %+v", ab)
	}
	if ab.RTTP95 == nil || *ab.RTTP95 != int64(2*time.Millisecond) {
		t.Errorf("a->b rttP95 must be 2ms in ns, got %+v", ab.RTTP95)
	}
	if ab.LossRatio != nil {
		t.Errorf("tcp must not carry lossRatio")
	}
}

func TestComputeUDPLoss(t *testing.T) {
	q := &fakeQuerier{byContains: map[string]string{
		"packet_loss_ratio": vec(sample("a", "b", "0.1")),
	}}
	m, err := matrix.Compute(context.Background(), q, "kconmon_ng", "udp")
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if len(m.Cells) != 1 || m.Cells[0].LossRatio == nil || *m.Cells[0].LossRatio != 0.1 {
		t.Fatalf("udp lossRatio: %+v", m.Cells)
	}
}

func TestComputeRejectsProtocol(t *testing.T) {
	_, err := matrix.Compute(context.Background(), &fakeQuerier{}, "kconmon_ng", "http")
	if !errors.Is(err, matrix.ErrBadProtocol) {
		t.Fatalf("expected ErrBadProtocol, got %v", err)
	}
}

// Prometheus serializes 0/0 (a pair whose series went stale mid-window) and empty-bucket
// histogram_quantile as the STRING "NaN"; NaN/±Inf is "no sample".
func TestComputeTreatsNaNAndInfAsNoData(t *testing.T) {
	q := &fakeQuerier{byContains: map[string]string{
		"results_total":              vec(sample("a", "b", "NaN"), sample("b", "a", "0.25")),
		"tcp_total_duration_seconds": vec(sample("a", "b", "+Inf")),
	}}
	m, err := matrix.Compute(context.Background(), q, "kconmon_ng", "tcp")
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	var ab, ba *matrix.Cell
	for i := range m.Cells {
		c := &m.Cells[i]
		if c.Source == "a" && c.Destination == "b" {
			ab = c
		}
		if c.Source == "b" && c.Destination == "a" {
			ba = c
		}
	}
	// Every metric for a→b was NaN/Inf, so the pair has no samples at all and
	// gets NO cell — the UI's honest "no data" slate, not a lying zero.
	if ab != nil {
		t.Errorf("all-NaN pair must have no cell, got %+v", *ab)
	}
	if ba == nil || ba.FailRatio == nil || *ba.FailRatio != 0.25 {
		t.Errorf("the healthy pair must keep its value, got %+v", ba)
	}
	if _, err := json.Marshal(m); err != nil {
		t.Fatalf("the matrix must always be marshalable, got: %v", err)
	}
}

func TestComputePMTU(t *testing.T) {
	q := &fakeQuerier{byContains: map[string]string{
		"pmtu_results_total": vec(sample("a", "b", "1"), sample("b", "a", "0"), sample("a", "c", "0")),
		"_pmtu_bytes)":       vec(sample("a", "b", "1400"), sample("b", "a", "1500"), sample("a", "c", "1450")),
		"pmtu_probe_bytes":   vec(sample("a", "b", "1500"), sample("b", "a", "1500"), sample("a", "c", "1500")),
	}}
	m, err := matrix.Compute(context.Background(), q, "kconmon_ng", "pmtu")
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if m.Protocol != "pmtu" {
		t.Fatalf("protocol = %q", m.Protocol)
	}
	cells := map[string]matrix.Cell{}
	for _, c := range m.Cells {
		cells[c.Source+"->"+c.Destination] = c
	}
	ab := cells["a->b"]
	if ab.FailRatio == nil || *ab.FailRatio != 1 || ab.MTUBytes == nil || *ab.MTUBytes != 1400 ||
		ab.ProbeMTUBytes == nil || *ab.ProbeMTUBytes != 1500 {
		t.Errorf("a->b = %+v, want a black hole at 1400 of 1500", ab)
	}
	ac := cells["a->c"]
	if ac.FailRatio == nil || *ac.FailRatio != 0 || ac.MTUBytes == nil || *ac.MTUBytes != 1450 {
		t.Errorf("a->c = %+v, want a reduced path at 1450 with no failures", ac)
	}
	if ab.RTTP95 != nil || ab.LossRatio != nil {
		t.Errorf("pmtu carries no rtt or loss: %+v", ab)
	}
}

// The pmtu_bytes gauge keeps its last value while a pair's probes are all unreachable (those write no
// result), so a gauge without a verdict in the window is stale and must read as no data.
func TestComputePMTUWithoutAVerdictHasNoCell(t *testing.T) {
	q := &fakeQuerier{byContains: map[string]string{
		"pmtu_results_total": vec(sample("a", "b", "NaN"), sample("b", "a", "0")),
		"_pmtu_bytes)":       vec(sample("a", "b", "1500"), sample("b", "a", "1500"), sample("a", "c", "1400")),
		"pmtu_probe_bytes":   vec(sample("a", "b", "1500"), sample("b", "a", "9000"), sample("a", "c", "1500")),
	}}
	m, err := matrix.Compute(context.Background(), q, "kconmon_ng", "pmtu")
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	cells := map[string]matrix.Cell{}
	for _, c := range m.Cells {
		cells[c.Source+"->"+c.Destination] = c
	}
	for _, k := range []string{"a->b", "a->c"} {
		if c, ok := cells[k]; ok {
			t.Errorf("%s has no pmtu verdict in the window and must have no cell, got failRatio=%v mtu=%v",
				k, c.FailRatio, c.MTUBytes)
		}
	}
	if ba, ok := cells["b->a"]; !ok || ba.MTUBytes == nil || *ba.MTUBytes != 1500 {
		t.Errorf("b->a has a verdict and must keep its MTU, got %+v", ba)
	}
}

// The probe size comes from the route to each peer: a 1420-byte VPN route and a 1500-byte LAN route
// from the same agent. Each pair is compared with its own probe size, never the agent's max.
func TestComputePMTUComparesEachPairWithItsOwnProbeSize(t *testing.T) {
	q := &fakeQuerier{byContains: map[string]string{
		"pmtu_results_total": vec(sample("a", "vpn", "0"), sample("a", "lan", "0")),
		"_pmtu_bytes)":       vec(sample("a", "vpn", "1420"), sample("a", "lan", "1400")),
		"pmtu_probe_bytes":   vec(sample("a", "vpn", "1420"), sample("a", "lan", "1500")),
	}}
	m, err := matrix.Compute(context.Background(), q, "kconmon_ng", "pmtu")
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	cells := map[string]matrix.Cell{}
	for _, c := range m.Cells {
		cells[c.Destination] = c
	}
	if c := cells["vpn"]; c.ProbeMTUBytes == nil || *c.ProbeMTUBytes != 1420 {
		t.Errorf("a->vpn probeMtuBytes = %v, want its own 1420: the healthy VPN path is not reduced", c.ProbeMTUBytes)
	}
	if c := cells["lan"]; c.ProbeMTUBytes == nil || *c.ProbeMTUBytes != 1500 {
		t.Errorf("a->lan probeMtuBytes = %v, want 1500: 1400 on it is a real reduction", c.ProbeMTUBytes)
	}
}

// The 5m fail ratio cannot tell a recovered pair from an ECMP-split black hole whose last probe
// crossed; the recent window can. It rides on pmtu cells only and never makes a cell of its own.
func TestComputePMTURecentFailRatio(t *testing.T) {
	q := &fakeQuerier{byContains: map[string]string{
		"[5m]":             vec(sample("a", "b", "0.4"), sample("a", "c", "0.4"), sample("a", "d", "0.2")),
		"[3m]":             vec(sample("a", "b", "0"), sample("a", "c", "0.5"), sample("a", "d", "NaN"), sample("x", "y", "0")),
		"_pmtu_bytes)":     vec(sample("a", "b", "1500"), sample("a", "c", "1500"), sample("a", "d", "1500")),
		"pmtu_probe_bytes": vec(sample("a", "b", "1500"), sample("a", "c", "1500"), sample("a", "d", "1500")),
	}}
	m, err := matrix.Compute(context.Background(), q, "kconmon_ng", "pmtu")
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	cells := map[string]matrix.Cell{}
	for _, c := range m.Cells {
		cells[c.Source+"->"+c.Destination] = c
	}
	if c := cells["a->b"]; c.RecentFailRatio == nil || *c.RecentFailRatio != 0 {
		t.Errorf("a->b recentFailRatio = %v, want 0: failures in the window, clean since", c.RecentFailRatio)
	}
	if c := cells["a->c"]; c.RecentFailRatio == nil || *c.RecentFailRatio != 0.5 {
		t.Errorf("a->c recentFailRatio = %v, want 0.5", c.RecentFailRatio)
	}
	if c := cells["a->d"]; c.RecentFailRatio != nil {
		t.Errorf("a->d recentFailRatio = %v, want none: no probe in the recent window", *c.RecentFailRatio)
	}
	if _, ok := cells["x->y"]; ok {
		t.Error("a recent-window sample alone must not make a cell")
	}
	if len(m.Nodes) != 4 {
		t.Errorf("nodes = %v, want a, b, c, d", m.Nodes)
	}
}

func TestComputeTCPHasNoRecentFailRatio(t *testing.T) {
	q := &fakeQuerier{byContains: map[string]string{"results_total": vec(sample("a", "b", "0.25"))}}
	m, err := matrix.Compute(context.Background(), q, "kconmon_ng", "tcp")
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if len(m.Cells) != 1 || m.Cells[0].RecentFailRatio != nil {
		t.Errorf("tcp cells = %+v, want no recentFailRatio", m.Cells)
	}
}
