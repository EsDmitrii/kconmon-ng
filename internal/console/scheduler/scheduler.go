// Package scheduler is the Console's cadence loop: on every tick the ONE
// replica that holds a PostgreSQL advisory lock fires the check schedules that
// have come due -- as ordinary diagnostics runs, through the existing
// checks.Runner -- and drives the stuck-run reaper.
//
// It is store.Pruner's twin (M4 Plan Decision 2). The primitive, the
// per-attempt lock, and the "not acquired is a silent skip" contract are
// deliberately identical; only the key, the cadence and the work differ.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// LockKey is the pg_try_advisory_lock key this loop serializes itself on
// across replicas. It is
// crc32.Checksum([]byte("kconmon-ng.console.Scheduler"), crc32.MakeTable(crc32.IEEE)),
// the same derivation goose uses for its own DefaultLockID, computed from a
// different input string.
//
// It is DISTINCT from both other keys this module takes, and that is the whole
// point of stating all three here:
//
//   - 4097083626 -- goose's lock.DefaultLockID, held for the length of a
//     migration run (store/migrate.go);
//   - 3698486424 -- store's pruneLockKey, held for the length of one retention
//     sweep (store/prune.go);
//   - 2111970501 -- this one.
//
// A collision would not corrupt anything, but it would make one of these
// silently starve the other: a long first-boot migration or a catch-up prune
// sweep would look, to every replica, exactly like "another replica is already
// scheduling", and no schedule would fire for as long as it ran.
const LockKey int64 = 2111970501

// dueLimit bounds how many due schedules one tick will look at. A tick holds
// the fleet-wide lock for as long as it runs, so it must not be able to grow
// without bound: a backlog simply spills into the next tick, seconds later,
// with the leader re-elected each time.
const dueLimit = 100

// The schedule kinds this loop knows. 'cron' is a later milestone
// (migration 00004's column comment, Decision 9) and would arrive here as an
// unrecognized kind -- see fireOne.
const (
	kindOnce       = "once"
	kindInterval   = "interval"
	kindContinuous = "continuous"
)

// SchedulerTicks result labels -- the closed set metrics.go documents.
const (
	tickOK        = "ok"
	tickNotLeader = "not-leader"
	tickError     = "error"
)

// SchedulerSkipped reason labels -- the closed set metrics.go documents.
const (
	skipOverrun  = "overrun"
	skipDisabled = "disabled"
)

// initiatorKind is what a scheduled run records in check_runs.initiator_kind,
// alongside the SCHEDULE's id as the initiator id. It is deliberately not one
// of authz's own SubjectKind values: no human and no token started this run,
// and writing "user" or "token" would put a lie in the one column an operator
// reads to answer "who ran this?". The answer here is "schedule <uuid>", and
// that id is the thing that makes a surprising run traceable back to the row
// that caused it. Nothing authorizes against this Subject -- Runner.Start only
// records it.
const initiatorKind = "scheduler"

// Locker is the cross-replica mutual-exclusion seam, satisfied by *store.DB.
// See store.DB.WithAdvisoryLock for the (false, nil) = "someone else has it"
// contract this package depends on.
type Locker interface {
	WithAdvisoryLock(ctx context.Context, key int64, fn func(context.Context) error) (bool, error)
}

// Store is everything the loop reads and writes in PostgreSQL. A narrow local
// interface over *store.DB, for the same reason httpapi declares its own:
// the unit tests drive the whole tick against a fake with no database at all.
type Store interface {
	ListDueSchedules(ctx context.Context, due time.Time, limit int) ([]store.Schedule, error)
	MarkScheduleFired(ctx context.Context, id string, firedAt time.Time, nextFireAt *time.Time, lastError string) error
	GetDefinition(ctx context.Context, id string) (store.Definition, error)
	GetTarget(ctx context.Context, id string) (store.Target, error)
}

// Runner is the diagnostics runner seam. A scheduled run is NOT a second kind
// of run: it goes through the very same Start as POST /api/v1/runs, lands in
// the same check_runs table, publishes on the same run:{id} WebSocket topic,
// and produces the same events. Get is how overrun protection asks whether the
// previous fire is still in flight; ReapStuckRuns is Task 12's sweep, which
// this loop drives because it must run in exactly one replica.
type Runner interface {
	Start(ctx context.Context, spec checks.Spec, initiator authz.Subject) (string, error) //nolint:gocritic // Spec is a value type by design (checks.Runner.Start)
	Get(ctx context.Context, runID string) (checks.Run, error)
	ReapStuckRuns(ctx context.Context, limit int32) (int64, error)
}

// TopologySource is the live node/agent snapshot a definition's
// source_selection is resolved against. Same narrow local interface httpapi's
// projection guard declares, for the same reason: a test pins a fixed topology
// with no controller, no HTTP server and no leader election.
type TopologySource interface {
	Topology(ctx context.Context) (*controllerclient.Topology, error)
}

var (
	_ Locker         = (*store.DB)(nil)
	_ Store          = (*store.DB)(nil)
	_ Runner         = (*checks.Runner)(nil)
	_ TopologySource = (*controllerclient.Client)(nil)
)

// Deps is everything New needs. Every field is REQUIRED except Topology:
// without a topology source the loop can still fire every definition whose
// selection is "all" or "per-zone" (checks.Plan expands an empty source list
// against the current topology on its own), and refuses only the
// "one-per-zone" ones it cannot narrow honestly.
type Deps struct {
	Lock     Locker
	Store    Store
	Runner   Runner
	Topology TopologySource
	Metrics  *metrics.Metrics
	// Interval is the tick cadence (console.scheduler.tickInterval).
	Interval time.Duration
}

// Scheduler is the loop itself. One per process; Run owns it.
type Scheduler struct {
	lock     Locker
	store    Store
	runner   Runner
	topology TopologySource
	m        *metrics.Metrics
	interval time.Duration

	// now is time.Now indirected for the tests that assert next-fire
	// arithmetic, which cannot be done against a real clock without either
	// sleeping or asserting a range.
	now func() time.Time

	// lastRun maps a schedule id to the id of the run this replica most
	// recently started for it -- overrun protection's entire state (see
	// previousRunActive).
	//
	// Owned by the single goroutine inside Run, so it needs no mutex: Tick is
	// never called concurrently with itself, and nothing else touches it.
	//
	// It is PER-REPLICA and in-memory on purpose. check_runs has no
	// schedule_id column to ask the database with (migration 00003 predates
	// schedules, and adding one is a schema change this task does not need),
	// so the leader remembers what it started. The cost of that choice is
	// bounded and self-correcting: when leadership moves -- a rollout, a pod
	// restart -- the new leader starts with an empty map and may fire one
	// extra overlapping run for a schedule whose previous run is still going.
	// One overlap at the moment of a leader change, never a growing backlog,
	// because the new leader records that run and protects every fire after it.
	lastRun map[string]string
}

// New returns a Scheduler. Run is what starts it.
func New(d Deps) *Scheduler { //nolint:gocritic // hugeParam: Deps mirrors httpapi.Deps' value semantics -- a named-field composition root, built once at boot
	return &Scheduler{
		lock:     d.Lock,
		store:    d.Store,
		runner:   d.Runner,
		topology: d.Topology,
		m:        d.Metrics,
		interval: d.Interval,
		now:      func() time.Time { return time.Now().UTC() },
		lastRun:  make(map[string]string),
	}
}

// Run ticks every Interval until ctx is cancelled, and returns promptly when
// it is -- cmd/console waits on it like every other background component.
//
// There is deliberately no immediate first tick (store.Pruner has one, after a
// jitter). The cadence here is SECONDS: a replica that just started is at most
// one tick behind, so an eager first pass would buy nothing and would have
// every replica of a fresh rollout race for the lock in the same instant.
func (s *Scheduler) Run(ctx context.Context) {
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

// Tick runs exactly one attempt, wrapped in the advisory lock. Exported so the
// tests drive single, deterministic ticks instead of racing a ticker.
//
// A lock this replica did not get is a SILENT skip -- no log, one
// SchedulerTicks{not-leader} increment. That is the steady state on N-1 of N
// replicas, every tick, forever: logging it would produce a line per replica
// per tick and drown the log in the normal case.
func (s *Scheduler) Tick(ctx context.Context) {
	locked, err := s.lock.WithAdvisoryLock(ctx, LockKey, s.leaderTick)
	switch {
	case err != nil:
		// Both "could not take the lock" and "the work itself failed" land
		// here. Neither stops Run's loop: the next tick is seconds away and
		// re-elects a leader from scratch.
		s.m.SchedulerTicks.WithLabelValues(tickError).Inc()
		slog.Warn("scheduler: tick failed", "error", err)
	case !locked:
		s.m.SchedulerTicks.WithLabelValues(tickNotLeader).Inc()
	default:
		s.m.SchedulerTicks.WithLabelValues(tickOK).Inc()
	}
}

// leaderTick is the work one tick does while holding LockKey: fire what is
// due, then reap what is stuck. Both halves always run -- a failure in the
// schedule pass must not cost the fleet its only reaper pass -- and their
// errors are joined, exactly as store's runSweeps joins its own.
func (s *Scheduler) leaderTick(ctx context.Context) error {
	return errors.Join(s.fireDue(ctx), s.reap(ctx))
}

// fireDue selects the schedules whose next_fire_at has passed and processes
// each one. The selection is store.ListDueSchedules verbatim -- the query
// whose shape check_schedules_due_idx exists for, and whose EXPLAIN a
// drift-proof test already pins (M4 Task 1) -- so this loop cannot quietly
// grow a predicate that turns the due poll into a sequential scan.
//
// One schedule's failure never skips the next: they are independent rows, and
// a definition someone deleted out from under one schedule must not stop every
// other schedule in the fleet from firing.
func (s *Scheduler) fireDue(ctx context.Context) error {
	now := s.now()

	due, err := s.store.ListDueSchedules(ctx, now, dueLimit)
	if err != nil {
		return fmt.Errorf("scheduler: list due schedules: %w", err)
	}
	if len(due) == 0 {
		return nil
	}

	// One topology read per tick at most, shared by every schedule that
	// actually needs it (only "one-per-zone" definitions do), and skipped
	// entirely when none does.
	topo := newTopologyCache(s.topology)

	var errs []error
	for i := range due {
		if err := s.fireOne(ctx, &due[i], now, topo); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// fireOne processes one due schedule.
//
// kind='continuous' returns immediately, without firing and WITHOUT stamping
// the row. Continuous checks are AGENT-SIDE by definition: they are pushed to
// agents as a standing assignment and run there for as long as they are
// enabled, so there is no discrete "fire" for this loop to perform and no
// cadence for it to advance. Task 17's reconciler is what consumes them.
// (In practice one never reaches here at all -- httpapi seeds a continuous
// schedule with next_fire_at = NULL, which keeps it out of the due index
// entirely -- but "the scheduler silently ignores a whole schedule kind" is
// exactly the line that reads as a bug to the next person, so it is stated
// rather than left to be inferred from an index predicate two packages away.)
//
// Everything else ALWAYS advances the cadence, whether it fired or not. A
// schedule that was skipped, or whose run failed to start, must not stay due:
// leaving next_fire_at in the past would re-select the same row on every tick
// seconds later, turning one broken definition into a hot loop, and -- for the
// overrun case -- would accumulate exactly the backlog the skip exists to
// prevent (a 1-minute schedule over a 5-minute run owes four fires, and none
// of them are wanted).
// The same stamp also carries WHY this attempt produced no run, when it did
// not (QA round 5, finding #5). Before it, the only difference between a
// schedule firing perfectly and one whose definition pointed at a deleted
// target was a log line on whichever replica held the lock: both advanced
// last_fired_at, both kept a fresh next_fire_at, and the console showed both
// as a healthy enabled row. The failure now rides in the SAME UPDATE as the
// cadence advance, so the two can never disagree, and it is CLEARED by the
// next attempt that goes through -- the column describes the last attempt, not
// the last bad one, or one bad minute would leave a row red forever.
//
// A deliberate SKIP (a disabled definition, an overrun) counts as clearing:
// nothing about this attempt failed, and leaving an old error standing beside
// "skipped, its previous run is still in flight" would be the console
// contradicting itself.
func (s *Scheduler) fireOne(ctx context.Context, sched *store.Schedule, now time.Time, topo *topologyCache) error {
	if sched.Kind == kindContinuous {
		return nil
	}

	startErr := s.startFor(ctx, sched, topo)
	lastError := ""
	if startErr != nil {
		lastError = scheduleErrorText(sched.ID, startErr)
	}
	if markErr := s.store.MarkScheduleFired(ctx, sched.ID, now, nextFireAt(sched, now), lastError); markErr != nil {
		return errors.Join(startErr, fmt.Errorf("scheduler: mark schedule %s fired: %w", sched.ID, markErr))
	}
	return startErr
}

// scheduleErrorText renders a startFor error for check_schedules.last_error --
// the string the console prints verbatim on the schedule row.
//
// The "scheduler: schedule <uuid>: " prefix every startFor error carries is
// stripped, and only that one: the row this text is attached to IS that
// schedule, so repeating its id would spend a third of a one-line UI field on
// something the reader is already looking at. Everything after the prefix is
// kept exactly as the error wrote it -- the store's own "target ... not found"
// is the actionable half, and paraphrasing it here would create a second
// vocabulary for the same failure.
func scheduleErrorText(scheduleID string, err error) string {
	return strings.TrimPrefix(err.Error(), "scheduler: schedule "+scheduleID+": ")
}

// startFor decides whether sched should actually produce a run right now, and
// starts it if so. A nil return means "handled" -- fired, or deliberately
// skipped with its reason counted.
func (s *Scheduler) startFor(ctx context.Context, sched *store.Schedule, topo *topologyCache) error {
	def, err := s.store.GetDefinition(ctx, sched.DefinitionID)
	if err != nil {
		return fmt.Errorf("scheduler: schedule %s: get definition %s: %w", sched.ID, sched.DefinitionID, err)
	}

	// An enabled schedule on a disabled definition is a paused check, not a
	// broken one: the operator switched the definition off and the cadence
	// keeps ticking underneath it, ready to resume the moment it is switched
	// back on.
	if !def.Enabled {
		s.m.SchedulerSkipped.WithLabelValues(skipDisabled).Inc()
		return nil
	}

	if s.previousRunActive(ctx, sched.ID) {
		s.m.SchedulerSkipped.WithLabelValues(skipOverrun).Inc()
		slog.Info("scheduler: skipping due schedule, its previous run is still in flight",
			"schedule", sched.ID, "definition", def.ID)
		return nil
	}

	spec, err := s.specFor(ctx, &def, topo)
	if err != nil {
		return fmt.Errorf("scheduler: schedule %s: %w", sched.ID, err)
	}

	runID, err := s.runner.Start(ctx, spec, authz.Subject{Kind: initiatorKind, ID: sched.ID})
	if err != nil {
		return fmt.Errorf("scheduler: schedule %s: start run: %w", sched.ID, err)
	}

	s.lastRun[sched.ID] = runID
	s.m.SchedulerFired.WithLabelValues(sched.Kind).Inc()
	slog.Info("scheduler: started scheduled run",
		"schedule", sched.ID, "definition", def.ID, "kind", sched.Kind, "run", runID)
	return nil
}

// previousRunActive reports whether the run this replica last started for
// scheduleID has not reached a terminal status yet.
//
// Anything other than a live "pending"/"running" row clears the entry: a run
// that finished, one the retention pruner already deleted, and a store error
// all mean "do not hold this schedule back". Failing OPEN here is the
// deliberate direction -- the cost of a wrong "not active" is one overlapping
// run, while the cost of a wrong "still active" is a schedule that never fires
// again for the life of the process.
func (s *Scheduler) previousRunActive(ctx context.Context, scheduleID string) bool {
	runID, ok := s.lastRun[scheduleID]
	if !ok {
		return false
	}
	run, err := s.runner.Get(ctx, runID)
	if err != nil {
		delete(s.lastRun, scheduleID)
		return false
	}
	if run.Status == "pending" || run.Status == "running" {
		return true
	}
	delete(s.lastRun, scheduleID)
	return false
}

// reap drives checks.Runner.ReapStuckRuns, which force-finishes run rows left
// "running" long past any deadline Start could have given them.
//
// It runs HERE, under the same per-tick advisory lock as the schedule pass,
// and not on a timer of its own: a reaper on every replica would race itself,
// with N replicas issuing the same bounded UPDATE against the same rows every
// cycle for no additional coverage. The cutoff stays checks' own business
// (only that package knows the ceiling it hands a run), and limit 0 takes its
// default sweep bound.
func (s *Scheduler) reap(ctx context.Context) error {
	n, err := s.runner.ReapStuckRuns(ctx, 0)
	if n > 0 {
		s.m.RunsReaped.WithLabelValues().Add(float64(n))
		slog.Warn("scheduler: force-finished stuck runs", "runs", n)
	}
	if err != nil {
		return fmt.Errorf("scheduler: reap stuck runs: %w", err)
	}
	return nil
}

// nextFireAt computes the schedule's new next_fire_at after a fire attempt at
// now.
//
//   - once: nil. That RETIRES the schedule from the due index -- the terminal
//     state store's own MarkScheduleFired query comment names for kind='once'
//     -- so it can never be handed out a second time however long the process
//     lives or however many replicas take the lock afterwards. It is
//     deliberately not an enabled=false write: the row keeps the flag the
//     operator set, last_fired_at records that it ran, and a NULL next_fire_at
//     is what the partial index already treats as "not due, ever".
//   - interval: now + interval, NOT previous + interval. The drift choice is
//     conscious: re-anchoring on the observed fire means an interval schedule
//     can slip by up to one tick per fire and will never try to catch up after
//     an outage, while prev+interval would keep perfect long-run cadence at
//     the price of firing a burst of back-dated runs the moment a console that
//     was down for an hour comes back. For diagnostics, "probe every 5
//     minutes from now on" is what an operator means; "owe me twelve probes
//     for the hour you were down" is not.
//   - anything else (a kind a future milestone adds, e.g. 'cron'): nil, so an
//     unrecognized cadence retires itself instead of re-firing every tick
//     forever. Logged, because it means this package is behind the schema.
func nextFireAt(sched *store.Schedule, now time.Time) *time.Time {
	switch sched.Kind {
	case kindOnce:
		return nil
	case kindInterval:
		if sched.IntervalNs <= 0 {
			// store.ScheduleInput.Validate rejects this, so it can only come
			// from a hand-edited row; retiring it beats dividing by it.
			slog.Warn("scheduler: interval schedule has no interval, retiring it from the due index",
				"schedule", sched.ID)
			return nil
		}
		next := now.Add(time.Duration(sched.IntervalNs))
		return &next
	default:
		slog.Warn("scheduler: unrecognized schedule kind, retiring it from the due index",
			"schedule", sched.ID, "kind", sched.Kind)
		return nil
	}
}

// specFor projects a stored definition onto the checks.Spec that
// checks.Runner.Start accepts. This is the one place the CONFIGURATION model
// (a definition: a selection, a destination kind, a target reference) meets
// the RUN model (a spec: source names and typed destinations).
func (s *Scheduler) specFor(ctx context.Context, def *store.Definition, topo *topologyCache) (checks.Spec, error) {
	sources, err := s.sourcesFor(ctx, def.SourceSelection, topo)
	if err != nil {
		return checks.Spec{}, err
	}

	spec := checks.Spec{
		Sources: sources,
		Type:    def.CheckType,
		Plane:   def.Plane,
	}

	switch def.DestinationKind {
	case "node":
		// Left empty on purpose: checks.Plan expands an empty destination side
		// into every node in the current topology, which is exactly what a
		// node-destination definition means (probe the mesh).
	case "target":
		target, terr := s.store.GetTarget(ctx, def.DestinationTargetID)
		if terr != nil {
			return checks.Spec{}, fmt.Errorf("get destination target %s: %w", def.DestinationTargetID, terr)
		}
		spec.TypedDestinations = []checks.Destination{{
			Kind: checks.DestKindTarget, Name: target.Name, Address: target.Address,
		}}
	case "adhoc":
		// Name is the DEFINITION's name, never the raw address: Name is what
		// becomes a Prometheus label value and a stored destination_node
		// (checks.Destination's own doc comment), and a definition name is
		// already validated and operator-chosen, while an address is neither.
		spec.TypedDestinations = []checks.Destination{{
			Kind: checks.DestKindAdhoc, Name: def.Name, Address: def.DestinationAddress,
		}}
	default:
		return checks.Spec{}, fmt.Errorf("unknown destination kind %q on definition %s", def.DestinationKind, def.ID)
	}

	return spec, nil
}

// sourcesFor resolves a definition's source_selection to the node names a run
// should dispatch FROM.
//
// The semantics are Plan Decision 11's, the same ones checks.AssignAgents and
// httpapi's projectedAgents implement for the agent-assignment and projection
// paths: "all" and "per-zone" both mean EVERY agent (per-zone groups the same
// set for labelling, it does not shrink it), and "one-per-zone" is the only
// mode that narrows -- exactly one agent per zone, the first by sorted node
// name, with zoneless agents forming one bucket of their own.
//
// "all"/"per-zone" return nil rather than an explicit node list: checks.Plan
// already expands an empty source side against the live topology, so this
// costs no topology round trip and cannot disagree with what a manual run
// through POST /api/v1/runs would have produced. "one-per-zone" is the only
// case that needs the snapshot -- and if it cannot be read, the run is refused
// rather than widened. That is the opposite direction from httpapi's
// fail-open projection guard, and deliberately so: failing open there lets a
// definition be SAVED, failing open here would DISPATCH probe traffic from
// every node in the cluster when the operator asked for one per zone.
func (s *Scheduler) sourcesFor(ctx context.Context, selection string, topo *topologyCache) ([]string, error) {
	if selection != "one-per-zone" {
		return nil, nil
	}
	snapshot, err := topo.get(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve one-per-zone sources: %w", err)
	}

	byZone := make(map[string]string, len(snapshot.Agents))
	for i := range snapshot.Agents {
		agent := &snapshot.Agents[i]
		if cur, ok := byZone[agent.Zone]; !ok || agent.NodeName < cur {
			byZone[agent.Zone] = agent.NodeName
		}
	}
	if len(byZone) == 0 {
		return nil, errors.New("resolve one-per-zone sources: topology reports no agents")
	}

	sources := make([]string, 0, len(byZone))
	for _, node := range byZone {
		sources = append(sources, node)
	}
	// Sorted so two ticks over an unchanged topology produce byte-identical
	// specs -- the stored spec snapshot is what an operator diffs runs by.
	sort.Strings(sources)
	return sources, nil
}

// topologyCache memoizes one tick's topology read. Built per tick, never
// reused across ticks: a snapshot older than one tick would resolve
// one-per-zone against nodes that may already be gone.
type topologyCache struct {
	src  TopologySource
	snap *controllerclient.Topology
	err  error
	done bool
}

func newTopologyCache(src TopologySource) *topologyCache {
	return &topologyCache{src: src}
}

// get returns the tick's snapshot, fetching it at most once -- including the
// failure, which is cached too so ten due schedules cannot turn one controller
// outage into ten round trips.
func (t *topologyCache) get(ctx context.Context) (*controllerclient.Topology, error) {
	if t.done {
		return t.snap, t.err
	}
	t.done = true
	if t.src == nil {
		t.err = errors.New("no topology source is configured (controller.url is unset)")
		return nil, t.err
	}
	t.snap, t.err = t.src.Topology(ctx)
	if t.err != nil {
		t.err = fmt.Errorf("read topology: %w", t.err)
		return nil, t.err
	}
	return t.snap, nil
}
