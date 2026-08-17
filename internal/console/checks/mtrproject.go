package checks

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

// mtrResult is the projector's typed view of one stored check_results.result payload; it is
// deliberately NOT model.CheckResult: that type carries Details as `any`.
type mtrResult struct {
	Type    model.CheckType  `json:"type"`
	Details model.MTRDetails `json:"details"`
}

// normalizeHops turns an agent's hop list into the snapshot payload: EVERY hop is kept, silent ones
// included, because a "*" at TTL 1 and 2 before the answer at TTL 3 is the network path -- two layers
// that did not answer, not a one-hop link. Path IDENTITY still ignores the silent hops (HashPath),
// so a flapping "*" does not read as a route change.
func normalizeHops(hops []model.MTRHop) []store.PathHop {
	out := make([]store.PathHop, 0, len(hops))
	for i := range hops {
		ip := strings.TrimSpace(hops[i].IP)
		if ip == "" {
			ip = "*"
		}
		out = append(out, store.PathHop{
			Number:    hops[i].Number,
			IP:        ip,
			Hostname:  hops[i].Hostname,
			RTTNs:     hops[i].RTT.Nanoseconds(),
			LossRatio: hops[i].LossRatio,
		})
	}
	return out
}

// TraceFromResult reads ONE stored check_results.result payload back as the hops it recorded plus
// the path identity those hops carry.
//
// It is the read-side twin of ProjectMTRSnapshot's own first half, and it exists because the path
// history folds N traces into one route row: the console could say "147 traces" and then had no way
// to show any of them. Answering "which of the stored traces walked THIS route" means recomputing
// each trace's identity the same way the projector did — the same normalizeHops, the same HashPath,
// the same silent-path fallback — so a trace can never be listed under a route it did not walk.
//
// ok is false for anything that is not a readable mtr trace: another check type, an unparseable
// payload, a result with no hops at all (a probe that never left the dispatcher records an error,
// not a path).
func TraceFromResult(resultJSON json.RawMessage) (hops []store.PathHop, pathHash string, ok bool) {
	if len(resultJSON) == 0 {
		return nil, "", false
	}
	var res mtrResult
	if err := json.Unmarshal(resultJSON, &res); err != nil {
		return nil, "", false
	}
	if res.Type != "" && res.Type != model.CheckMTR {
		return nil, "", false
	}
	hops = normalizeHops(res.Details.Hops)
	if len(hops) == 0 {
		return nil, "", false
	}
	hash := store.HashPath(hops)
	if hash == "" {
		hash = store.HashSilentPath()
	}
	return hops, hash, true
}

// ProjectMTRSnapshot turns one stored mtr result into the path-history row it describes; only a
// store failure downstream is an error.
func ProjectMTRSnapshot(spec *Spec, pair *Pair, resultJSON json.RawMessage, observedAt time.Time, runID string) (store.PathSnapshotInput, bool) {
	// The nil guards are not defensive noise: this runs inside one pair's dispatch goroutine.
	if spec == nil || pair == nil || spec.Type != string(model.CheckMTR) {
		return store.PathSnapshotInput{}, false
	}
	if len(resultJSON) == 0 {
		return store.PathSnapshotInput{}, false
	}

	var res mtrResult
	if err := json.Unmarshal(resultJSON, &res); err != nil {
		return store.PathSnapshotInput{}, false
	}
	// A payload that names a type at all must name mtr.
	if res.Type != "" && res.Type != model.CheckMTR {
		return store.PathSnapshotInput{}, false
	}

	hops := normalizeHops(res.Details.Hops)
	if len(hops) == 0 {
		return store.PathSnapshotInput{}, false
	}
	/* An empty hash means no hop answered. That is not "nothing to record": the probe ran, and a
	   destination whose every trace is silent — an external target behind a NAT that eats ICMP
	   TTL-exceeded, say — disappeared from MTR Explorer entirely, so a run the operator watched
	   succeed left no trace on the page. It gets the one silent identity instead. */
	hash := store.HashPath(hops)
	if hash == "" {
		hash = store.HashSilentPath()
	}

	return store.PathSnapshotInput{
		SourceNode:  pair.Source,
		Destination: pair.Destination.Label(),
		PathHash:    hash,
		Hops:        hops,
		SeenAt:      observedAt,
		RunID:       runID,
	}, true
}
