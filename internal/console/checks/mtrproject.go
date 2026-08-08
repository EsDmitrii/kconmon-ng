package checks

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

// mtrResult is the projector's typed view of one stored check_results.result
// payload.
//
// It is deliberately NOT model.CheckResult: that type carries Details as
// `any`, so getting an MTRDetails back out of a decoded one means
// re-marshalling the map json produced and decoding it a second time -- a
// round trip whose only possible effect is to lose or mangle something.
// Naming just the two fields the projection reads decodes the stored bytes
// exactly once. Every other field of the result (source, duration, error,
// success) is ignored on purpose: the check_results row is already the
// authority on those, and a snapshot repeating them would be a second,
// divergable copy.
type mtrResult struct {
	Type    model.CheckType  `json:"type"`
	Details model.MTRDetails `json:"details"`
}

// silentHop reports whether hop carries no usable address -- a TTL that never
// produced an answer. internal/checker/mtr.go writes "*" for both a read
// timeout and an unparseable reply; "" covers a payload from any other
// producer that simply left the field out. This is the same predicate
// internal/cli/format.go applies when it renders a trace, spelled once more
// here because path history needs it for a stronger reason than display does:
// a silent hop must never reach HashPath. ICMP rate limiting makes a given
// router answer one trace and ignore the next, so hashing "*" would make a
// perfectly stable route look like a NEW path every time an intermediate
// router felt like staying quiet -- turning mtr_path_snapshots into a
// write-only log and new-path into noise.
func silentHop(ip string) bool {
	ip = strings.TrimSpace(ip)
	return ip == "" || ip == "*"
}

// normalizeHops turns an agent's hop list into the snapshot payload: silent
// hops dropped, everything else carried across in order, durations in
// nanoseconds (the repo-wide convention on the wire and in storage alike).
//
// The kept hops keep the NUMBER the agent gave them. That is the deliberate
// half of this function: after dropping hops 3 and 4, hop 5 is still hop 5,
// so the stored list reads 1, 2, 5 with a visible hole. The hole is
// information -- it says two routers on this path did not answer -- and
// renumbering would not only erase it, it would make a trace through a silent
// hop indistinguishable from a genuinely shorter trace when the two are
// diffed side by side (Task 8's whole job).
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

// ProjectMTRSnapshot turns one stored mtr result into the path-history row it
// describes (M5 Decision 1). It is pure: no I/O, no clock, no store --
// observedAt and runID are supplied by the caller -- which is what lets the
// interesting half of this feature be tested without a database or a
// controller anywhere in sight.
//
// The second return is "there is something to project", NOT "this succeeded".
// false is an ordinary, expected answer on the runner's ingest path and never
// an error worth counting: a non-mtr run, a pair whose dispatch failed and
// carries no payload, the empty-object placeholder runOne writes for the NOT
// NULL result column, a malformed payload, and a trace whose every hop was
// silent all take that branch. Only a store failure downstream is an error.
//
// observedAt becomes the snapshot's SeenAt, and it is the CONSOLE's clock on
// purpose even though the payload carries the agent's own Timestamp: last_seen
// is half of the keyset ListPathSnapshots pages by, so one agent with a skewed
// clock would otherwise be able to wedge a row permanently at the top (or
// bottom) of every pair's history. The Console reads one clock; the agent's
// timestamp stays in the result row, where it describes the trace rather than
// ordering the table.
//
// Destination is pair.Destination.Label() -- the metric-safe NAME. An address
// must never become the key of a history row, exactly as it never becomes a
// metric label value (Destination's own doc comment).
func ProjectMTRSnapshot(spec *Spec, pair *Pair, resultJSON json.RawMessage, observedAt time.Time, runID string) (store.PathSnapshotInput, bool) {
	// The nil guards are not defensive noise: this runs inside one pair's
	// dispatch goroutine, where a panic would be recovered as a FAILED PAIR
	// (runOneRecovered) and so would report a network failure that never
	// happened.
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
	// A payload that names a type at all must name mtr. The spec gate above is
	// the primary one; this catches the case the spec cannot see -- a result
	// whose own type contradicts the run it arrived under, which is not a
	// trace whatever the request said. An absent type ("") is left to the hop
	// check below, which is what rejects runOne's `{}` placeholder.
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
		// The hash is computed here and handed over rather than left to
		// Validate to derive: this is the layer that decided which hops count,
		// and passing it makes the store CROSS-CHECK the two agree instead of
		// simply trusting whatever list arrived.
		PathHash: store.HashPath(hops),
		Hops:     hops,
		SeenAt:   observedAt,
		RunID:    runID,
	}, true
}
