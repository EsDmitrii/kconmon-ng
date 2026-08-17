package checks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// defaultContinuousInterval is the cadence a continuous external check is pushed to agents with;
// the search, so the next person does not repeat.
const defaultContinuousInterval = 30 * time.Second

// externalResyncInterval is how often the desired state is re-PUT even when nothing about it has
// changed; see leaderTick's skip.
const externalResyncInterval = 2 * time.Minute

// defaultContinuousTimeout is the per-probe timeout pushed alongside the interval; it is
// deliberately a small fraction of the interval so a hung probe cannot overlap.
const defaultContinuousTimeout = 5 * time.Second

// schedulePageSize is one ListSchedules page, at the store's own clamp
// ceiling: the reconciler wants every continuous schedule, so the fewest
// possible round trips is the right shape.
const schedulePageSize = 500

// maxReconcileSchedules bounds how many check_schedules rows one tick will page through in total;
// the bound exists because a tick holds the fleet-wide advisory lock for as long as it runs (see
// Reconciler's doc comment).
const maxReconcileSchedules = 2000

// kindContinuous is the check_schedules.kind value this loop consumes; it is a deliberate copy of
// the scheduler's own constant rather than an import.
const kindContinuous = "continuous"

// ExternalReconciles result labels -- the closed set metrics.go documents.
const (
	reconcilePushed    = "pushed"
	reconcileUnchanged = "unchanged"
	reconcileNotLeader = "not-leader"
	reconcileError     = "error"
)

// ExternalSpecsSkipped reason labels -- the closed set metrics.go documents.
const (
	skipCheckType       = "check-type"
	skipDestinationKind = "destination-kind"
	// skipUnrunnable is a definition every agent would refuse to parse -- checkType=http against a
	// target of kind host, checkType=dns with no params.query. Pushing it wastes a slot in every
	// agent's assignment and produces nothing but one WARN per agent per push, forever, while the
	// Console goes on listing the definition as enabled. The write routes refuse these at the door
	// (see httpapi.refuseUnrunnableDefinition); this is the backstop for a row that predates the
	// guard or was written straight to the database.
	skipUnrunnable = "unrunnable"
)

// externalCheckTypes mirrors internal/controller's own validExternalCheckTypes
// (internal/controller/external.go) and; enforcing it HERE, before the PUT, is not defensive
// tidiness.
var externalCheckTypes = map[string]bool{"tcp": true, "icmp": true, "dns": true, "http": true}

// Locker is the cross-replica mutual-exclusion seam, satisfied by *store.DB --
// the same interface (and the same (false, nil) = "someone else has it"
// contract) the scheduler declares for its own tick.
type Locker interface {
	WithAdvisoryLock(ctx context.Context, key int64, fn func(context.Context) error) (bool, error)
}

// ReconcileStore is everything the reconciler reads in PostgreSQL: the schedules it filters for
// kind='continuous'.
type ReconcileStore interface {
	ListSchedules(ctx context.Context, f store.ScheduleFilter) (store.SchedulePage, error)
	GetDefinition(ctx context.Context, id string) (store.Definition, error)
	GetTarget(ctx context.Context, id string) (store.Target, error)
}

// TopologySource is the live agent snapshot a definition's source_selection is
// resolved against, the same narrow interface the scheduler and httpapi's
// projection guard each declare for the same reason.
type TopologySource interface {
	Topology(ctx context.Context) (*controllerclient.Topology, error)
}

// externalChecksAPI is the subset of *controllerclient.Client the reconciler needs; unexported like
// controllerAPI: a real *controllerclient.Client satisfies it structurally.
type externalChecksAPI interface {
	PutExternalChecks(ctx context.Context, agents map[string][]controllerclient.ExternalCheckSpec) (*controllerclient.ExternalChecksResult, error)
}

var (
	_ Locker            = (*store.DB)(nil)
	_ ReconcileStore    = (*store.DB)(nil)
	_ TopologySource    = (*controllerclient.Client)(nil)
	_ externalChecksAPI = (*controllerclient.Client)(nil)
)

// ReconcilerDeps is everything NewReconciler needs.
type ReconcilerDeps struct {
	Lock       Locker
	Store      ReconcileStore
	Topology   TopologySource
	Controller externalChecksAPI
	Metrics    *metrics.Metrics
	// Interval is the reconcile cadence.
	Interval time.Duration
	// LockKey is the advisory-lock key each tick runs under.
	LockKey int64
}

// Reconciler is the continuous external-check assignment loop; this loop pushes the COMPLETE
// desired state on a timer.
type Reconciler struct {
	lock     Locker
	store    ReconcileStore
	topology TopologySource
	ctrl     externalChecksAPI
	m        *metrics.Metrics
	interval time.Duration
	lockKey  int64

	// lastPushedAt is when that PUT happened; see externalResyncInterval.
	lastPushedAt time.Time
	// lastPushed is the JSON fingerprint of the desired state this replica most recently PUT
	// successfully; the controller's own change detection (ExternalCheckManager.Apply) absorbs.
	lastPushed []byte

	// warnedSkips remembers which definition IDs have already been logged as skipped.
	warnedSkips map[string]struct{}
}

// NewReconciler returns a Reconciler. Run is what starts it.
/*
 * ReconcilerLockKey is this loop's OWN advisory key.
 *
 * main.go handed it the scheduler's key, so the two loops — which do unrelated work on the same
 * tick interval — took turns on one lock: every pass of one was a skipped pass of the other, at half
 * the cadence each was configured for. It is crc32.Checksum([]byte("kconmon-ng.checks.Reconciler"),
 * crc32.MakeTable(crc32.IEEE)).
 */
const ReconcilerLockKey int64 = 3318800038

func NewReconciler(d ReconcilerDeps) *Reconciler { //nolint:gocritic // hugeParam: ReconcilerDeps mirrors scheduler.Deps' value semantics -- a named-field composition root, built once at boot
	if d.LockKey == 0 {
		d.LockKey = ReconcilerLockKey
	}
	return &Reconciler{
		lock:        d.Lock,
		store:       d.Store,
		topology:    d.Topology,
		ctrl:        d.Controller,
		m:           d.Metrics,
		interval:    d.Interval,
		lockKey:     d.LockKey,
		warnedSkips: make(map[string]struct{}),
	}
}

// Run ticks every Interval until ctx is cancelled, and returns promptly when it is; there is
// deliberately no immediate first tick.
func (r *Reconciler) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Tick(ctx)
		}
	}
}

// Tick runs exactly one attempt, wrapped in the advisory lock; exported so the tests drive single,
// deterministic ticks instead of racing a ticker.
func (r *Reconciler) Tick(ctx context.Context) {
	locked, err := r.lock.WithAdvisoryLock(ctx, r.lockKey, r.leaderTick)
	switch {
	case err != nil:
		// Both "could not take the lock" and "the reconcile itself failed" land here.
		r.m.ExternalReconciles.WithLabelValues(reconcileError).Inc()
		slog.Warn("checks: external-check reconcile failed", "error", err)
	case !locked:
		r.m.ExternalReconciles.WithLabelValues(reconcileNotLeader).Inc()
	}
}

// leaderTick is the work one tick does while holding the lock: compute the desired state.
func (r *Reconciler) leaderTick(ctx context.Context) error {
	desired, series, err := r.desired(ctx)
	if err != nil {
		return err
	}

	// The fingerprint is the desired state's own JSON, which is canonical for this purpose without any
	// extra work.
	fingerprint, err := json.Marshal(desired)
	if err != nil {
		return fmt.Errorf("checks: reconcile: encode desired state: %w", err)
	}

	/* The fingerprint skip is a local memo about a REMOTE state, so it is bounded in time as well as
	   by equality. The controller holds its assignments in memory: a restart (a rolling update, an
	   OOM kill, a node drain — the chart runs one replica by default) or a leadership move leaves the
	   new process with none, every agent re-subscribes and is sent an EMPTY assignment, and this
	   reconciler said "unchanged" forever after. Continuous external checks then stopped, silently,
	   until something else happened to alter the desired state.
	   A periodic full resync is the standard answer and the cheap one: the PUT replaces the whole
	   assignment state, so re-sending it is idempotent, and one small request every few minutes is
	   nothing beside what it repairs. */
	if r.lastPushed != nil && bytes.Equal(r.lastPushed, fingerprint) && time.Since(r.lastPushedAt) < externalResyncInterval {
		r.m.ExternalSeriesProjected.WithLabelValues().Set(float64(series))
		r.m.ExternalReconciles.WithLabelValues(reconcileUnchanged).Inc()
		return nil
	}

	res, err := r.ctrl.PutExternalChecks(ctx, desired)
	if err != nil {
		// lastPushed is deliberately NOT updated: the controller may have applied nothing (a 400 rejects
		// the whole body) or everything (a response lost on the way back).
		return fmt.Errorf("checks: reconcile: put external checks: %w", err)
	}

	r.lastPushed = fingerprint
	r.lastPushedAt = time.Now()
	r.m.ExternalSeriesProjected.WithLabelValues().Set(float64(series))
	r.m.ExternalReconciles.WithLabelValues(reconcilePushed).Inc()
	slog.Info("checks: pushed continuous external-check assignment",
		"agents", res.Agents, "changed", res.Changed, "projectedSeries", series)
	if len(res.Unknown) > 0 {
		// Not an error: the Console's topology view can legitimately lag the
		// controller's registry, and the next tick corrects it. Worth a line
		// because a PERSISTENT mismatch means those agents are probing nothing.
		slog.Warn("checks: controller does not know some agents in the assignment",
			"agents", len(res.Unknown))
	}
	return nil
}

// desired computes the complete per-agent assignment to PUT, keyed by the CONTROLLER's agent ID;
// the results are named for readability only -- every return below is explicit.
func (r *Reconciler) desired(ctx context.Context) (assignment map[string][]controllerclient.ExternalCheckSpec, projectedSeries int, err error) {
	defs, specs, err := r.continuousDefinitions(ctx)
	if err != nil {
		return nil, 0, err
	}

	topo, err := r.topology.Topology(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("checks: reconcile: read topology: %w", err)
	}
	agents, agentIDByNode := agentRefs(topo)

	assignments, series, err := AssignAgents(defs, agents)
	if err != nil {
		// Returned bare, exactly as continuousDefinitions' error is above: AssignAgents already says
		// "checks: assign: ...".
		return nil, 0, err
	}

	out := make(map[string][]controllerclient.ExternalCheckSpec, len(assignments))
	for node, assignment := range assignments {
		agentID, ok := agentIDByNode[node]
		if !ok {
			// Unreachable: every node name AssignAgents can return came from the very snapshot agentIDByNode
			// was built from.
			continue
		}
		assigned := make([]controllerclient.ExternalCheckSpec, 0, len(assignment.Specs))
		for i := range assignment.Specs {
			spec, known := specs[assignment.Specs[i].DefinitionID]
			if !known {
				continue
			}
			assigned = append(assigned, spec)
		}
		if len(assigned) == 0 {
			continue
		}
		out[agentID] = assigned
	}
	return out, series, nil
}

// agentRefs projects a topology snapshot onto the agent list AssignAgents takes; the planner keys
// on NODE NAME -- that is what httpapi's projection guard and the scheduler's one-per-zone
// resolution both count.
func agentRefs(topo *controllerclient.Topology) (agents []AgentRef, agentIDByNode map[string]string) {
	byNode := make(map[string]AgentRef, len(topo.Agents))
	agentIDByNode = make(map[string]string, len(topo.Agents))
	for i := range topo.Agents {
		a := &topo.Agents[i]
		if a.ID == "" || a.NodeName == "" {
			continue
		}
		if cur, dup := agentIDByNode[a.NodeName]; dup && cur <= a.ID {
			continue
		}
		agentIDByNode[a.NodeName] = a.ID
		byNode[a.NodeName] = AgentRef{NodeName: a.NodeName, Zone: a.Zone}
	}

	agents = make([]AgentRef, 0, len(byNode))
	for _, ref := range byNode {
		agents = append(agents, ref)
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].NodeName < agents[j].NodeName })
	return agents, agentIDByNode
}

// continuousDefinitions reads every enabled kind='continuous' schedule's definition and returns
// both the planner's view of them (what AssignAgents takes) and the per-definition
// ExternalCheckSpec the PUT body carries; a DISABLED definition is still handed to the planner.
func (r *Reconciler) continuousDefinitions(ctx context.Context) ([]Definition, map[string]controllerclient.ExternalCheckSpec, error) {
	ids, err := r.continuousDefinitionIDs(ctx)
	if err != nil {
		return nil, nil, err
	}

	defs := make([]Definition, 0, len(ids))
	specs := make(map[string]controllerclient.ExternalCheckSpec, len(ids))
	for _, id := range ids {
		def, getErr := r.store.GetDefinition(ctx, id)
		if errors.Is(getErr, store.ErrNotFound) {
			// A definition deleted between the schedule listing and this read; ON DELETE CASCADE removes its
			// schedules too, so the next tick will not see.
			continue
		}
		if getErr != nil {
			return nil, nil, fmt.Errorf("checks: reconcile: get definition %s: %w", id, getErr)
		}

		if def.Enabled {
			spec, ok := r.specFor(&def)
			if !ok {
				continue
			}
			var specErr error
			spec, specErr = r.resolveTarget(ctx, &def, spec)
			if specErr != nil {
				return nil, nil, specErr
			}
			/* And the SAME parse every agent applies, before the push rather than after.
			   An agent that cannot parse a spec drops it with an agent-local WARN; nothing reached
			   the controller, nothing reached the Console, and the definition kept its "enabled"
			   badge while producing no result on any node. Running the parser here turns that into a
			   counted skip with a reason, next to the two skips this loop already reports. */
			if _, perr := checker.ParseExternalSpec(&checker.ExternalSpecInput{
				DefinitionID: spec.DefinitionID,
				Name:         spec.Target.Name,
				Address:      spec.Target.Address,
				Port:         spec.Target.Port,
				CheckType:    spec.CheckType,
				Interval:     time.Duration(spec.IntervalNs),
				Timeout:      time.Duration(spec.TimeoutNs),
				ParamsJSON:   spec.Params,
			}); perr != nil {
				r.skip(&def, skipUnrunnable,
					"checks: reconcile: skipping continuous definition, no agent can parse it: "+perr.Error())
				continue
			}
			specs[def.ID] = spec
		}

		defs = append(defs, Definition{
			ID:                 def.ID,
			Name:               def.Name,
			Selection:          Selection(def.SourceSelection),
			CheckType:          def.CheckType,
			DestinationAddress: def.DestinationAddress,
			Enabled:            def.Enabled,
		})
	}
	return defs, specs, nil
}

// continuousDefinitionIDs pages through check_schedules and returns the definition IDs of the
// enabled kind='continuous' rows.
func (r *Reconciler) continuousDefinitionIDs(ctx context.Context) ([]string, error) {
	var (
		ids    []string
		seen   = map[string]struct{}{}
		cursor string
		read   int
	)
	for {
		page, err := r.store.ListSchedules(ctx, store.ScheduleFilter{Cursor: cursor, Limit: schedulePageSize})
		if err != nil {
			return nil, fmt.Errorf("checks: reconcile: list schedules: %w", err)
		}
		for i := range page.Schedules {
			s := &page.Schedules[i]
			if s.Kind != kindContinuous || !s.Enabled {
				continue
			}
			if _, dup := seen[s.DefinitionID]; dup {
				continue
			}
			seen[s.DefinitionID] = struct{}{}
			ids = append(ids, s.DefinitionID)
		}
		read += len(page.Schedules)
		cursor = page.NextCursor
		if cursor == "" || len(page.Schedules) == 0 {
			return ids, nil
		}
		if read >= maxReconcileSchedules {
			// Truncated: the desired state below is INCOMPLETE.
			slog.Warn("checks: reconcile: schedule scan truncated, desired assignment may be incomplete",
				"scanned", read, "limit", maxReconcileSchedules)
			return ids, nil
		}
	}
}

// specFor builds the constant part of one enabled definition's ExternalCheckSpec and reports
// whether the definition is eligible to be pushed at all; an ineligible one is counted, warned
// about once, and left out of the desired state entirely.
func (r *Reconciler) specFor(def *store.Definition) (controllerclient.ExternalCheckSpec, bool) {
	if !externalCheckTypes[def.CheckType] {
		// mtr and udp. Skipping the DEFINITION, rather than letting the
		// controller reject it, is what keeps every other definition's
		// assignment alive: the handler answers 400 for the whole body.
		r.skip(def, skipCheckType,
			"checks: reconcile: skipping continuous definition, its check type cannot be a continuous external check")
		return controllerclient.ExternalCheckSpec{}, false
	}
	if def.DestinationKind == "node" {
		// A continuous check against cluster nodes is the agents' own peer mesh, which every agent
		// already runs unprompted.
		r.skip(def, skipDestinationKind,
			"checks: reconcile: skipping continuous definition, a node destination is the agents' own peer mesh, not an external check")
		return controllerclient.ExternalCheckSpec{}, false
	}

	return controllerclient.ExternalCheckSpec{
		DefinitionID: def.ID,
		CheckType:    def.CheckType,
		IntervalNs:   defaultContinuousInterval.Nanoseconds(),
		TimeoutNs:    defaultContinuousTimeout.Nanoseconds(),
		Params:       def.Params,
	}, true
}

// resolveTarget fills spec's ExternalTarget from the definition's destination.
func (r *Reconciler) resolveTarget(ctx context.Context, def *store.Definition, spec controllerclient.ExternalCheckSpec) (controllerclient.ExternalCheckSpec, error) { //nolint:gocritic // hugeParam: ExternalCheckSpec is a value-type wire payload, taken and returned so the caller never holds a half-built spec
	switch def.DestinationKind {
	case "target":
		target, err := r.store.GetTarget(ctx, def.DestinationTargetID)
		if err != nil {
			return spec, fmt.Errorf("checks: reconcile: definition %s: get destination target %s: %w",
				def.ID, def.DestinationTargetID, err)
		}
		spec.Target = externalTarget(target.Name, target.Kind, target.Address)
	default: // "adhoc" -- "node" was already refused by specFor
		// Name is the DEFINITION's name, never the raw address, for the reason the scheduler's specFor
		// states.
		spec.Target = externalTarget(def.Name, adhocTargetKind(def.DestinationAddress), def.DestinationAddress)
	}
	return spec, nil
}

// externalTarget splits an "address" the way pb.ExternalTarget wants it; the split is attempted for
// kind "host".
func externalTarget(name, kind, address string) controllerclient.ExternalTarget {
	t := controllerclient.ExternalTarget{Name: name, Kind: kind, Address: address}
	if kind != "host" {
		return t
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return t
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return t
	}
	t.Address, t.Port = host, uint32(n) //nolint:gosec // G115: n is range-checked to [1,65535] on the line above
	return t
}

// adhocTargetKind classifies an ad-hoc destination address into the host|url set store's own
// targetKinds defines.
func adhocTargetKind(address string) string {
	lower := strings.ToLower(address)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return "url"
	}
	return "host"
}

// skip counts one definition left out of the desired state and logs it the
// first time this process sees that definition -- see warnedSkips for why the
// two cadences differ.
func (r *Reconciler) skip(def *store.Definition, reason, msg string) {
	r.m.ExternalSpecsSkipped.WithLabelValues(reason).Inc()
	if _, warned := r.warnedSkips[def.ID]; warned {
		return
	}
	r.warnedSkips[def.ID] = struct{}{}
	slog.Warn(msg, "definition", def.ID, "checkType", def.CheckType, "destinationKind", def.DestinationKind)
}
