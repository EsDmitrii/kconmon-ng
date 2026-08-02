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
