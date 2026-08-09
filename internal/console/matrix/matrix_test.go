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

// Prometheus serializes 0/0 (a pair whose series went stale mid-window) and
// empty-bucket histogram_quantile as the STRING "NaN" — and
// strconv.ParseFloat accepts "NaN" without error, so it used to flow into the
// cell and kill json.Marshal for the WHOLE matrix: the REST handler answered
// 200 with an empty body ("Unexpected end of JSON input" in the UI) and the
// WS pusher dropped every snapshot. Caught live: pausing one fleet node took
// the entire matrix API down five minutes later. NaN/±Inf is "no sample",
// never a value.
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
