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

	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// defaultContinuousInterval is the cadence a continuous external check is
// pushed to agents with, and it is a CONSTANT because nothing in the data
// model can express one yet.
//
// The search, so the next person does not repeat it: check_schedules is the
// only row that carries a cadence, and kind='continuous' is forbidden from
// carrying one -- store.ScheduleInput.Validate rejects a non-zero interval_ns
// on it, deliberately ("'continuous' because it has no cadence at all"). That
// rule was written about the CONSOLE-side firing cadence (a continuous check
// is never "fired" by the scheduler), but it also leaves the AGENT-side probe
// cadence -- which pb.ExternalCheckSpec.interval_ns requires -- with nowhere
// to live. check_definitions.params is free-form JSON, but no schema, API
// document, validation rule or UI field defines a key in it for this, and
// inventing an unvalidated magic key would turn an operator's typo into a
// silently-wrong probe rate with no feedback anywhere. TARGETS.md describes
// continuous checks ("agents probe targets in the normal checker loop") but
// names no number.
//
// So: one documented default for the whole fleet, matching the agents' own
// normal checker loop cadence, pending an explicit field. The natural home
// for that field is check_schedules.interval_ns with the continuous
// prohibition relaxed to mean "the probe interval" -- one migration-free
// validation change plus a read here.
const defaultContinuousInterval = 30 * time.Second

// defaultContinuousTimeout is the per-probe timeout pushed alongside the
// interval, and exists for exactly the same reason: pb.ExternalCheckSpec
// requires a timeout_ns and no stored row carries one. It is deliberately a
// small fraction of the interval so a hung probe cannot overlap the next one.
const defaultContinuousTimeout = 5 * time.Second

// schedulePageSize is one ListSchedules page, at the store's own clamp
// ceiling: the reconciler wants every continuous schedule, so the fewest
// possible round trips is the right shape.
const schedulePageSize = 500

// maxReconcileSchedules bounds how many check_schedules rows one tick will
// page through in total. There is no "WHERE kind='continuous' AND enabled"
// query -- adding one is a store change this task does not make -- so the
// filter runs here, over pages of the general listing. The bound exists
// because a tick holds the fleet-wide advisory lock for as long as it runs
// (see Reconciler's doc comment): an unbounded scan over a table that grew
// without anyone noticing would starve the scheduler sharing that lock.
const maxReconcileSchedules = 2000

// kindContinuous is the check_schedules.kind value this loop consumes. It is
// a deliberate copy of the scheduler's own constant rather than an import:
// internal/console/scheduler imports THIS package, so the dependency can only
// point one way.
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
)

// externalCheckTypes mirrors internal/controller's own validExternalCheckTypes
// (internal/controller/external.go) and, through it, pb.ExternalCheckSpec's
// "tcp|icmp|dns|http" comment. It is a deliberate COPY, not an import -- the
// same posture validCheckTypes takes toward the controller's diagnostics set.
//
// Enforcing it HERE, before the PUT, is not defensive tidiness: the
// controller's handler answers 400 for one ineligible spec and applies
// NOTHING, so a single mtr definition an operator saved would cost every other
// agent in the fleet its entire assignment, on every tick, until someone
// noticed.
var externalCheckTypes = map[string]bool{"tcp": true, "icmp": true, "dns": true, "http": true}

// Locker is the cross-replica mutual-exclusion seam, satisfied by *store.DB --
// the same interface (and the same (false, nil) = "someone else has it"
// contract) the scheduler declares for its own tick.
type Locker interface {
	WithAdvisoryLock(ctx context.Context, key int64, fn func(context.Context) error) (bool, error)
}

// ReconcileStore is everything the reconciler reads in PostgreSQL: the
// schedules it filters for kind='continuous', the definitions they name, and
// the targets those definitions point at. A narrow local interface over
// *store.DB, exactly as Runner's Store and the scheduler's Store are -- the
// unit tests drive whole ticks against a fake with no database at all.
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

// externalChecksAPI is the subset of *controllerclient.Client the reconciler
// needs. Unexported like controllerAPI: a real *controllerclient.Client
// satisfies it structurally, so no caller has to name it, and a test can hand
// in a fake that fails persistently -- which a real controller cannot be asked
// to do on demand.
type externalChecksAPI interface {
	PutExternalChecks(ctx context.Context, agents map[string][]controllerclient.ExternalCheckSpec) (*controllerclient.ExternalChecksResult, error)
}

var (
	_ Locker            = (*store.DB)(nil)
	_ ReconcileStore    = (*store.DB)(nil)
	_ TopologySource    = (*controllerclient.Client)(nil)
	_ externalChecksAPI = (*controllerclient.Client)(nil)
)

// ReconcilerDeps is everything NewReconciler needs. Every field is REQUIRED --
// unlike the scheduler, which can degrade without a topology source, there is
// no useful half of this loop: no store means no continuous definitions to
// read, no topology means no agents to assign them to, and no controller means
// nowhere to push the result.
type ReconcilerDeps struct {
	Lock       Locker
	Store      ReconcileStore
	Topology   TopologySource
	Controller externalChecksAPI
	Metrics    *metrics.Metrics
	// Interval is the reconcile cadence.
	Interval time.Duration
	// LockKey is the advisory-lock key each tick runs under. cmd/console
	// passes scheduler.LockKey; it is a FIELD rather than a constant read
	// from that package because internal/console/scheduler imports this
	// package, so naming scheduler.LockKey here would be an import cycle.
	// See Reconciler's doc comment for why sharing the scheduler's key is
	// the intended wiring and not an accident.
	LockKey int64
}

// Reconciler is the continuous external-check assignment loop: on every tick
// the one replica holding the advisory lock reads every enabled
// kind='continuous' schedule's definition, resolves it against the live
// topology through AssignAgents, and PUTs the resulting per-agent assignment
// to the controller.
//
// RECONCILE, NOT NOTIFY. This loop pushes the COMPLETE desired state on a
// timer; it never computes, tracks or sends a delta, and it is never triggered
// by a definition being saved. That is what makes the whole feature
// self-healing without any durable coordination: the controller holds
// assignments in memory ONLY (Decision 5's documented consequence -- see
// internal/controller.ExternalCheckManager's type comment), so a controller
// restart, a leader failover onto a replica that never saw a PUT, a Console
// restart that lost whatever it thought it had already sent, an assignment
// dropped because an agent's subscriber channel was full, and a cluster that
// scaled up or down all converge on the correct state within ONE interval, by
// the same code path, with nothing to replay and nothing to persist. A
// notify-on-change design would need every one of those cases handled
// separately, and would still be wrong after the one it forgot.
//
// The lock is the scheduler's own LockKey, deliberately. Two consequences,
// both intended:
//
//   - N Console replicas do not PUT N times per interval. Not-holder is a
//     SILENT skip (one not-leader increment, no log) -- the steady state on
//     N-1 of N replicas, every tick, forever.
//   - It also serializes this tick against the SCHEDULER's tick, since they
//     share the key. That is acceptable, and cheaper than a second key: both
//     halves are sub-second in the normal case (a due-schedule poll and a
//     bounded schedule scan plus one topology read and one PUT), and both run
//     on a seconds cadence, so the only effect is that one occasionally waits
//     a tick for the other. A second key would buy overlap that neither needs
//     while adding one more number that must never collide with goose's
//     migration lock or store's prune lock (scheduler.LockKey's own comment
//     enumerates all three).
//
// A tick that fails at ANY stage -- store read, topology read, PUT -- leaves
// the last-pushed state untouched, so the next tick recomputes and retries
// from scratch. Nothing wedges: the ticker keeps ticking, the lock is taken
// and released per tick, and a persistent controller outage costs one counted,
// logged error per interval and nothing else.
type Reconciler struct {
	lock     Locker
	store    ReconcileStore
	topology TopologySource
	ctrl     externalChecksAPI
	m        *metrics.Metrics
	interval time.Duration
	lockKey  int64

	// lastPushed is the JSON fingerprint of the desired state this replica
	// most recently PUT successfully -- change detection's entire state. nil
	// means "nothing has been pushed yet", which is deliberately distinct from
	// the fingerprint of an EMPTY desired state: a freshly started replica must
	// push once even when the answer is "no agent has any check", because the
	// controller it is talking to may be holding assignments from before.
	//
	// It is PER-REPLICA and in-memory on purpose, and its worst case is one
	// redundant PUT: when leadership moves, the new leader has no fingerprint
	// and pushes state the old leader had already pushed. The controller's own
	// change detection (ExternalCheckManager.Apply) absorbs that -- an
	// identical PUT pushes zero agents -- so the cost is one request, never a
	// spurious fan-out to every agent.
	//
	// Owned by the single goroutine inside Run, so it needs no mutex: Tick is
	// never called concurrently with itself, and nothing else touches it.
	lastPushed []byte

	// warnedSkips remembers which definition IDs have already been logged as
	// skipped, so a definition an operator will not fix produces ONE warning
	// per process rather than one per tick forever. The metric is incremented
	// every tick regardless -- that is what makes the condition alertable as a
	// steady rate instead of a single line that scrolls away.
	warnedSkips map[string]struct{}
}

// NewReconciler returns a Reconciler. Run is what starts it.
func NewReconciler(d ReconcilerDeps) *Reconciler { //nolint:gocritic // hugeParam: ReconcilerDeps mirrors scheduler.Deps' value semantics -- a named-field composition root, built once at boot
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

// Run ticks every Interval until ctx is cancelled, and returns promptly when
// it is -- cmd/console waits on it like every other background component.
//
// There is deliberately no immediate first tick, the same choice
// scheduler.Run makes and for the same reason: the cadence is seconds, so a
// replica that just started is at most one tick behind, while an eager first
// pass would have every replica of a fresh rollout race for the lock in the
// same instant. "Converges within one interval" is the guarantee this loop
// offers everywhere else too, and the boot path is not special.
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

// Tick runs exactly one attempt, wrapped in the advisory lock. Exported so the
// tests drive single, deterministic ticks instead of racing a ticker.
//
// A lock this replica did not get is a SILENT skip, and -- because every read
// happens inside the locked function -- a non-holder issues no store query, no
// topology request and no PUT at all. It is one counter increment and nothing
// else.
func (r *Reconciler) Tick(ctx context.Context) {
	locked, err := r.lock.WithAdvisoryLock(ctx, r.lockKey, r.leaderTick)
	switch {
	case err != nil:
		// Both "could not take the lock" and "the reconcile itself failed"
		// land here. Neither stops Run's loop, and neither marks the
		// last-pushed state clean, so the next tick -- one interval away --
		// recomputes and retries.
		r.m.ExternalReconciles.WithLabelValues(reconcileError).Inc()
		slog.Warn("checks: external-check reconcile failed", "error", err)
	case !locked:
		r.m.ExternalReconciles.WithLabelValues(reconcileNotLeader).Inc()
	}
}

// leaderTick is the work one tick does while holding the lock: compute the
// desired state, compare it against what was last successfully pushed, and PUT
// only if it differs.
//
// The unchanged case is the STEADY STATE -- an unchanging cluster with
// unchanging definitions produces it on every tick forever -- so it is counted
// and never logged. The projected-series gauge is still refreshed on it: that
// number describes the assignment currently in force, which is exactly as true
// when nothing changed as when something did.
func (r *Reconciler) leaderTick(ctx context.Context) error {
	desired, series, err := r.desired(ctx)
	if err != nil {
		return err
	}

	// The fingerprint is the desired state's own JSON, which is canonical for
	// this purpose without any extra work: Go's encoder sorts map keys, and
	// AssignAgents already returns each agent's specs sorted by definition ID.
	// Two ticks over an identical cluster therefore produce byte-identical
	// bytes, and any real difference -- an added agent, a disabled definition,
	// a changed target address -- produces different ones.
	fingerprint, err := json.Marshal(desired)
	if err != nil {
		return fmt.Errorf("checks: reconcile: encode desired state: %w", err)
	}

	if r.lastPushed != nil && bytes.Equal(r.lastPushed, fingerprint) {
		r.m.ExternalSeriesProjected.WithLabelValues().Set(float64(series))
		r.m.ExternalReconciles.WithLabelValues(reconcileUnchanged).Inc()
		return nil
	}

	res, err := r.ctrl.PutExternalChecks(ctx, desired)
	if err != nil {
		// lastPushed is deliberately NOT updated: the controller may have
		// applied nothing (a 400 rejects the whole body) or everything (a
		// response lost on the way back), and the only claim this replica can
		// honestly make is that it does not know. Leaving the state dirty makes
		// the next tick re-PUT, which is safe precisely because the PUT is
		// absolute and idempotent.
		return fmt.Errorf("checks: reconcile: put external checks: %w", err)
	}

	r.lastPushed = fingerprint
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

// desired computes the complete per-agent assignment to PUT, keyed by the
// CONTROLLER's agent ID, alongside the number of Prometheus series it projects
// to. The results are named for readability only -- every return below is
// explicit, never naked (AssignAgents' own convention).
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
		// Returned bare, exactly as continuousDefinitions' error is above:
		// AssignAgents already says "checks: assign: ...", and re-wrapping an
		// error this package has ALREADY qualified renders the package name
		// twice ("checks: reconcile: checks: assign: ...") in the log line
		// reconcileOnce writes.
		return nil, 0, err
	}

	out := make(map[string][]controllerclient.ExternalCheckSpec, len(assignments))
	for node, assignment := range assignments {
		agentID, ok := agentIDByNode[node]
		if !ok {
			// Unreachable: every node name AssignAgents can return came from
			// the very snapshot agentIDByNode was built from. Skipping rather
			// than indexing blind keeps a future change to either side from
			// turning into an empty-string agent key in the PUT body.
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

// agentRefs projects a topology snapshot onto the agent list AssignAgents
// takes, plus the node-name -> agent-ID map the PUT body needs.
//
// The two identities are NOT the same string, and conflating them is the one
// mistake this function exists to prevent. The planner keys on NODE NAME --
// that is what httpapi's projection guard and the scheduler's one-per-zone
// resolution both count with, so keying on anything else here would let the
// guard and the reconciler disagree about how many series a definition costs.
// The controller's registry, meanwhile, keys on the AGENT ID the agent
// registered itself under, which is "<node>-<pod>" (internal/agent/agent.go),
// and an "agents" map keyed by node name would land in the PUT response's
// "unknown" list with every assignment silently dropped.
//
// One agent per node is the deployment invariant (a DaemonSet), but the pick
// is made deterministic anyway -- lowest agent ID wins -- so a moment with two
// pods on one node (a rollout) cannot make two consecutive ticks compute
// different bodies for an otherwise unchanged cluster and PUT on every tick.
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

// continuousDefinitions reads every enabled kind='continuous' schedule's
// definition and returns both the planner's view of them (what AssignAgents
// takes) and the per-definition ExternalCheckSpec the PUT body carries, keyed
// by definition ID.
//
// A DISABLED definition is still handed to the planner, minus a spec: it
// produces no assignment and no series, but its source_selection is still
// validated, which is AssignAgents' documented posture toward stored rows and
// the reason a malformed one surfaces as a reconcile error rather than waiting
// for the day an operator flips Enabled.
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
			// A definition deleted between the schedule listing and this read.
			// ON DELETE CASCADE removes its schedules too, so the next tick
			// will not see it at all -- this is a race with a delete, not a
			// dangling row, and failing the whole tick over it would let one
			// operator's delete stop the fleet's reconcile.
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

// continuousDefinitionIDs pages through check_schedules and returns the
// definition IDs of the enabled kind='continuous' rows, deduplicated and in
// listing order (several schedules may name the same definition; the
// definition is assigned once either way).
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
			// Truncated: the desired state below is INCOMPLETE, so some agents
			// will be pushed an assignment missing checks they should be
			// running. Loud on every tick on purpose -- this is a real
			// correctness gap, not a transient, and it needs the store-side
			// query the bound's comment describes.
			slog.Warn("checks: reconcile: schedule scan truncated, desired assignment may be incomplete",
				"scanned", read, "limit", maxReconcileSchedules)
			return ids, nil
		}
	}
}

// specFor builds the constant part of one enabled definition's
// ExternalCheckSpec and reports whether the definition is eligible to be
// pushed at all. An ineligible one is counted, warned about once, and left out
// of the desired state entirely -- never PUT.
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
		// A continuous check against cluster nodes is the agents' own peer
		// mesh, which every agent already runs unprompted; the external-check
		// channel exists for destinations that are NOT registered agents
		// (pb.ExternalTarget's own comment). There is no ExternalTarget to
		// build here, and pushing one with an empty address would make agents
		// probe nothing and export a broken series.
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
		// Name is the DEFINITION's name, never the raw address, for the reason
		// the scheduler's specFor states: name is the only ExternalTarget field
		// that becomes a Prometheus label value, and a definition name is
		// validated and operator-chosen while an address is neither.
		spec.Target = externalTarget(def.Name, adhocTargetKind(def.DestinationAddress), def.DestinationAddress)
	}
	return spec, nil
}

// externalTarget splits an "address" the way pb.ExternalTarget wants it: a
// host and a numeric port, or the address verbatim with port 0 ("the check
// type's default", the proto's own convention).
//
// The split is attempted for kind "host" ONLY. A "url" target's address is a
// URL, whose colon belongs to the scheme, and taking it apart here would both
// mangle the address and invent a port the operator never wrote. Even for a
// host the port must parse as a number in range, so a bare IPv6 literal or a
// name with a colon in it survives untouched rather than being silently
// reinterpreted.
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

// adhocTargetKind classifies an ad-hoc destination address into the host|url
// set store's own targetKinds defines. An address carrying an http(s) scheme
// is a URL; everything else is a host, which is also the safe direction --
// "host" is what every non-HTTP check type expects.
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
