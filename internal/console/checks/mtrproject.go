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

// silentHop reports whether hop carries no usable address -- a TTL that never produced an answer.
func silentHop(ip string) bool {
	ip = strings.TrimSpace(ip)
	return ip == "" || ip == "*"
}

// normalizeHops turns an agent's hop list into the snapshot payload: silent hops dropped; the hole
// is information -- it says two routers on this path did not answer.
func normalizeHops(hops []model.MTRHop) []store.PathHop {
	out := make([]store.PathHop, 0, len(hops))
	for i := range hops {
		if silentHop(hops[i].IP) {
			continue
		}
		out = append(out, store.PathHop{
			Number:    hops[i].Number,
			IP:        strings.TrimSpace(hops[i].IP),
			Hostname:  hops[i].Hostname,
			RTTNs:     hops[i].RTT.Nanoseconds(),
			LossRatio: hops[i].LossRatio,
		})
	}
	return out
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

	return store.PathSnapshotInput{
		SourceNode:  pair.Source,
		Destination: pair.Destination.Label(),
		// The hash is computed here and handed over rather than left to Validate to derive.
		PathHash: store.HashPath(hops),
		Hops:     hops,
		SeenAt:   observedAt,
		RunID:    runID,
	}, true
}
