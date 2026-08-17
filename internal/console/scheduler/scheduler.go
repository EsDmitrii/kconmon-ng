// Package scheduler is the Console's cadence loop; the primitive, the per-attempt lock, and the
// "not acquired is a silent skip" contract are deliberately identical.
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

// LockKey is the pg_try_advisory_lock key this loop serializes itself on across replicas; a
// collision would not corrupt anything, but it would make one of these silently starve.
const LockKey int64 = 2111970501

// dueLimit bounds how many due schedules one tick will look at; a tick holds the fleet-wide lock
// for as long as it runs.
const dueLimit = 100

/*
onceSkipRetry is how far forward a SKIPPED kind='once' schedule pushes its cadence.

	A once-schedule has no next occurrence -- nextFireAt returns nil for it -- so the skip path used
	to leave its row alone entirely, on the reasoning that writing nil would clear next_fire_at and
	retire the occurrence permanently. Leaving it alone retires the whole LOOP instead: ListDueSchedules
	is `WHERE enabled AND next_fire_at <= now ORDER BY next_fire_at LIMIT 100`, and a row whose
	next_fire_at is stuck in the past sorts ahead of every schedule that is merely due now. A skip
	reason can outlive the tick that saw it -- a disabled definition stays disabled until someone
	re-enables it -- so those rows accumulate at the head of the ordering, and at 100 of them nothing
	else in the fleet ever fires again while the schedules themselves still read as enabled.

	Pushing the cadence forward keeps the occurrence pending (next_fire_at stays non-NULL, so it comes
	back when the block clears) and takes the row out of the due window in between, which is what the
	ordering needs. The interval is minutes, not the tick's seconds: a skip that resolves does so on
	human time -- someone re-enables a definition, or a long run finishes.
*/
const onceSkipRetry = time.Minute

// The schedule kinds this loop knows; 'cron' is a later milestone and would arrive here as an
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

// initiatorKind is what a scheduled run records in check_runs.initiator_kind; it is deliberately
// not one of authz's own SubjectKind values.
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
	// MarkScheduleSkipped advances the cadence WITHOUT stamping last_fired_at, for an occurrence
	// that produced no run.
	MarkScheduleSkipped(ctx context.Context, id string, nextFireAt *time.Time) error
	GetDefinition(ctx context.Context, id string) (store.Definition, error)
	GetTarget(ctx context.Context, id string) (store.Target, error)
}

// Runner is the diagnostics runner seam.
type Runner interface {
	Start(ctx context.Context, spec checks.Spec, initiator authz.Subject) (string, error) //nolint:gocritic // Spec is a value type by design (checks.Runner.Start)
	// ActiveForInitiator is the overrun guard's replica-independent question; see previousRunActive.
	ActiveForInitiator(ctx context.Context, initiatorKind, initiatorID string) (bool, error)
	Get(ctx context.Context, runID string) (checks.Run, error)
	ReapStuckRuns(ctx context.Context, limit int32) (int64, error)
}

// TopologySource is the live node/agent snapshot a definition's source_selection is resolved
// against.
type TopologySource interface {
	Topology(ctx context.Context) (*controllerclient.Topology, error)
}

var (
	_ Locker         = (*store.DB)(nil)
	_ Store          = (*store.DB)(nil)
	_ Runner         = (*checks.Runner)(nil)
	_ TopologySource = (*controllerclient.Client)(nil)
)

// Deps is everything New needs; every field is REQUIRED except Topology: without a topology source
// the loop can still fire every definition whose selection is "all" or "per-zone" (checks.Plan
// expands an empty source list against the current topology on its own), and refuses only the
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

	// lastRun maps a schedule id to the id of the run this replica most recently started for it; owned
	// by the single goroutine inside Run, so it needs no mutex.
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

// Run ticks every Interval until ctx is cancelled, and returns promptly when it is; the cadence
// here is SECONDS: a replica that just started is at most one tick behind.
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

// Tick runs exactly one attempt, wrapped in the advisory lock; exported so the tests drive single,
// deterministic ticks instead of racing a ticker.
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

// leaderTick is the work one tick does while holding LockKey: fire what is due; both halves always
// run -- a failure in the schedule pass must not cost the fleet its only reaper pass.
func (s *Scheduler) leaderTick(ctx context.Context) error {
	return errors.Join(s.fireDue(ctx), s.reap(ctx))
}

// fireDue selects the schedules whose next_fire_at has passed and processes each one; the selection
// is store.ListDueSchedules verbatim.
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

// fireOne processes one due schedule. kind='continuous' returns immediately; (in practice one never
// reaches here at all -- httpapi seeds a continuous schedule with next_fire_at = NULL, which keeps
// it out of the due index entirely -- but "the scheduler silently ignores a whole schedule kind" is
// exactly the line that reads as a bug to the next person, so it is stated rather than left to be
// inferred from an index predicate two packages away.) Everything else ALWAYS advances the cadence,
// whether it fired.
func (s *Scheduler) fireOne(ctx context.Context, sched *store.Schedule, now time.Time, topo *topologyCache) error {
	if sched.Kind == kindContinuous {
		return nil
	}

	skipped, startErr := s.startFor(ctx, sched, topo)
	next := nextFireAt(sched, now)

	/* A skip produced NO RUN, so stamping last_fired_at would report one that never happened. Both
	   skips take this path now: a disabled definition, and an overrun.

	   The overrun skip used to go the fired way, and that was the worst possible moment for it — an
	   overrun means the previous run is still in flight (or its replica died holding the row), so the
	   schedule's own column reported it firing on cadence while it had produced nothing for minutes.
	   During an orphan window that column is the only signal an operator has.

	   A kind='once' schedule is the exception: nextFireAt returns nil for it, and writing that here
	   would clear next_fire_at and retire the occurrence permanently — re-enabling the definition
	   would not bring it back, and nothing in the API or the UI would say it had been dropped. It
	   retries instead, onceSkipRetry from now: the occurrence stays pending, and the row leaves the
	   due window in between rather than parking at the head of it forever. */
	if skipped {
		if sched.Kind == kindOnce {
			retryAt := now.Add(onceSkipRetry)
			next = &retryAt
		}
		if markErr := s.store.MarkScheduleSkipped(ctx, sched.ID, next); markErr != nil {
			return fmt.Errorf("scheduler: mark schedule %s skipped: %w", sched.ID, markErr)
		}
		return nil
	}

	/* A START THAT FAILED did not fire either, and for a kind='once' schedule that distinction is
	   the difference between a retry and a silent loss.

	   startErr means no run was created — the controller was unreachable, the topology could not be
	   resolved, the store refused the row. This path used to hand that case to MarkScheduleFired
	   with nextFireAt=nil (nil is what nextFireAt returns for `once`), which cleared next_fire_at and
	   retired the occurrence for good, while stamping last_fired_at with a fire that never happened.
	   The operator's one-shot check was gone, the row said it had run, and last_error was the only
	   trace — on a schedule nothing would look at again.

	   It retries instead, on the same cadence a skip uses: the occurrence is still owed, and the
	   reason it failed is usually transient. last_fired_at stays where it is, because nothing fired.
	   An INTERVAL schedule already had its next occurrence and is unaffected either way. */
	if startErr != nil && sched.Kind == kindOnce {
		retryAt := now.Add(onceSkipRetry)
		if markErr := s.store.MarkScheduleSkipped(ctx, sched.ID, &retryAt); markErr != nil {
			return errors.Join(startErr, fmt.Errorf("scheduler: mark schedule %s for retry: %w", sched.ID, markErr))
		}
		return startErr
	}

	lastError := ""
	if startErr != nil {
		lastError = scheduleErrorText(sched.ID, startErr)
	}
	if markErr := s.store.MarkScheduleFired(ctx, sched.ID, now, next, lastError); markErr != nil {
		return errors.Join(startErr, fmt.Errorf("scheduler: mark schedule %s fired: %w", sched.ID, markErr))
	}
	return startErr
}

// scheduleErrorText renders a startFor error for check_schedules.last_error; the "scheduler:
// schedule <uuid>: " prefix every startFor error carries is stripped.
func scheduleErrorText(scheduleID string, err error) string {
	return strings.TrimPrefix(err.Error(), "scheduler: schedule "+scheduleID+": ")
}

// startFor decides whether sched should actually produce a run right now, and starts it if so. It
// reports skipped=true for EITHER skip -- a disabled definition or an overrun -- because neither
// produced a run and the caller must not write either down as a fire. A nil error means "handled":
// fired, or deliberately skipped with its reason counted.
func (s *Scheduler) startFor(ctx context.Context, sched *store.Schedule, topo *topologyCache) (skipped bool, err error) {
	def, err := s.store.GetDefinition(ctx, sched.DefinitionID)
	if err != nil {
		return false, fmt.Errorf("scheduler: schedule %s: get definition %s: %w", sched.ID, sched.DefinitionID, err)
	}

	// An enabled schedule on a disabled definition is a paused check, not a broken one.
	if !def.Enabled {
		s.m.SchedulerSkipped.WithLabelValues(skipDisabled).Inc()
		slog.Info("scheduler: skipping due schedule, its definition is disabled",
			"schedule", sched.ID, "definition", def.ID)
		return true, nil
	}

	if s.previousRunActive(ctx, sched.ID) {
		s.m.SchedulerSkipped.WithLabelValues(skipOverrun).Inc()
		slog.Info("scheduler: skipping due schedule, its previous run is still in flight",
			"schedule", sched.ID, "definition", def.ID)
		return true, nil
	}

	spec, err := s.specFor(ctx, &def, topo)
	if err != nil {
		return false, fmt.Errorf("scheduler: schedule %s: %w", sched.ID, err)
	}

	runID, err := s.runner.Start(ctx, spec, authz.Subject{Kind: initiatorKind, ID: sched.ID})
	if err != nil {
		return false, fmt.Errorf("scheduler: schedule %s: start run: %w", sched.ID, err)
	}

	s.lastRun[sched.ID] = runID
	s.m.SchedulerFired.WithLabelValues(sched.Kind).Inc()
	slog.Info("scheduler: started scheduled run",
		"schedule", sched.ID, "definition", def.ID, "kind", sched.Kind, "run", runID)
	return false, nil
}

// previousRunActive reports whether a run for scheduleID is still mid-flight; failing OPEN here is
// the deliberate direction.
//
// It ASKS THE STORE rather than this replica's memory. The advisory lock rotates tick by tick, so
// with the chart's default of two console replicas the replica that fired a run was routinely not
// the one holding the next tick — and the other had no memory of it, so it fired the same schedule
// again. Two overlapping runs of the same check, doubling the fan-out at exactly the moment the
// first one was already slow enough to still be going.
//
// The in-memory map stays as the fast path and as the fallback when the store cannot answer.
func (s *Scheduler) previousRunActive(ctx context.Context, scheduleID string) bool {
	if active, err := s.runner.ActiveForInitiator(ctx, initiatorKind, scheduleID); err == nil {
		if !active {
			delete(s.lastRun, scheduleID)
		}
		return active
	}

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

// reap drives checks.Runner.ReapStuckRuns from the schedule pass.
//
// It is no longer the ONLY caller: cmd/console spawns checks.Runner.ReapLoop unconditionally,
// because reaping an orphaned run must not depend on the schedule loop being enabled (it is off by
// default). This stays because a tick that has just fired runs is the cheapest moment to notice the
// ones that never finished, and the statement is idempotent either way.
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

// nextFireAt computes the schedule's new next_fire_at after a fire attempt at now. - once: nil;
// that RETIRES the schedule from the due index.
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

// specFor projects a stored definition onto the checks.Spec that checks.Runner.Start accepts.
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
		// Name is the DEFINITION's name, never the raw address.
		spec.TypedDestinations = []checks.Destination{{
			Kind: checks.DestKindAdhoc, Name: def.Name, Address: def.DestinationAddress,
		}}
	default:
		return checks.Spec{}, fmt.Errorf("unknown destination kind %q on definition %s", def.DestinationKind, def.ID)
	}

	return spec, nil
}

// sourcesFor resolves a definition's source_selection to the node names a run should dispatch FROM;
// the semantics are the, the same ones checks.AssignAgents and httpapi's projectedAgents implement
// for the agent-assignment and projection paths.
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
