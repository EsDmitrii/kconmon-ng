// Package matrix computes the N×N node connectivity matrix from Prometheus instant queries over the
// agent probe metrics (docs/metrics.md).
package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"
)

// ErrBadProtocol rejects protocols the matrix does not support.
var ErrBadProtocol = errors.New("unsupported protocol")

// Querier is the Prometheus seam (satisfied by *promql.Client).
type Querier interface {
	Query(ctx context.Context, query string, ts time.Time) (json.RawMessage, error)
}

// Cell is one source→destination pair. Pointer fields distinguish "no data".
type Cell struct {
	Source      string   `json:"source"`
	Destination string   `json:"destination"`
	FailRatio   *float64 `json:"failRatio"`
	RTTP95      *int64   `json:"rttP95,omitempty"`
	LossRatio   *float64 `json:"lossRatio,omitempty"`
	// pmtu protocol only: the largest datagram that crossed the pair (bytes) and the size its source
	// probes at. MTUBytes below ProbeMTUBytes with a zero FailRatio is a reduced path, not a failure.
	MTUBytes      *int64 `json:"mtuBytes,omitempty"`
	ProbeMTUBytes *int64 `json:"probeMtuBytes,omitempty"`
}

// Matrix is the computed heatmap payload.
type Matrix struct {
	Protocol  string    `json:"protocol"`
	Plane     string    `json:"plane"`
	Nodes     []string  `json:"nodes"`
	Cells     []Cell    `json:"cells"`
	Timestamp time.Time `json:"timestamp"`
}

type promEnvelope struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

type pair struct{ src, dst string }

// Compute runs the per-protocol instant queries and folds them into a Matrix.
func Compute(ctx context.Context, q Querier, metricsPrefix, protocol string) (*Matrix, error) {
	var failQ, rttQ, lossQ, mtuQ string
	switch protocol {
	case "tcp":
		failQ = failRatioQuery(metricsPrefix, "tcp")
		rttQ = p95Query(metricsPrefix + "_tcp_total_duration_seconds_bucket")
	case "udp":
		failQ = failRatioQuery(metricsPrefix, "udp")
		rttQ = p95Query(metricsPrefix + "_udp_rtt_seconds_bucket")
		lossQ = lossQuery(metricsPrefix, "udp")
	case "icmp":
		failQ = failRatioQuery(metricsPrefix, "icmp")
		rttQ = p95Query(metricsPrefix + "_icmp_rtt_seconds_bucket")
		lossQ = lossQuery(metricsPrefix, "icmp")
	case "pmtu":
		failQ = failRatioQuery(metricsPrefix, "pmtu")
		mtuQ = `min by (source_node, destination_node) (` + metricsPrefix + `_pmtu_bytes)`
	default:
		return nil, fmt.Errorf("%q: %w", protocol, ErrBadProtocol)
	}

	fail, err := vectorByPair(ctx, q, failQ)
	if err != nil {
		return nil, err
	}
	rtt := map[pair]float64{}
	if rttQ != "" {
		if rtt, err = vectorByPair(ctx, q, rttQ); err != nil {
			return nil, err
		}
	}
	loss := map[pair]float64{}
	if lossQ != "" {
		if loss, err = vectorByPair(ctx, q, lossQ); err != nil {
			return nil, err
		}
	}
	mtu := map[pair]float64{}
	probe := map[string]float64{}
	if mtuQ != "" {
		if mtu, err = vectorByPair(ctx, q, mtuQ); err != nil {
			return nil, err
		}
		if probe, err = vectorBySource(ctx, q,
			`max by (source_node) (`+metricsPrefix+`_agent_pmtu_probe_bytes)`); err != nil {
			return nil, err
		}
	}

	nodeSet := map[string]struct{}{}
	pairSet := map[pair]struct{}{}
	for _, m := range []map[pair]float64{fail, rtt, loss, mtu} {
		for p := range m {
			nodeSet[p.src] = struct{}{}
			nodeSet[p.dst] = struct{}{}
			pairSet[p] = struct{}{}
		}
	}
	nodes := make([]string, 0, len(nodeSet))
	for n := range nodeSet {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)

	cells := make([]Cell, 0, len(pairSet))
	for p := range pairSet {
		c := Cell{Source: p.src, Destination: p.dst}
		if v, ok := fail[p]; ok {
			f := v
			c.FailRatio = &f
		}
		if v, ok := rtt[p]; ok {
			ns := int64(v * float64(time.Second))
			c.RTTP95 = &ns
		}
		if v, ok := loss[p]; ok {
			l := v
			c.LossRatio = &l
		}
		if v, ok := mtu[p]; ok {
			c.MTUBytes = new(int64(v))
			if pv, ok := probe[p.src]; ok {
				c.ProbeMTUBytes = new(int64(pv))
			}
		}
		cells = append(cells, c)
	}
	sort.Slice(cells, func(i, j int) bool {
		if cells[i].Source != cells[j].Source {
			return cells[i].Source < cells[j].Source
		}
		return cells[i].Destination < cells[j].Destination
	})

	return &Matrix{Protocol: protocol, Plane: "pod", Nodes: nodes, Cells: cells, Timestamp: time.Now().UTC()}, nil
}

func failRatioQuery(prefix, proto string) string {
	m := prefix + "_" + proto + "_results_total"
	return `sum by (source_node, destination_node) (rate(` + m + `{result="fail"}[5m])) / sum by (source_node, destination_node) (rate(` + m + `[5m]))`
}

func p95Query(bucketMetric string) string {
	return `histogram_quantile(0.95, sum by (source_node, destination_node, le) (rate(` + bucketMetric + `[5m])))`
}

func lossQuery(prefix, proto string) string {
	return `avg by (source_node, destination_node) (` + prefix + `_` + proto + `_packet_loss_ratio)`
}

// sample is one instant-vector element whose value parsed to a finite number.
type sample struct {
	metric map[string]string
	value  float64
}

func queryVector(ctx context.Context, q Querier, query string) ([]sample, error) {
	raw, err := q.Query(ctx, query, time.Time{})
	if err != nil {
		return nil, err
	}
	var env promEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode prometheus vector: %w", err)
	}
	out := make([]sample, 0, len(env.Data.Result))
	for _, r := range env.Data.Result {
		vs, ok := r.Value[1].(string)
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(vs, 64)
		if err != nil {
			continue
		}
		// ParseFloat ACCEPTS "NaN" and "+Inf" (no error), and Prometheus emits both.
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		out = append(out, sample{metric: r.Metric, value: v})
	}
	return out, nil
}

func vectorByPair(ctx context.Context, q Querier, query string) (map[pair]float64, error) {
	samples, err := queryVector(ctx, q, query)
	if err != nil {
		return nil, err
	}
	out := make(map[pair]float64, len(samples))
	for _, smp := range samples {
		s, d := smp.metric["source_node"], smp.metric["destination_node"]
		if s == "" || d == "" {
			continue
		}
		out[pair{s, d}] = smp.value
	}
	return out, nil
}

// vectorBySource is vectorByPair for a series keyed by source_node alone: the agent-level gauges.
func vectorBySource(ctx context.Context, q Querier, query string) (map[string]float64, error) {
	samples, err := queryVector(ctx, q, query)
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(samples))
	for _, smp := range samples {
		if s := smp.metric["source_node"]; s != "" {
			out[s] = smp.value
		}
	}
	return out, nil
}
