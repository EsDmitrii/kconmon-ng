package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

// runDeadlineSlack is added on top of the computed dispatch-time ceiling
// (see runDeadline) to leave room for the controller round trip, JSON
// encode/decode, and store writes on top of the raw per-pair wait.
const runDeadlineSlack = 30 * time.Second

// terminalOpTimeout bounds the store writes that MUST still complete even after runCtx's own
// deadline has already fired.
const terminalOpTimeout = 10 * time.Second

// relayWaitTimeout bounds execute's pre-close wait for the hub to relay the run's bus-published
// progress frames (waitForRelay); generous relative to the relay path (an in-process channel hop)
// but bounded.
const relayWaitTimeout = 2 * time.Second

// statusCancelled is the terminal status a cancelled or reaped run carries.
const statusCancelled = "cancelled"

// reapSlack is added on top of the longest deadline Start can give a run before ReapStuckRuns will
// touch it; so the margin is deliberately generous rather than tight.
const reapSlack = 5 * time.Minute

// defaultReapLimit bounds one sweep when the caller names no limit.
const defaultReapLimit = 100

// maxRunLifetime is the longest a run can legitimately sit in status "running"; derived from
// runDeadline rather than hardcoded so it cannot drift out of step with the bound Start actually
// applies.
func maxRunLifetime() time.Duration {
	/* The WORST shape, not the average one: maxPairs pairs all from a single source, which is the
	   arrangement the per-source gate serialises hardest and therefore the longest a legitimate run
	   can take. Anything shorter here would have the reaper cancelling runs that are still working. */
	worst := make([]Pair, maxPairs)
	for i := range worst {
		worst[i] = Pair{Source: "one-source"}
	}
	return runDeadline(worst, maxPerPairTimeout, MaxRunDuration) + reapSlack
}

// Store is the persistence seam Runner needs; satisfied by *store.DB when database.mode is enabled.
type Store interface {
	store.RunStore
	store.RunReader
	store.PathSnapshotStore
}

// Run, RunPage and ListFilter are the store package's own Run/RunPage/ RunFilter types.
type (
	Run        = store.Run
	RunPage    = store.RunPage
	ListFilter = store.RunFilter
)

// controllerAPI is the subset of *controllerclient.Client Runner needs; a real
// *controllerclient.Client always satisfies.
type controllerAPI interface {
	Topology(ctx context.Context) (*controllerclient.Topology, error)
	Diagnose(ctx context.Context, req controllerclient.DiagnoseRequest, timeout time.Duration) (json.RawMessage, error)
}

var _ controllerAPI = (*controllerclient.Client)(nil)

// Runner executes on-demand diagnostics runs: Plan expands a Spec into pairs.
type Runner struct {
	ctrl    controllerAPI
	hub     *ws.Hub
	bus     cache.Bus
	store   Store
	metrics *metrics.Metrics

	// activeRuns/activeCount track every run Start has launched until its execute goroutine returns.
	activeRuns  sync.WaitGroup
	activeCount atomic.Int32

	// framesPublished maps a run's topic to its bus-accepted frame count (*atomic.Uint64).
	framesPublished sync.Map

	// runControls maps a run's id to the *runControl Cancel needs; registered by Start before the
	// run's goroutine is scheduled.
	runControls sync.Map
}

// runControl is one in-flight run's cancellation state: the CancelFunc for its own context;
// inferring the status from runCtx.Err alone would collapse the two and silently relabel every
// deadline-expired.
type runControl struct {
	cancel    context.CancelFunc
	cancelled atomic.Bool
}

// NewRunner returns a Runner. ctrl is used both to resolve "every node in the current topology"
// (Plan's nodes argument) and to dispatch each pair.
func NewRunner(ctrl controllerAPI, hub *ws.Hub, bus cache.Bus, st Store, m *metrics.Metrics) *Runner {
	return &Runner{ctrl: ctrl, hub: hub, bus: bus, store: st, metrics: m}
}

// progressFrame is one per-pair frame on run:{id}, states dispatched -> succeeded|failed|timeout.
type progressFrame struct {
	RunID       string `json:"runId"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	State       string `json:"state"`
	Success     bool   `json:"success"`
	DurationNs  int64  `json:"durationNs"`
	Error       string `json:"error,omitempty"`
	Completed   int    `json:"completed"`
	Total       int    `json:"total"`
	// SampleSeq is which probe of this pair the frame reports; always 0 for an instant run, so the
	// frame an client sees is unchanged in meaning.
	SampleSeq int `json:"sampleSeq"`
}

// finishedFrame is the terminal frame on run:{id}.
type finishedFrame struct {
	State  string `json:"state"` // always "finished"
	Status string `json:"status"`
}

// Start validates spec, plans it into pairs, persists a pending run; it returns as soon as the run
// is durable and registered.
func (r *Runner) Start(ctx context.Context, spec Spec, initiator authz.Subject) (string, error) { //nolint:gocritic // hugeParam: Spec mirrors the store package's own value-type write-payload structs (store/checks.go)
	if err := ValidateDuration(spec.Duration); err != nil {
		return "", err
	}
	// Range, not feasibility: a cadence outside [1s, duration] has no plan to be adjusted to, while
	// one this fan-out merely cannot keep is stretched below and reported. See ValidateSampleInterval.
	if err := ValidateSampleInterval(spec.RequestedSampleInterval, spec.Duration); err != nil {
		return "", err
	}
	nodes, err := r.resolveNodes(ctx, spec.Sources, spec.Destinations, spec.TypedDestinations)
	if err != nil {
		return "", err
	}
	pairs, err := Plan(spec, nodes)
	if err != nil {
		return "", err
	}

	if spec.Plane == "" {
		spec.Plane = "pod"
	}

	// The cadence is re-planned, never refused: a check type slower than the cadence it was given
	// stretches the interval instead of making the run impossible. That was true when the base
	// cadence could only be derived from the duration, and it stays true now that an operator can
	// name one -- an unkeepable request is a fact about traceroute, not a mistake in the request.
	// Snapshotted into the spec, with the reason, so the UI reads the plan the server actually made
	// rather than re-deriving one from the duration alone.
	perPairTimeout := clampTimeoutFor(spec.Type, spec.Timeout)
	cadence := PlanCadence(&spec, pairs, perPairTimeout)
	rounds := cadence.SamplesPerPair
	if spec.Duration > 0 {
		// Only an interval run has a cadence to describe, and an instant run's spec snapshot is
		// pinned byte-for-byte by TestSpecSnapshotForNodeOnlyRunIsM3Identical.
		spec.PlannedSampleIntervalNs = cadence.Interval.Nanoseconds()
		spec.PlannedSamplesPerPair = rounds
		// Empty unless something moved, so a run that got exactly what it asked for stores nothing
		// extra and the permalink has nothing to explain away.
		spec.SampleIntervalAdjusted = cadence.Adjusted
	}

	specJSON, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("checks: start: encode spec: %w", err)
	}

	id := uuid.NewString()
	// The budget FIRST, so the row can carry the same instant the context does; see store.CreateRun.
	budget := runDeadline(pairs, perPairTimeout, spec.Duration)
	deadlineAt := time.Now().Add(budget)
	run, err := r.store.CreateRun(ctx, id, spec.Type, spec.Plane, specJSON, string(initiator.Kind), initiator.ID, int32(len(pairs)), deadlineAt) //nolint:gosec // len(pairs) <= maxPairs (400)
	if err != nil {
		return "", fmt.Errorf("checks: start: create run: %w", err)
	}

	runCtx, cancel := context.WithTimeout(context.Background(), budget)

	/* The topic outlives the run's CANCELLATION, and execute's CloseTopicWithFinal is its only
	   closer -- the same contract OpenRunTopic and RunTopicLive already assume.
	   It used to be opened on runCtx, so cancelling a run tore the hub's relay down underneath it:
	   the terminal progress frames execute still publishes were accepted by the bus but no longer
	   relayed, TopicSeq could never reach framesPublished, and every cancelled run spun the full
	   relayWaitTimeout before logging a WARN blaming the bus for dropping frames it had delivered.
	   That is precisely the false alarm the shutdown reorder was meant to remove, arriving by a
	   second route -- and on an operator's explicit cancel, not only at shutdown. */
	topic := ws.RunTopic(run.ID)
	topicOpen := r.hub.OpenTopic(context.WithoutCancel(runCtx), topic)

	// Add happens here, before the goroutine below is scheduled -- not as execute's own first
	// statement.
	r.runControls.Store(run.ID, &runControl{cancel: cancel})
	r.activeRuns.Add(1)
	r.activeCount.Add(1)
	go func() {
		defer r.activeRuns.Done()
		defer r.activeCount.Add(-1)
		// The BASE cadence paces the loop, not the stretched estimate: a round slower than it simply
		// starts the next one immediately, and a fast one is held to the cadence. With an operator's
		// own interval that base IS their request (capped), which is the whole point of the control.
		r.execute(runCtx, cancel, run.ID, pairs, &spec, perPairTimeout, rounds, cadence.Base, topicOpen)
	}()

	return run.ID, nil
}

// CancelTopic is the bus topic a cancel request travels on. It is NOT a WebSocket topic: the hub
// never subscribes to it, only the runners do.
const CancelTopic = "run-cancel"

// cancelRequest is the whole payload: which run, and nothing else.
type cancelRequest struct {
	RunID string `json:"runId"`
}

// Cancel stops the run named by runID; it cancels that run's own context: pairs already dispatched
// still record their outcome (each result write runs on a context.WithoutCancel-derived context of
// its own -- see terminalOpTimeout).
//
// A run belongs to ONE replica — the one whose Start built its context — and the Service load
// balances, so with the chart's default of two console replicas a cancel had about even odds of
// landing on the replica that holds nothing. That call used to read the row, log "leaving it to the
// stuck-run reaper" and return nil, and the handler answered 204: the operator was told the run was
// cancelled while it kept fanning out to the fleet, and the reaper it deferred to only acts after a
// cutoff measured in tens of minutes. So a miss is now FORWARDED on the bus every replica already
// subscribes to, and the replica that owns the run cancels it.
func (r *Runner) Cancel(ctx context.Context, runID string) error {
	if r.cancelLocal(runID) {
		return nil
	}

	// Not in flight here. Only the store can tell "already finished" (nil)
	// apart from "no such run" (ErrNotFound) apart from "someone else's"
	// (forwarded below).
	run, err := r.store.GetRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("checks: cancel: %w", err)
	}
	if run.Status != "pending" && run.Status != "running" {
		return nil // already terminal: nothing to cancel, and nothing to forward
	}
	/* A bus that cannot reach another process is not a forwarding path, and saying so is the
	   difference between an honest 502 and a silent lie.

	   The guard used to be `bus == nil` alone. It is nil only when the console was built without
	   one; the far more common degraded shape is a NON-nil in-process bus, which is what newBus
	   falls back to when redis is unreachable at startup — silently, while the deployment still runs
	   two replicas. Publishing there returns nil unconditionally, so this function reported success,
	   logged "cancel forwarded to the replica that owns the run", and the handler answered 204 to an
	   operator watching a runaway run keep dispatching for its full duration. */
	if r.bus == nil || !r.bus.CrossReplica() {
		slog.Warn("checks: cancel: the run is not in flight in this process and this console has no "+
			"cross-replica bus to forward on — the run continues on the replica that owns it",
			"run", runID, "status", run.Status)
		return ErrCancelUnreachable
	}
	payload, err := json.Marshal(cancelRequest{RunID: runID})
	if err != nil {
		return fmt.Errorf("checks: cancel: encode request: %w", err)
	}
	if err := r.bus.Publish(ctx, CancelTopic, cache.Message{Type: "run.cancel", Data: payload}); err != nil {
		return fmt.Errorf("checks: cancel: forward to the owning replica: %w", err)
	}
	slog.Info("checks: cancel forwarded to the replica that owns the run", "run", runID)
	return nil
}

// cancelLocal cancels the run if THIS process owns it, reporting whether it did.
func (r *Runner) cancelLocal(runID string) bool {
	v, ok := r.runControls.Load(runID)
	if !ok {
		return false
	}
	ctl, _ := v.(*runControl)
	// Set the flag BEFORE cancelling: execute reads it after wg.Wait() returns, and cancelling
	// first would let a single-pair run finish and read the flag before it was written.
	ctl.cancelled.Store(true)
	ctl.cancel()
	return true
}

// WatchCancellations applies cancel requests published by other replicas. Runs until ctx is done;
// a no-op when there is no bus.
func (r *Runner) WatchCancellations(ctx context.Context) error {
	if r.bus == nil {
		<-ctx.Done()
		return nil
	}
	msgs, unsubscribe := r.bus.Subscribe(CancelTopic)
	defer unsubscribe()
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-msgs:
			if !ok {
				return nil
			}
			var req cancelRequest
			if err := json.Unmarshal(msg.Data, &req); err != nil || req.RunID == "" {
				slog.Warn("checks: undecodable cancel request on the bus", "error", err)
				continue
			}
			// Every replica receives it; only the owner has anything to do.
			if r.cancelLocal(req.RunID) {
				slog.Info("checks: cancelled a run on request from another replica", "run", req.RunID)
			}
		}
	}
}

/*
 * CancelAll cancels every run this process is executing, and is the SHUTDOWN path.
 *
 * On SIGTERM the process used to stop the HTTP server and then block in Wait for runs whose
 * contexts it deliberately never cancelled: anything longer than the wait budget could not finish,
 * so the wait was dead time and the process exited mid-run, leaving rows in 'running' with
 * pair_ok/pair_failed at 0 — for the reaper to correct, tens of minutes later, on every rolling
 * update. The cancel funcs were in hand the whole time.
 */
func (r *Runner) CancelAll() int {
	cancelled := 0
	r.runControls.Range(func(key, _ any) bool {
		id, _ := key.(string)
		if r.cancelLocal(id) {
			cancelled++
		}
		return true
	})
	if cancelled > 0 {
		slog.Info("checks: cancelling in-flight runs for shutdown", "runs", cancelled)
	}
	return cancelled
}

/*
 * OpenRunTopic opens a run:{id} topic on THIS replica, if the id names a run that is still going.
 *
 * It is the hub's on-demand opener (Hub.SetEphemeralOpener). The run itself belongs to whichever
 * replica served its POST, and only that replica called OpenTopic — so a browser whose socket landed
 * on the other one asked for a topic that did not exist there and was told "unknown topic", with no
 * progress on the page until the run was over. The frames travel on the bus, so any replica can
 * carry them; what was missing was somebody here subscribing.
 *
 * A terminal run is refused: its frames are finished, the run detail comes from REST, and opening a
 * topic that will never speak again would just leak a subscription.
 */
func (r *Runner) OpenRunTopic(ctx context.Context, topic string) bool {
	runID, ok := ws.RunIDFromTopic(topic)
	if !ok {
		return false
	}
	run, err := r.store.GetRun(ctx, runID)
	if err != nil {
		return false
	}
	if run.Status != "pending" && run.Status != "running" {
		return false
	}
	/* And the SAME reachability guard Cancel applies, for the same reason.
	   The run is live in the store but not in this process, so its frames are published on the
	   replica that owns it. Without a cross-replica bus nothing in THIS process will ever publish to
	   that topic — so accepting the subscribe handed the browser a socket that is connected, silent
	   and permanently empty, while the page's own liveness test ("subscribed and connected") kept the
	   "Live — realtime is up" badge on. Refusing it makes the hub answer "unknown topic", which is
	   what the page's polling fallback and its honest "Delayed data" badge key off. The owning
	   replica still opens the topic in execute(), so nothing is lost where the frames actually are. */
	if r.bus == nil || !r.bus.CrossReplica() {
		return false
	}
	// The topic's lifetime is the RUN's, not this call's. On the OWNING replica execute() closes it;
	// here there is no owner to do that, which is what RunTopicLive is for.
	return r.hub.OpenTopic(context.WithoutCancel(ctx), topic)
}

/*
RunTopicLive reports whether a run:{id} topic still has a run behind it. It is the hub's reaper
predicate (Hub.SetEphemeralLiveness).

A topic opened by OpenRunTopic belongs to a run this process does not own, so nothing here ever
calls CloseTopicWithFinal for it — the owner's close is hub-local. Without this the entry stayed
open forever and neither reclaim path in the hub would touch it; 256 of them (one per run permalink
opened on the wrong replica) and this replica stops serving live run progress altogether.

A topic whose id does not parse is not live: nothing can ever publish to it.

A STORE ERROR is a different answer, and reading it as "the run is over" was wrong in the direction
that costs something. A pool exhaustion, a reset connection or a statement timeout would have torn
down the topic of a run that is still going — the browser watching it stops receiving frames
mid-run, with the run itself unaffected and nothing on the page saying why. A run that has genuinely
ended is reclaimed on the next sweep; a live one that was reaped is not recoverable. So an error
means "leave it alone".
*/
func (r *Runner) RunTopicLive(topic string) bool {
	runID, ok := ws.RunIDFromTopic(topic)
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), runTopicLivenessTimeout)
	defer cancel()
	run, err := r.store.GetRun(ctx, runID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// The run is gone from the table (retention, or an id that never existed): nothing will ever
		// publish here again.
		return false
	case err != nil:
		// Could not tell. Keep it.
		slog.Warn("checks: could not check whether a run topic is still live; keeping the topic",
			"run", runID, "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
		return true
	}
	return run.Status == "pending" || run.Status == "running"
}

// runTopicLivenessTimeout bounds the reaper's store read; it runs on the hub's select loop, which
// must not park.
const runTopicLivenessTimeout = 3 * time.Second

// ReapLoop drives ReapStuckRuns on a ticker until ctx is done.
//
// It is its OWN loop, and not part of the schedule pass, because the two answer to different things.
// Reaping used to live inside scheduler.leaderTick, which is spawned only when
// console.scheduler.enabled is set — and that is off by default. So on the shipped configuration
// nothing ever reaped: a run whose replica died stayed 'running' forever, POST /runs/{id}/cancel
// answered 204 for it while logging "leaving it to the stuck-run reaper", and the reaper did not
// exist. Finishing an orphaned run is not a feature an operator opts into; it is the runner keeping
// its own table honest.
//
// It needs no lock of its own: ReapStuckRuns is a single idempotent statement, and two replicas
// running it at the same moment either finish the same rows once or find nothing left.
func (r *Runner) ReapLoop(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = defaultReapInterval
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := r.ReapStuckRuns(ctx, 0)
			if n > 0 {
				r.metrics.RunsReaped.WithLabelValues().Add(float64(n))
				slog.Warn("checks: force-finished stuck runs", "runs", n)
			}
			if err != nil && ctx.Err() == nil {
				slog.Warn("checks: reap stuck runs failed", "error", err)
			}
		}
	}
}

// defaultReapInterval is how often ReapLoop looks; a stuck run is already past its own deadline by
// the time it qualifies, so minutes rather than seconds is the right cadence.
const defaultReapInterval = time.Minute

// ReapStuckRuns force-finishes runs abandoned mid-flight; the budget is this package's business,
// not the caller's.
//
// It is a BUDGET rather than an instant because a single cutoff has to be the worst run this build
// accepts — 400 pairs from one source over 24 hours, better than thirty hours — and an orphaned
// five-minute run then sat reporting "0 of 1 ok" for a day and a half. Each row is judged against
// its own declared duration and fan-out instead; these three numbers are the same ones runDeadline
// gives a live run, so the reaper and the runner agree about what "too long" means.
func (r *Runner) ReapStuckRuns(ctx context.Context, limit int32) (int64, error) {
	if limit <= 0 {
		limit = defaultReapLimit
	}
	n, err := r.store.ReapStuckRuns(ctx, store.ReapBudget{
		PerSourceConcurrency: maxPerSourceConcurrency,
		PerPairTimeout:       maxPerPairTimeout,
		Slack:                runDeadlineSlack + reapSlack,
	}, limit)
	if err != nil {
		return 0, fmt.Errorf("checks: reap stuck runs: %w", err)
	}
	return n, nil
}

// ActiveForInitiator reports whether this initiator has a run mid-flight anywhere in the fleet --
// the replica-independent form of the scheduler's overrun guard.
func (r *Runner) ActiveForInitiator(ctx context.Context, initiatorKind, initiatorID string) (bool, error) {
	n, err := r.store.ActiveRunsByInitiator(ctx, initiatorKind, initiatorID)
	if err != nil {
		return false, fmt.Errorf("checks: active runs for %s/%s: %w", initiatorKind, initiatorID, err)
	}
	return n > 0, nil
}

// Get returns one run by id.
func (r *Runner) Get(ctx context.Context, runID string) (Run, error) {
	return r.store.GetRun(ctx, runID)
}

// List pages through runs.
func (r *Runner) List(ctx context.Context, f ListFilter) (RunPage, error) {
	return r.store.ListRuns(ctx, f)
}

// GetResults returns one run's per-pair results, in insertion order -- the httpapi seam for GET
// /api/v1/runs/{id}. `truncated` is true when the run holds more rows than one read may carry
// (store.RunResultsCap); the page says so rather than presenting a tail as the whole run.
func (r *Runner) GetResults(ctx context.Context, runID string) (results []store.RunResult, truncated bool, err error) {
	return r.store.GetRunResults(ctx, runID)
}

// Wait blocks until every run Start has launched so far finishes, or until ctx is done, whichever
// comes first.
func (r *Runner) Wait(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		r.activeRuns.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		slog.Warn("checks: runner wait: shutdown budget exceeded with runs still in flight",
			"runsInFlight", r.activeCount.Load())
	}
}

// resolveNodes fetches the current topology's node names when spec needs them for Plan's
// full-mesh/one-sided fallback (either side is empty); getting that wrong would not just cost a
// needless round trip.
func (r *Runner) resolveNodes(ctx context.Context, sources, destinations []string, typed []Destination) ([]string, error) {
	if len(sources) > 0 && (len(destinations) > 0 || len(typed) > 0) {
		return nil, nil
	}
	topo, err := r.ctrl.Topology(ctx)
	if err != nil {
		return nil, fmt.Errorf("checks: resolve topology: %w", err)
	}
	seen := make(map[string]struct{}, len(topo.Agents))
	nodes := make([]string, 0, len(topo.Agents))
	for i := range topo.Agents {
		name := topo.Agents[i].NodeName
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		nodes = append(nodes, name)
	}
	sort.Strings(nodes)
	return nodes, nil
}

// runDeadline computes the run's overall execution ceiling; adding one round's worth rather than
// multiplying is deliberate.
func runDeadline(pairs []Pair, perPairTimeout, duration time.Duration) time.Duration {
	/* roundFloor, not a batch count derived from maxConcurrency alone. The dispatch loop gates every
	   pair TWICE — the run-wide window and the per-source one — and for a run with a single source
	   the per-source gate is the slower of the two by a factor of four. Twelve MTR pairs from one
	   node need six sequential batches, not two, so the context this deadline builds expired
	   mid-flight: dispatch stopped, the reaper later marked the run cancelled, and the pairs that
	   never ran left no result at all. roundFloor is the same arithmetic the planner already uses
	   to decide the cadence, so the deadline and the plan now agree. */
	oneRound := roundFloor(pairs, perPairTimeout) + runDeadlineSlack
	if duration > 0 {
		return duration + oneRound
	}
	return oneRound
}

// execute runs the bounded-concurrency fan-out to completion: MarkRunStarted; runs on runCtx
// (background-derived, own deadline -- never the HTTP request that called Start).
func (r *Runner) execute(runCtx context.Context, cancel context.CancelFunc, runID string, pairs []Pair, spec *Spec, perPairTimeout time.Duration, rounds int, interval time.Duration, topicOpen bool) {
	defer cancel()

	topic := ws.RunTopic(runID)
	started := time.Now()

	// Register the run's relay counter before the first frame can publish;
	// removed after close so the map never outgrows the set of live runs.
	r.framesPublished.Store(topic, &atomic.Uint64{})
	defer r.framesPublished.Delete(topic)

	// The control is removed here, not by Cancel: as long as this run is executing, Cancel must be
	// able to find it.
	defer r.runControls.Delete(runID)

	// MarkRunStarted deliberately does NOT run on runCtx.
	startedCtx, startedCancel := context.WithTimeout(context.WithoutCancel(runCtx), terminalOpTimeout)
	startErr := r.store.MarkRunStarted(startedCtx, runID)
	startedCancel()
	if startErr != nil {
		/* The row never left "pending", so finishing it later would be a write against a state
		   machine that never advanced -- and the run would sit on the page forever, claiming to be
		   about to start. Fail it now, while there is still something to say about why. */
		slog.Error("checks: mark run started failed, abandoning the run", "run", runID, "error", startErr)
		failCtx, failCancel := context.WithTimeout(context.WithoutCancel(runCtx), terminalOpTimeout)
		/* AbandonRun, not FinishRun. FinishRun's UPDATE requires status='running', and the row is
		   still 'pending' precisely because MarkRunStarted is what failed — so it matched nothing,
		   returned ErrWrongState, and the branch below swallowed that as "already terminal". The run
		   stayed 'pending' forever: the detail page kept saying it was about to start, and the
		   stuck-run reaper reaps by deadline against 'running', so it never came back either. */
		if ferr := r.store.AbandonRun(failCtx, runID, "failed"); ferr != nil {
			slog.Error("checks: marking the abandoned run failed too", "run", runID, "error", ferr)
		}
		failCancel()
		/* And the topic goes with it. The run is over before it began; leaving the topic open would
		   hold its ring, its seq counter and its bus subscription for the process's lifetime, and
		   any browser on the permalink would wait for frames that will never come. */
		r.hub.CloseTopicWithFinal(ws.RunTopic(runID), "run.finished",
			json.RawMessage(`{"status":"failed"}`))
		return
	}

	var completed atomic.Int32
	outcomes := newPairOutcomes()

	// An instant run is ONE round.
	if rounds < 1 {
		rounds = 1
	}
	// A duration run runs for the WALL CLOCK the operator asked for. Rounds repeat until it elapses;
	// the planned count is only an estimate and must never end the run early, which it did while the
	// loop counted rounds instead of watching the clock.
	deadline := started.Add(spec.Duration)

	for round := 0; ; round++ {
		// Total counts PROBES, not pairs, so the browser's progress bar means the same thing for
		// both kinds of run. It widens rather than lies: fast rounds outrun the estimate.
		total := len(pairs) * max(rounds, round+1)
		r.dispatchRound(runCtx, topic, runID, pairs, spec, perPairTimeout, round, total, &completed, outcomes)

		switch {
		case runCtx.Err() != nil:
			// Cancelled, or the run's own ceiling fired.
		case spec.Duration <= 0:
			// An instant run is exactly one round.
		case round+1 >= MaxSamplesPerPair:
			// The documented upper bound on samples per pair is the one true cap.
		case !time.Now().Before(deadline):
			// The requested duration has elapsed. A round that overran it still finished, which is
			// the honest end for a run whose rounds are slower than the time it was given.
		default:
			// Sleep to the NEXT ROUND BOUNDARY measured from the run's start, not for a fixed
			// interval after the round finished. A round slower than the cadence finds the boundary
			// already past and starts again immediately, which is what lets fast traces densify.
			// Waiting past the deadline would spend the operator's duration asleep and then run one
			// more round after it had already elapsed, so a boundary beyond it ends the run instead.
			next := started.Add(time.Duration(round+1) * interval)
			if !next.After(deadline) && r.waitForNextRound(runCtx, next) {
				continue
			}
		}
		break
	}

	// An interval run's outcome is judged on the samples it actually took, not on the ones it planned.
	sampleOK, sampleFailed := outcomes.samples()
	expected := len(pairs)
	if spec.Duration > 0 {
		expected = int(sampleOK + sampleFailed)
	}
	status := finalStatus(int(sampleOK), int(sampleFailed), expected)
	// An operator cancel overrides the computed status; runCtx's own deadline
	// firing does not (see runControl's doc comment).
	if v, ok := r.runControls.Load(runID); ok {
		if ctl, isCtl := v.(*runControl); isCtl && ctl.cancelled.Load() {
			status = statusCancelled
		}
	}

	// FinishRun MUST NOT be handed runCtx directly: by the time every pair has been dispatched.
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(runCtx), terminalOpTimeout)
	pairOK, pairFailed := outcomes.pairs()
	if err := r.store.FinishRun(finishCtx, runID, status, pairOK, pairFailed); err != nil && !errors.Is(err, store.ErrWrongState) {
		// ErrWrongState here means a retried/duplicate FinishRun call -- per
		// store.RunStore.FinishRun's doc comment the row already carries a
		// terminal outcome, so it is not a failure worth logging loudly.
		slog.Error("checks: finish run failed", "run", runID, "error", err)
	}
	finishCancel()

	r.metrics.RunsTotal.WithLabelValues(spec.Type, status).Inc()
	r.metrics.RunDuration.WithLabelValues(spec.Type).Observe(time.Since(started).Seconds())

	// The run's terminal frame and the topic's TypeClosed control frame go out together.
	if topicOpen {
		finalData, err := json.Marshal(finishedFrame{State: "finished", Status: status})
		if err != nil {
			slog.Error("checks: encode finished frame failed", "run", runID, "error", err)
			finalData = json.RawMessage(`{}`)
		}
		// Without this wait a still-in-flight progress frame can be assigned a seq ABOVE TypeClosed's.
		if v, ok := r.framesPublished.Load(topic); ok {
			r.waitForRelay(topic, v.(*atomic.Uint64).Load())
		}
		r.hub.CloseTopicWithFinal(topic, ws.TypeEvent, finalData)
	}
}

// waitForNextRound sleeps until the next round boundary, reporting false when the run ended while
// waiting. A boundary already in the past does not sleep at all.
func (r *Runner) waitForNextRound(runCtx context.Context, next time.Time) bool {
	wait := time.Until(next)
	if wait <= 0 {
		return runCtx.Err() == nil
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-runCtx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// pairOutcomes tallies a run's probes two ways at once. sampleOK/sampleFailed count every PROBE --
// what an interval run's terminal status is judged on. latest keeps only each pair's MOST RECENT
// sample, and that is the single rule behind the pairOk/pairFailed a run summary carries: a pair is
// OK when its LATEST sample succeeded. It is the same rule the run detail page applies when it
// collapses samples into one row per pair (web mergeRunPairs, last row wins), so a run's history
// row and its detail page can never disagree about the same run.
type pairOutcomes struct {
	sampleOK     atomic.Int32
	sampleFailed atomic.Int32

	mu     sync.Mutex
	latest map[string]bool
}

func newPairOutcomes() *pairOutcomes {
	return &pairOutcomes{latest: make(map[string]bool)}
}

// record files one finished sample. Rounds are dispatched strictly one after the other and a pair is
// probed at most once per round, so the last record wins for the right reason.
func (o *pairOutcomes) record(source, destination string, success bool) {
	if success {
		o.sampleOK.Add(1)
	} else {
		o.sampleFailed.Add(1)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	// NUL keeps the composite key unambiguous, same convention as the web pairKey.
	o.latest[source+"\x00"+destination] = success
}

func (o *pairOutcomes) samples() (ok, failed int32) {
	return o.sampleOK.Load(), o.sampleFailed.Load()
}

// pairs counts only pairs that produced at least one sample; a cancelled run's untouched pairs stay
// out of both numbers rather than being invented as failures.
func (o *pairOutcomes) pairs() (ok, failed int32) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, success := range o.latest {
		if success {
			ok++
		} else {
			failed++
		}
	}
	return ok, failed
}

// dispatchRound runs ONE bounded-concurrency pass over every pair and returns when the last of them
// has finished.
func (r *Runner) dispatchRound(
	runCtx context.Context, topic, runID string, pairs []Pair, spec *Spec, perPairTimeout time.Duration,
	round, total int, completed *atomic.Int32, outcomes *pairOutcomes,
) {
	sem := make(chan struct{}, maxConcurrency)
	sources := newSourceGate()
	var wg sync.WaitGroup

	// Interleaved so the bounded window spreads across agents; Plan emits pairs source-major, which
	// would otherwise aim the whole window at one agent at a time.
	order := interleaveBySource(pairs)

dispatch:
	// Iterated by index, and each goroutine handed &pairs[i].
	for _, i := range order {
		// A cancelled (or expired) run must not dispatch anything further.
		select {
		case <-runCtx.Done():
			break dispatch
		case sem <- struct{}{}:
		}
		// The capacity that actually bounds a run lives on the agent, not here: it runs at most
		// maxConcurrentTasks on-demand checks and refuses the rest outright.
		if !sources.acquire(runCtx, pairs[i].Source) {
			<-sem
			break dispatch
		}
		wg.Add(1)
		go func(pair *Pair) {
			defer wg.Done()
			defer func() { sources.release(pair.Source); <-sem }()
			r.runOneRecovered(runCtx, topic, runID, pair, spec, perPairTimeout, round, total, completed, outcomes)
		}(&pairs[i])
	}
	wg.Wait()
}

// sourceGate bounds how many pairs are in flight against ONE source agent at a time.
type sourceGate struct {
	mu    sync.Mutex
	slots map[string]chan struct{}
}

func newSourceGate() *sourceGate {
	return &sourceGate{slots: make(map[string]chan struct{})}
}

// acquire blocks until this source has a free slot; false means the run ended while waiting.
func (g *sourceGate) acquire(ctx context.Context, source string) bool {
	g.mu.Lock()
	slot, ok := g.slots[source]
	if !ok {
		slot = make(chan struct{}, maxPerSourceConcurrency)
		g.slots[source] = slot
	}
	g.mu.Unlock()

	select {
	case slot <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (g *sourceGate) release(source string) {
	g.mu.Lock()
	slot := g.slots[source]
	g.mu.Unlock()
	if slot != nil {
		<-slot
	}
}

// interleaveBySource returns pair indices ordered round-robin across sources, so consecutive
// dispatches land on different agents.
func interleaveBySource(pairs []Pair) []int {
	bySource := make(map[string][]int)
	order := make([]string, 0, len(pairs))
	for i := range pairs {
		src := pairs[i].Source
		if _, seen := bySource[src]; !seen {
			order = append(order, src)
		}
		bySource[src] = append(bySource[src], i)
	}

	out := make([]int, 0, len(pairs))
	for round := 0; len(out) < len(pairs); round++ {
		for _, src := range order {
			if idx := bySource[src]; round < len(idx) {
				out = append(out, idx[round])
			}
		}
	}
	return out
}

// runOneRecovered wraps runOne with a panic recovery.
func (r *Runner) runOneRecovered(
	ctx context.Context, topic, runID string, pair *Pair, spec *Spec, perPairTimeout time.Duration, round, total int,
	completed *atomic.Int32, outcomes *pairOutcomes,
) {
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}
		slog.Error("checks: pair panicked, recording as failed", "run", runID,
			"source", pair.Source, "destination", pair.Destination.Label(), "panic", rec)
		outcomes.record(pair.Source, pair.Destination.Label(), false)
		done := completed.Add(1)
		r.publishFrame(ctx, topic, progressFrame{
			RunID: runID, Source: pair.Source, Destination: pair.Destination.Label(),
			State: "failed", Success: false, Error: fmt.Sprintf("checks: pair panicked: %v", rec),
			Completed: int(done), Total: total, SampleSeq: round,
		})
	}()
	r.runOne(ctx, topic, runID, pair, spec, perPairTimeout, round, total, completed, outcomes)
}

// runOne dispatches one pair, persists its result, and publishes its
// dispatched and terminal progress frames.
func (r *Runner) runOne(
	ctx context.Context, topic, runID string, pair *Pair, spec *Spec, perPairTimeout time.Duration, round, total int,
	completed *atomic.Int32, outcomes *pairOutcomes,
) {
	r.publishFrame(ctx, topic, progressFrame{
		RunID: runID, Source: pair.Source, Destination: pair.Destination.Label(),
		State: "dispatched", Completed: int(completed.Load()), Total: total, SampleSeq: round,
	})

	outcome := r.dispatchPair(ctx, pair, spec, perPairTimeout)

	state, result := "succeeded", "ok"
	if !outcome.success {
		state, result = "failed", "failed"
		if outcome.timedOut {
			state, result = "timeout", "timeout"
		}
	}
	outcomes.record(pair.Source, pair.Destination.Label(), outcome.success)
	r.metrics.RunPairs.WithLabelValues(result).Inc()

	// A result that already arrived must still be recorded even if ctx
	// (runCtx) has, by now, hit its own deadline -- see terminalOpTimeout's
	// doc comment. resultCtx deliberately outlives ctx's own cancellation.
	resultCtx, resultCancel := context.WithTimeout(context.WithoutCancel(ctx), terminalOpTimeout)
	// check_results.result is NOT NULL: a pair the agent refused (an errorResult carries no
	// CheckResult at all) or a transport failure yields an empty resultJSON.
	resultJSON := outcome.resultJSON
	if len(resultJSON) == 0 {
		resultJSON = json.RawMessage(`{}`)
	}
	row, err := r.store.UpsertRunResult(resultCtx, store.RunResultInput{
		RunID: runID, SourceNode: pair.Source, DestinationNode: pair.Destination.Label(),
		Success: outcome.success, DurationNs: outcome.durationNs, Error: outcome.errStr, Result: resultJSON,
		SampleSeq: int32(round), //nolint:gosec // round < MaxSamplesPerPair (500)
	})
	if err != nil {
		slog.Error("checks: upsert run result failed", "run", runID, "error", err)
	} else {
		/* Path history is projected only once the result row it describes is durable — and it is
		   stamped with THAT ROW'S OWN recorded_at rather than with the clock at this line.
		   time.Now() here is a few hundred microseconds later than the row's now(), which sounds
		   harmless and is not: a route's first_seen then lands AFTER the very trace that created
		   it, so that trace falls outside [first_seen, last_seen] — the window everything reading
		   back from a route uses. It cost exactly one trace per route in the trace list, and it is
		   why the tick that CREATED a route was the one tick told "no recorded route covers this
		   probe" on the run permalink. */
		r.projectMTRSnapshot(ctx, runID, pair, spec, outcome.resultJSON, row.RecordedAt)
	}
	resultCancel()

	done := completed.Add(1)
	r.publishFrame(ctx, topic, progressFrame{
		RunID: runID, Source: pair.Source, Destination: pair.Destination.Label(),
		State: state, Success: outcome.success, DurationNs: outcome.durationNs, Error: outcome.errStr,
		Completed: int(done), Total: total, SampleSeq: round,
	})
}

// projectMTRSnapshot records one finished mtr pair's trace in path history; it is a PROJECTION and
// never an authority.
func (r *Runner) projectMTRSnapshot(ctx context.Context, runID string, pair *Pair, spec *Spec, resultJSON json.RawMessage, recordedAt time.Time) {
	in, ok := ProjectMTRSnapshot(spec, pair, resultJSON, recordedAt.UTC(), runID)
	if !ok {
		return
	}

	snapCtx, snapCancel := context.WithTimeout(context.WithoutCancel(ctx), terminalOpTimeout)
	defer snapCancel()

	_, isNew, err := r.store.UpsertPathSnapshot(snapCtx, in)
	if err != nil {
		slog.Error("checks: upsert path snapshot failed", "run", runID,
			"source", pair.Source, "destination", pair.Destination.Label(), "error", err)
		r.metrics.MTRSnapshots.WithLabelValues("error").Inc()
		return
	}
	result := "repeat"
	if isNew {
		result = "new-path"
	}
	r.metrics.MTRSnapshots.WithLabelValues(result).Inc()
}

// pairOutcome is dispatchPair's result: enough to both persist a
// store.RunResultInput and build a progress frame.
type pairOutcome struct {
	success    bool
	durationNs int64
	errStr     string
	timedOut   bool
	resultJSON json.RawMessage
}

// dispatchPair calls controllerclient.Diagnose for one pair; failures are never fatal here --
// errors.Is against controllerclient's sentinels only distinguishes "timeout" (controller 504) from
// every other failure mode (ErrNoAgent, ErrDispatch, ErrBadRequest, ErrUnavailable, a network
// error) for the progress frame's state.
func (r *Runner) dispatchPair(ctx context.Context, pair *Pair, spec *Spec, perPairTimeout time.Duration) pairOutcome {
	req := controllerclient.DiagnoseRequest{
		Source: pair.Source, Destination: pair.Destination.Label(), Type: spec.Type, Plane: spec.Plane,
	}
	// A node destination leaves both external fields at their zero value, and they are `omitempty`.
	if !pair.Destination.IsNode() {
		req.DestinationKind = controllerclient.DestinationKindExternal
		req.DestinationAddress = pair.Destination.Address
	}

	start := time.Now()
	raw, err := r.ctrl.Diagnose(ctx, req, perPairTimeout)
	elapsed := time.Since(start)

	if err != nil {
		return pairOutcome{
			success:    false,
			durationNs: elapsed.Nanoseconds(),
			errStr:     err.Error(),
			timedOut:   errors.Is(err, controllerclient.ErrCheckTimeout),
		}
	}

	var result model.CheckResult
	if decErr := json.Unmarshal(raw, &result); decErr != nil {
		return pairOutcome{
			success:    false,
			durationNs: elapsed.Nanoseconds(),
			errStr:     fmt.Sprintf("checks: decode result: %v", decErr),
		}
	}

	return pairOutcome{
		success:    result.Success,
		durationNs: result.Duration.Nanoseconds(),
		errStr:     result.Error,
		resultJSON: raw,
	}
}

// publishFrame marshals frame and publishes it on topic via the bus; it never touches the hub
// directly -- ws.Hub.OpenTopic already subscribed the hub to this bus topic.
func (r *Runner) publishFrame(ctx context.Context, topic string, frame any) {
	data, err := json.Marshal(frame)
	if err != nil {
		slog.Error("checks: encode progress frame failed", "topic", topic, "error", err)
		return
	}
	if err := r.bus.Publish(ctx, topic, cache.Message{Type: ws.TypeEvent, Data: data}); err != nil {
		slog.Warn("checks: publish progress frame failed", "topic", topic, "error", err)
		return
	}
	// Count only frames the bus accepted: execute's pre-close relay wait
	// compares this against hub.TopicSeq, which advances once per relayed
	// frame — counting a failed publish would make that target unreachable.
	if v, ok := r.framesPublished.Load(topic); ok {
		v.(*atomic.Uint64).Add(1)
	}
}

// waitForRelay blocks until the hub has relayed (assigned a seq to) at least target frames on
// topic; bounded, never exact-by-force: the in-process bus DROPS frames when a subscriber's buffer
// overflows (documented lossiness).
func (r *Runner) waitForRelay(topic string, target uint64) {
	if target == 0 {
		return
	}
	deadline := time.Now().Add(relayWaitTimeout)
	for time.Now().Before(deadline) {
		if r.hub.TopicSeq(topic) >= target {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	slog.Warn("checks: relay wait timed out — some progress frames were dropped by the bus",
		"topic", topic, "published", target, "relayed", r.hub.TopicSeq(topic))
}

// finalStatus computes a run's terminal status from its pair counts: not a boolean.
func finalStatus(pairOK, pairFailed, total int) string {
	switch {
	case total == 0, pairOK == total:
		return "succeeded"
	case pairFailed == total:
		return "failed"
	default:
		return "partial"
	}
}
