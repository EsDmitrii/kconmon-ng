package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
)

/*
 * The topology sweeper (roadmap M10-3): a slow census of the pairs a sparse topology stopped
 * probing. One pair per interval goes through the same controller dispatch the on-demand runs use
 * (Diagnose), and the ONLY record of the outcome is the zone-aggregated SweepResults counter.
 *
 * That narrowness is the point, verified against what the run pipeline writes today: a run persists
 * a check_runs row, per-pair check_results rows and per-pair WebSocket frames — recreating exactly
 * the per-pair footprint sparse mode exists to shed, and polluting the operator's run history with
 * one machine-made run per minute. So the sweeper deliberately does NOT go through Runner.Start; it
 * dispatches one probe and increments one zone-labeled counter, nothing else.
 */

// defaultSweepInterval is the census cadence when the config names none.
const defaultSweepInterval = time.Minute

// sweeperLockKey is this loop's own advisory key, same convention as ReconcilerLockKey:
// crc32.ChecksumIEEE([]byte("kconmon-ng.checks.Sweeper")).
const sweeperLockKey int64 = 2749161301

// intendedMetric is the controller's plan gauge (roadmap M10-2): value 1 for every pair the current
// topology plan assigns, stale series deleted on plan change. The name is a cross-component
// contract, hardcoded the same way the web matrix hardcodes its metric prefix.
const intendedMetric = "kconmon_ng_probe_intended"

// PairKey is one directed node pair, the sweeper's census unit.
type PairKey struct {
	Source      string
	Destination string
}

// IntendedPairsSource reports which directed pairs the CURRENT topology plan already probes; the
// sweeper subtracts them from its census. An error degrades to sweeping the full census — the
// superset is always safe, it just re-measures pairs the mesh covers anyway.
type IntendedPairsSource interface {
	IntendedPairs(ctx context.Context) (map[PairKey]struct{}, error)
}

// diagnoseAPI is the dispatch subset of *controllerclient.Client the sweeper needs.
type diagnoseAPI interface {
	Diagnose(ctx context.Context, req controllerclient.DiagnoseRequest, timeout time.Duration) (json.RawMessage, error)
}

var _ diagnoseAPI = (*controllerclient.Client)(nil)

// SweeperDeps is everything NewSweeper needs.
type SweeperDeps struct {
	// Lock is optional cross-replica election (the reconciler's own seam). Nil sweeps on every
	// replica: with R replicas that is R pairs per interval instead of one — a faster census, not a
	// correctness problem, so a database is not a hard requirement.
	Lock       Locker
	Topology   TopologySource
	Controller diagnoseAPI
	Metrics    *metrics.Metrics
	// Interval is the census cadence; nonpositive falls back to defaultSweepInterval.
	Interval time.Duration
	// CheckType defaults to "tcp".
	CheckType string
	// Timeout is the per-probe budget, clamped exactly as a run pair's would be.
	Timeout time.Duration
	// Intended is optional; nil sweeps the full pair census.
	Intended IntendedPairsSource
}

// Sweeper probes one rotating census pair per tick. See the package comment block above.
type Sweeper struct {
	lock      Locker
	topology  TopologySource
	ctrl      diagnoseAPI
	m         *metrics.Metrics
	interval  time.Duration
	checkType string
	timeout   time.Duration
	intended  IntendedPairsSource

	// now is the rotation clock; a test seam only. Deriving the census position from wall time
	// (rather than a local counter) keeps the rotation stable across restarts and replicas without
	// any stored cursor.
	now func() time.Time
}

// NewSweeper returns a Sweeper; Run starts it.
func NewSweeper(d SweeperDeps) *Sweeper { //nolint:gocritic // hugeParam: SweeperDeps mirrors ReconcilerDeps' value semantics -- a named-field composition root, built once at boot
	if d.Interval <= 0 {
		d.Interval = defaultSweepInterval
	}
	if d.CheckType == "" {
		d.CheckType = "tcp"
	}
	return &Sweeper{
		lock:      d.Lock,
		topology:  d.Topology,
		ctrl:      d.Controller,
		m:         d.Metrics,
		interval:  d.Interval,
		checkType: d.CheckType,
		timeout:   d.Timeout,
		intended:  d.Intended,
		now:       time.Now,
	}
}

// Run ticks every Interval until ctx is cancelled; no immediate first tick, same as the reconciler.
func (s *Sweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}

// Tick runs one sweep attempt, under the advisory lock when one is wired; exported so tests drive
// single deterministic ticks.
func (s *Sweeper) Tick(ctx context.Context) {
	if s.lock == nil {
		if err := s.leaderTick(ctx); err != nil {
			slog.Warn("checks: topology sweep failed", "error", err)
		}
		return
	}
	if _, err := s.lock.WithAdvisoryLock(ctx, sweeperLockKey, s.leaderTick); err != nil {
		// Both "could not take the lock" and "the sweep itself failed" land here; "someone else
		// holds it" is the silent normal case on every replica but one.
		slog.Warn("checks: topology sweep failed", "error", err)
	}
}

// leaderTick probes exactly one census pair and records its outcome into the zone counter.
func (s *Sweeper) leaderTick(ctx context.Context) error {
	topo, err := s.topology.Topology(ctx)
	if err != nil {
		return fmt.Errorf("checks: sweep: read topology: %w", err)
	}
	nodes, zoneOf := sweepNodes(topo)
	if len(nodes) < 2 {
		return nil // nothing to pair
	}

	pairs := s.censusPairs(ctx, nodes)
	if len(pairs) == 0 {
		return nil // the plan already probes every pair: nothing to census
	}

	// The census position comes from the wall clock, not from local state: tick N of the rotation
	// is the same pair on every replica and across restarts, and a topology change merely re-deals
	// the indices of a census that has no completeness deadline anyway.
	idx := int((s.now().UnixNano() / s.interval.Nanoseconds()) % int64(len(pairs)))
	pair := pairs[idx]

	result := s.probe(ctx, pair)
	s.m.SweepResults.WithLabelValues(zoneOf[pair.Source], zoneOf[pair.Destination], result).Inc()
	return nil
}

// censusPairs builds the ordered directed-pair universe minus the pairs the topology plan already
// probes. A plan that cannot be read degrades to the full census.
func (s *Sweeper) censusPairs(ctx context.Context, nodes []string) []PairKey {
	var planned map[PairKey]struct{}
	if s.intended != nil {
		var err error
		planned, err = s.intended.IntendedPairs(ctx)
		if err != nil {
			slog.Debug("checks: sweep: could not read the intended pair set, sweeping the full census", "error", err)
			planned = nil
		}
	}
	pairs := make([]PairKey, 0, len(nodes)*(len(nodes)-1))
	for _, src := range nodes {
		for _, dst := range nodes {
			if src == dst {
				continue
			}
			p := PairKey{Source: src, Destination: dst}
			if _, ok := planned[p]; ok {
				continue
			}
			pairs = append(pairs, p)
		}
	}
	return pairs
}

// probe dispatches one pair on the on-demand path and classifies the outcome into the same
// ok|failed|timeout vocabulary RunPairs uses.
func (s *Sweeper) probe(ctx context.Context, pair PairKey) string {
	req := controllerclient.DiagnoseRequest{
		Source: pair.Source, Destination: pair.Destination, Type: s.checkType, Plane: "pod",
	}
	raw, err := s.ctrl.Diagnose(ctx, req, clampTimeoutFor(s.checkType, s.timeout))
	if err != nil {
		if errors.Is(err, controllerclient.ErrCheckTimeout) {
			return "timeout"
		}
		slog.Debug("checks: sweep probe failed to dispatch",
			"source", pair.Source, "destination", pair.Destination, "error", err)
		return "failed"
	}
	var result struct {
		Success bool `json:"success"`
	}
	if decErr := json.Unmarshal(raw, &result); decErr != nil || !result.Success {
		return "failed"
	}
	return "ok"
}

// sweepNodes projects a topology snapshot onto a sorted, de-duplicated node list plus a node->zone
// lookup — the agent-backed nodes, same source Runner.resolveNodes reads.
func sweepNodes(topo *controllerclient.Topology) (nodes []string, zoneOf map[string]string) {
	zoneOf = make(map[string]string, len(topo.Agents))
	for i := range topo.Agents {
		a := &topo.Agents[i]
		if a.NodeName == "" {
			continue
		}
		if _, dup := zoneOf[a.NodeName]; dup {
			continue
		}
		zoneOf[a.NodeName] = a.Zone
		nodes = append(nodes, a.NodeName)
	}
	sort.Strings(nodes)
	return nodes, zoneOf
}

// promQuerier is the one promql.Client call PromIntendedPairs needs.
type promQuerier interface {
	Query(ctx context.Context, query string, ts time.Time) (json.RawMessage, error)
}

// PromIntendedPairs reads the topology plan out of Prometheus via the controller's intendedMetric
// gauge — the only place the console can learn the plan without a controller API change.
type PromIntendedPairs struct {
	prom promQuerier
}

// NewPromIntendedPairs returns a Prometheus-backed IntendedPairsSource.
func NewPromIntendedPairs(prom promQuerier) *PromIntendedPairs {
	return &PromIntendedPairs{prom: prom}
}

// IntendedPairs runs one instant query and projects the vector onto pair keys. max by collapses
// duplicate series (two controller replicas exporting through a leadership change).
func (p *PromIntendedPairs) IntendedPairs(ctx context.Context) (map[PairKey]struct{}, error) {
	raw, err := p.prom.Query(ctx, "max by (source_node, destination_node) ("+intendedMetric+")", time.Time{})
	if err != nil {
		return nil, fmt.Errorf("checks: sweep: query %s: %w", intendedMetric, err)
	}
	var envelope struct {
		Data struct {
			Result []struct {
				Metric struct {
					SourceNode      string `json:"source_node"`
					DestinationNode string `json:"destination_node"`
				} `json:"metric"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("checks: sweep: decode %s vector: %w", intendedMetric, err)
	}
	out := make(map[PairKey]struct{}, len(envelope.Data.Result))
	for _, r := range envelope.Data.Result {
		if r.Metric.SourceNode == "" || r.Metric.DestinationNode == "" {
			continue
		}
		out[PairKey{Source: r.Metric.SourceNode, Destination: r.Metric.DestinationNode}] = struct{}{}
	}
	return out, nil
}
