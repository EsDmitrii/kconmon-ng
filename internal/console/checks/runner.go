package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// terminalOpTimeout bounds the store writes that MUST still complete even
// after runCtx's own deadline has already fired: FinishRun (the run's
// terminal status) and each pair's UpsertRunResult (a result that already
// arrived must still be recorded, even for the pair racing the deadline).
// Both run on a context derived from context.WithoutCancel(runCtx) --
// deliberately outliving runCtx's own cancellation and deadline -- with this
// fresh bound of their own, instead of runCtx directly: handed runCtx
// unmodified, pgx returns context.DeadlineExceeded for every write attempted
// after the run's own deadline, and since nothing reaps a run stuck
// "running" (no reaper exists for run rows), that row would never become
// terminal again.
const terminalOpTimeout = 10 * time.Second

// relayWaitTimeout bounds execute's pre-close wait for the hub to relay the
// run's bus-published progress frames (waitForRelay). Generous relative to
// the relay path (an in-process channel hop) but bounded: a slow subscriber
// can make the bus drop frames, and a dropped frame never arrives.
const relayWaitTimeout = 2 * time.Second

// statusCancelled is the terminal status a cancelled or reaped run carries.
// It has been a legal check_runs.status since migration 00003 (M3) -- nothing
// wrote it until now.
const statusCancelled = "cancelled"

// reapSlack is added on top of the longest deadline Start can give a run
// before ReapStuckRuns will touch it. The reaper's whole job is to catch rows
// nothing else will ever finish (a Console killed mid-run leaves its
// FinishRun unwritten), and the cost asymmetry is severe: reaping a live run
// destroys real work an operator is waiting on, while reaping a dead one a
// few minutes late costs nothing at all. So the margin is deliberately
// generous rather than tight.
const reapSlack = 5 * time.Minute

// defaultReapLimit bounds one sweep when the caller names no limit.
const defaultReapLimit = 100

// maxRunLifetime is the longest a run can legitimately sit in status
// "running": the deadline Start computes for the largest, slowest fan-out it
// will accept (maxPairs pairs at maxPerPairTimeout each), plus reapSlack.
// Derived from runDeadline rather than hardcoded so it cannot drift out of
// step with the bound Start actually applies.
func maxRunLifetime() time.Duration {
	return runDeadline(maxPairs, maxPerPairTimeout) + reapSlack
}

// Store is the persistence seam Runner needs: create/mutate a run and its
// per-pair results (store.RunStore), read them back (store.RunReader, used by
// Get/List), and record the MTR path snapshots the result-ingest hook projects
// (store.PathSnapshotStore, M5 Task 2). Satisfied by *store.DB when
// database.mode is enabled, or by *MemoryStore when it is disabled (Plan
// Decision 15) -- Runner takes this interface and never branches on which
// backend it was handed.
//
// The snapshot write joined this interface rather than arriving as a separate
// optional field on Runner because there is exactly ONE store behind it: the
// snapshot is a projection of the check_results row written a few lines
// earlier, in the same process, on the same backend, and a second seam would
// make it possible to wire a Runner whose two writes disagree about where they
// land. Nothing has to be nil-checked at every call site either.
type Store interface {
	store.RunStore
	store.RunReader
	store.PathSnapshotStore
}

// Run, RunPage and ListFilter are the store package's own Run/RunPage/
// RunFilter types, aliased here so callers of this package's public surface
// (Runner.Get, Runner.List) never need to import internal/console/store
// themselves for these shapes.
type (
	Run        = store.Run
	RunPage    = store.RunPage
	ListFilter = store.RunFilter
)

// controllerAPI is the subset of *controllerclient.Client Runner needs:
// Topology (resolveNodes' full-mesh fallback) and Diagnose (the per-pair
// dispatch). A real *controllerclient.Client always satisfies it -- this
// exists so tests can substitute a fake for behaviour a real controller
// cannot be made to produce on demand, such as one pair's dispatch panicking
// (runOneRecovered's test).
type controllerAPI interface {
	Topology(ctx context.Context) (*controllerclient.Topology, error)
	Diagnose(ctx context.Context, req controllerclient.DiagnoseRequest, timeout time.Duration) (json.RawMessage, error)
}

var _ controllerAPI = (*controllerclient.Client)(nil)

// Runner executes on-demand diagnostics runs: Plan expands a Spec into
// pairs, Start persists and kicks off a bounded-concurrency fan-out over
// controllerclient.Diagnose, publishing per-pair progress on the run's own
// WebSocket topic as it goes.
type Runner struct {
	ctrl    controllerAPI
	hub     *ws.Hub
	bus     cache.Bus
	store   Store
	metrics *metrics.Metrics

	// activeRuns/activeCount track every run Start has launched until its
	// execute goroutine returns -- Wait's seam for cmd/console's shutdown
	// drain (task-23-brief.md carry-forward). activeRuns is what Wait
	// actually blocks on; activeCount is kept alongside it purely so Wait
	// can log a meaningful count when its budget expires with runs still
	// outstanding (a sync.WaitGroup exposes no way to read its own counter).
	activeRuns  sync.WaitGroup
	activeCount atomic.Int32

	// framesPublished maps a run's topic to its bus-accepted frame count
	// (*atomic.Uint64), registered by execute for the run's lifetime and
	// removed at close. publishFrame increments it; execute's pre-close
	// waitForRelay compares it against hub.TopicSeq so every progress
	// frame's seq lands below the terminal frames' (the ordering the
	// browser's stop-at-TypeClosed contract depends on).
	framesPublished sync.Map

	// runControls maps a run's id to the *runControl Cancel needs (Decision
	// 15). Registered by Start before the run's goroutine is scheduled --
	// same happens-before reasoning as activeRuns.Add -- and removed by
	// execute on its way out, so the map never outgrows the set of runs
	// actually in flight IN THIS PROCESS. A run this replica did not start
	// (another replica's, or one from before a restart) is deliberately
	// absent: Cancel reports that honestly rather than pretending, and the
	// stuck-run reaper is what eventually finishes it.
	runControls sync.Map
}

// runControl is one in-flight run's cancellation state: the CancelFunc for
// its own context, plus the flag that turns the run's terminal status into
// "cancelled".
//
// The flag is what distinguishes an OPERATOR cancel from runCtx's own deadline
// firing. Both cancel the same context, but they mean different things: a run
// that ran out of time reports the outcome its pairs actually produced
// (succeeded/partial/failed -- M3's behaviour, unchanged), while a cancelled
// one reports "cancelled" no matter how many pairs happened to finish first.
// Inferring the status from runCtx.Err() alone would collapse the two and
// silently relabel every deadline-expired M3 run.
type runControl struct {
	cancel    context.CancelFunc
	cancelled atomic.Bool
}

// NewRunner returns a Runner. ctrl is used both to resolve "every node in
// the current topology" (Plan's nodes argument) and to dispatch each pair --
// in production always a *controllerclient.Client, which satisfies
// controllerAPI structurally (callers never need to name that unexported
// type); hub/bus are the WebSocket progress seam (Task 20); st is the
// persistence seam (Task 21) -- a *store.DB or a *MemoryStore.
func NewRunner(ctrl controllerAPI, hub *ws.Hub, bus cache.Bus, st Store, m *metrics.Metrics) *Runner {
	return &Runner{ctrl: ctrl, hub: hub, bus: bus, store: st, metrics: m}
}

// progressFrame is one per-pair frame on run:{id}, states dispatched ->
// succeeded|failed|timeout (task-22-brief.md's verbatim shape).
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
}

// finishedFrame is the terminal frame on run:{id}, delivered via
// hub.CloseTopicWithFinal in the same synchronous call that broadcasts the
// TypeClosed control frame immediately after it -- see execute's doc comment
// for why bus-publishing this frame and then calling hub.CloseTopic
// separately (Task 20's original shape) is not good enough.
type finishedFrame struct {
	State  string `json:"state"` // always "finished"
	Status string `json:"status"`
}

// Start validates spec, plans it into pairs, persists a pending run, opens
// the run:{id} WebSocket topic, and executes the fan-out asynchronously with
// maxConcurrency (8) dispatches in flight. It returns as soon as the run is
// durable and registered -- a fan-out over up to 400 pairs must not be held
// open on the HTTP request that started it.
//
// The run's own execution context is deliberately NOT derived from ctx: ctx
// is used only for the synchronous work here (resolving topology if needed,
// persisting the pending run, opening the WS topic). The goroutine Start
// launches runs on a context derived from context.Background() with its own
// computed deadline, so a browser closing the tab that called Start cannot
// cancel a run already in flight (cancellation is out of scope for M3 --
// task-22-brief.md).
func (r *Runner) Start(ctx context.Context, spec Spec, initiator authz.Subject) (string, error) { //nolint:gocritic // hugeParam: Spec mirrors the store package's own value-type write-payload structs (store/checks.go)
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

	specJSON, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("checks: start: encode spec: %w", err)
	}

	id := uuid.NewString()
	run, err := r.store.CreateRun(ctx, id, spec.Type, spec.Plane, specJSON, string(initiator.Kind), initiator.ID, int32(len(pairs))) //nolint:gosec // len(pairs) <= maxPairs (400)
	if err != nil {
		return "", fmt.Errorf("checks: start: create run: %w", err)
	}

	perPairTimeout := clampTimeout(spec.Timeout)
	runCtx, cancel := context.WithTimeout(context.Background(), runDeadline(len(pairs), perPairTimeout))

	// ctx here MUST be runCtx, not the caller's ctx: OpenTopic's subscription
	// goroutine lives exactly as long as the context it is given, and a
	// request-scoped ctx would tear it down the instant this HTTP handler
	// returns (ws.Hub.OpenTopic's doc comment).
	topic := ws.RunTopic(run.ID)
	topicOpen := r.hub.OpenTopic(runCtx, topic)

	// Add happens here, before the goroutine below is scheduled -- not as
	// execute's own first statement -- so a Wait call racing Start's return
	// can never observe zero in-flight runs before this one has actually
	// registered itself (the standard sync.WaitGroup pitfall: Add must
	// happen-before the Wait it is meant to be visible to). execute itself
	// stays untouched by this bookkeeping so runner_internal_test.go's
	// white-box tests, which call execute directly without going through
	// Start, are unaffected.
	//
	// The run's cancellation control is registered here, in the same
	// before-the-goroutine window and for the same reason: a Cancel racing
	// Start's return must find the run, not a map that the run's own
	// goroutine has not populated yet.
	r.runControls.Store(run.ID, &runControl{cancel: cancel})
	r.activeRuns.Add(1)
	r.activeCount.Add(1)
	go func() {
		defer r.activeRuns.Done()
		defer r.activeCount.Add(-1)
		r.execute(runCtx, cancel, run.ID, pairs, &spec, perPairTimeout, topicOpen)
	}()

	return run.ID, nil
}

// Cancel stops the run named by runID (Plan Decision 15).
//
// It cancels that run's own context: pairs already dispatched still record
// their outcome (each result write runs on a context.WithoutCancel-derived
// context of its own -- see terminalOpTimeout), pairs not yet dispatched never
// dispatch at all, and the run finishes with status "cancelled" rather than
// the succeeded/partial/failed its partial pair counts would otherwise
// produce.
//
// Two non-errors are deliberate. Cancelling a run that already reached a
// terminal status is a NO-OP returning nil: an operator clicking cancel on a
// run that finished a moment earlier has not done anything wrong, and turning
// that race into an error would make the button lie. Cancelling a run this
// process did not start -- another replica's, or one orphaned by a restart --
// also returns nil, but logs: there is no cross-process cancellation seam
// (none is in scope here), and the stuck-run reaper is what eventually
// finishes such a row. Cancelling an id that names no run at all is
// store.ErrNotFound, so httpapi can answer 404.
func (r *Runner) Cancel(ctx context.Context, runID string) error {
	if v, ok := r.runControls.Load(runID); ok {
		ctl, _ := v.(*runControl)
		// Set the flag BEFORE cancelling: execute reads it after wg.Wait()
		// returns, and cancelling first would let a single-pair run finish
		// and read the flag before it was written.
		ctl.cancelled.Store(true)
		ctl.cancel()
		return nil
	}

	// Not in flight here. Only the store can tell "already finished" (nil)
	// apart from "no such run" (ErrNotFound) apart from "someone else's"
	// (nil, logged).
	run, err := r.store.GetRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("checks: cancel: %w", err)
	}
	if run.Status == "pending" || run.Status == "running" {
		slog.Warn("checks: cancel: run is not in flight in this process; leaving it to the stuck-run reaper",
			"run", runID, "status", run.Status)
	}
	return nil
}

// ReapStuckRuns force-finishes runs left in status "running" long past any
// deadline Start could have given them, recording them as "cancelled" (M4
// follow-up #6). It is the seam Task 13's scheduler calls on a timer; there is
// no timer, and no cross-replica lock, here.
//
// The cutoff is this package's business, not the caller's: only this package
// knows the ceiling Start can hand a run (runDeadline over maxPairs at the
// maximum per-pair timeout), and a caller passing its own would eventually
// pass one that kills live runs. limit bounds one sweep so a backlog cannot
// hold a long transaction open over thousands of rows; 0 takes the default.
func (r *Runner) ReapStuckRuns(ctx context.Context, limit int32) (int64, error) {
	if limit <= 0 {
		limit = defaultReapLimit
	}
	n, err := r.store.ReapStuckRuns(ctx, time.Now().UTC().Add(-maxRunLifetime()), limit)
	if err != nil {
		return 0, fmt.Errorf("checks: reap stuck runs: %w", err)
	}
	return n, nil
}

// Get returns one run by id.
func (r *Runner) Get(ctx context.Context, runID string) (Run, error) {
	return r.store.GetRun(ctx, runID)
}

// List pages through runs.
func (r *Runner) List(ctx context.Context, f ListFilter) (RunPage, error) {
	return r.store.ListRuns(ctx, f)
}

// GetResults returns one run's per-pair results, in insertion order -- the
// httpapi seam for GET /api/v1/runs/{id}, which answers "the run and its
// results" (task-23-brief.md) and therefore needs this alongside Get.
func (r *Runner) GetResults(ctx context.Context, runID string) ([]store.RunResult, error) {
	return r.store.GetRunResults(ctx, runID)
}

// Wait blocks until every run Start has launched so far finishes, or until
// ctx is done, whichever comes first. cmd/console's shutdown sequence calls
// this with a short (10s) budget after stopBackground: a run's own execution
// context is deliberately NOT derived from the process's background context
// (Start's doc comment), so stopping the realtime pipeline does not, by
// itself, wait for or cancel any run in flight -- this is the seam that
// gives a rolling update a bounded chance to let one finish, its terminal
// store write and WS frames land, before the process exits, instead of
// abandoning it silently. A run whose own deadline (up to tens of minutes
// for a large, slow fan-out) outlasts ctx's budget is logged here, not
// force-stopped -- Start's own doc comment already establishes that nothing
// external can cancel a run once launched.
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

// resolveNodes fetches the current topology's node names when spec needs
// them for Plan's full-mesh/one-sided fallback (either side is empty). When
// both sides are already explicit, Plan never consults nodes, so this skips
// the controller round trip entirely -- and a spec whose destination side is
// carried entirely by typed destinations counts as explicit, exactly as Plan
// treats it (planDestinations only falls back when BOTH destination halves
// are empty). Getting that wrong would not just cost a needless round trip:
// it would make a purely external run fail outright on a cluster whose
// controller is unreachable, for a node list the run never needed.
func (r *Runner) resolveNodes(ctx context.Context, sources, destinations []string, typed []Destination) ([]string, error) {
	if len(sources) > 0 && (len(destinations) > 0 || len(typed) > 0) {
		return nil, nil
	}
	topo, err := r.ctrl.Topology(ctx)
	if err != nil {
		return nil, fmt.Errorf("checks: resolve topology: %w", err)
	}
	nodes := make([]string, len(topo.Nodes))
	for i, n := range topo.Nodes {
		nodes[i] = n.Name
	}
	return nodes, nil
}

// runDeadline computes the run's overall execution ceiling: the number of
// sequential batches maxConcurrency dispatches produces, times the per-pair
// timeout, plus slack for everything that is not the raw dispatch wait.
func runDeadline(pairCount int, perPairTimeout time.Duration) time.Duration {
	batches := (pairCount + maxConcurrency - 1) / maxConcurrency
	if batches < 1 {
		batches = 1
	}
	return time.Duration(batches)*perPairTimeout + runDeadlineSlack
}

// execute runs the bounded-concurrency fan-out to completion: MarkRunStarted,
// dispatch every pair (at most maxConcurrency in flight), UpsertRunResult and
// publish a progress frame per pair, then FinishRun and
// hub.CloseTopicWithFinal (the finished summary frame followed synchronously
// by the topic's TypeClosed control frame). Runs on runCtx (background-derived,
// own deadline -- never the HTTP request that called Start), which Runner.Cancel
// is the one external thing that can stop short of that deadline or process
// shutdown (Decision 15).
//
// The finished frame goes through hub.CloseTopicWithFinal, not a bus publish
// followed by a separate hub.CloseTopic call: per-pair progress frames reach
// this topic's subscribed clients asynchronously, through the bus and the
// hub's own per-topic subscription goroutine (runEphemeralTopic), while
// CloseTopic's TypeClosed frame is broadcast SYNCHRONOUSLY, directly against
// the hub. Publishing the finished frame the same (asynchronous) way the
// progress frames go out races it against that synchronous TypeClosed --
// TypeClosed can and does win, land a lower seq, and reach a client first,
// and a client that treats TypeClosed as "nothing more is coming" (the
// contract ws.Hub.CloseTopic's own doc comment describes) then drops the
// run's actual terminal status. hub.CloseTopicWithFinal closes that seam by
// broadcasting both frames synchronously, back-to-back, in one call --
// ws.Hub's own doc comment on CloseTopicWithFinal is the seam-level
// guarantee this relies on.
func (r *Runner) execute(runCtx context.Context, cancel context.CancelFunc, runID string, pairs []Pair, spec *Spec, perPairTimeout time.Duration, topicOpen bool) {
	defer cancel()

	topic := ws.RunTopic(runID)
	started := time.Now()

	// Register the run's relay counter before the first frame can publish;
	// removed after close so the map never outgrows the set of live runs.
	r.framesPublished.Store(topic, &atomic.Uint64{})
	defer r.framesPublished.Delete(topic)

	// The control is removed here, not by Cancel: as long as this run is
	// executing, Cancel must be able to find it. Deleting it on the way out
	// is also what makes a second Cancel fall through to the store and answer
	// the "already terminal" no-op instead of re-cancelling a dead context.
	defer r.runControls.Delete(runID)

	// MarkRunStarted deliberately does NOT run on runCtx: a Cancel landing in
	// the window between Start's return and this call would cancel runCtx
	// before the row ever left "pending", MarkRunStarted would fail, and
	// FinishRun's `AND status = 'running'` guard would then refuse the
	// terminal write -- leaving the row stuck "pending" forever. Same
	// WithoutCancel reasoning as FinishRun/UpsertRunResult below, and the
	// same bound.
	startedCtx, startedCancel := context.WithTimeout(context.WithoutCancel(runCtx), terminalOpTimeout)
	if err := r.store.MarkRunStarted(startedCtx, runID); err != nil {
		slog.Error("checks: mark run started failed", "run", runID, "error", err)
	}
	startedCancel()

	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	var completed atomic.Int32
	var pairOK, pairFailed atomic.Int32

dispatch:
	// Iterated by index, and each goroutine handed &pairs[i], so one Pair
	// (80 bytes since Decision 14's typed destination) is not copied twice per
	// dispatch. pairs is never mutated after Plan returned it.
	for i := range pairs {
		// A cancelled (or expired) run must not dispatch anything further:
		// this is the guarantee that "undispatched pairs never dispatch",
		// and it has to be checked BOTH before taking a slot and while
		// waiting for one -- a run cancelled while every slot is occupied
		// would otherwise sit here until an in-flight pair released one, then
		// dispatch the next pair anyway.
		select {
		case <-runCtx.Done():
			break dispatch
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(pair *Pair) {
			defer wg.Done()
			defer func() { <-sem }()
			r.runOneRecovered(runCtx, topic, runID, pair, spec, perPairTimeout, len(pairs), &completed, &pairOK, &pairFailed)
		}(&pairs[i])
	}
	wg.Wait()

	status := finalStatus(int(pairOK.Load()), int(pairFailed.Load()), len(pairs))
	// An operator cancel overrides the computed status; runCtx's own deadline
	// firing does not (see runControl's doc comment).
	if v, ok := r.runControls.Load(runID); ok {
		if ctl, isCtl := v.(*runControl); isCtl && ctl.cancelled.Load() {
			status = statusCancelled
		}
	}

	// FinishRun MUST NOT be handed runCtx directly: by the time every pair has
	// been dispatched, runCtx may already be past its own deadline (that is
	// exactly the case a slow controller/agent produces), and pgx would
	// return context.DeadlineExceeded for the write that is supposed to
	// record that very outcome -- see terminalOpTimeout's doc comment.
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(runCtx), terminalOpTimeout)
	if err := r.store.FinishRun(finishCtx, runID, status, pairOK.Load(), pairFailed.Load()); err != nil && !errors.Is(err, store.ErrWrongState) {
		// ErrWrongState here means a retried/duplicate FinishRun call -- per
		// store.RunStore.FinishRun's doc comment the row already carries a
		// terminal outcome, so it is not a failure worth logging loudly.
		slog.Error("checks: finish run failed", "run", runID, "error", err)
	}
	finishCancel()

	r.metrics.RunsTotal.WithLabelValues(spec.Type, status).Inc()
	r.metrics.RunDuration.WithLabelValues(spec.Type).Observe(time.Since(started).Seconds())

	// The run's terminal frame and the topic's TypeClosed control frame go
	// out together, synchronously, via hub.CloseTopicWithFinal -- see
	// execute's own doc comment for why a bus-publish-then-CloseTopic shape
	// cannot guarantee that ordering. Broadcast is in-process and needs no
	// context: unlike FinishRun/UpsertRunResult there is no I/O here for
	// runCtx's expiry to break.
	if topicOpen {
		finalData, err := json.Marshal(finishedFrame{State: "finished", Status: status})
		if err != nil {
			slog.Error("checks: encode finished frame failed", "run", runID, "error", err)
			finalData = json.RawMessage(`{}`)
		}
		// The progress frames travelled through the bus and are relayed to
		// the hub ASYNCHRONOUSLY; CloseTopicWithFinal broadcasts
		// synchronously. Without this wait a still-in-flight progress frame
		// can be assigned a seq ABOVE TypeClosed's, and a client honoring
		// the terminal signal would never see that pair's outcome.
		if v, ok := r.framesPublished.Load(topic); ok {
			r.waitForRelay(topic, v.(*atomic.Uint64).Load())
		}
		r.hub.CloseTopicWithFinal(topic, ws.TypeEvent, finalData)
	}
}

// runOneRecovered wraps runOne with a panic recovery: a panic anywhere in one
// pair's dispatch/persist/publish path (a bug, not a controller/network
// failure -- those already surface as an ordinary error) must not take down
// the whole run. Without this, a single panicking goroutine would crash the
// process (Go does not confine a panic to the goroutine it started in when
// nothing recovers it) or, if it somehow didn't crash the process, would
// never reach wg.Done() and leave execute's wg.Wait() -- and the run itself
// -- hung until runCtx's own deadline. The panicking pair is recorded exactly
// like an ordinary dispatch failure (failed progress frame, pairFailed
// incremented, completed advanced) so the run's pair counts stay consistent
// and every other in-flight pair is unaffected.
func (r *Runner) runOneRecovered(
	ctx context.Context, topic, runID string, pair *Pair, spec *Spec, perPairTimeout time.Duration, total int,
	completed, pairOK, pairFailed *atomic.Int32,
) {
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}
		slog.Error("checks: pair panicked, recording as failed", "run", runID,
			"source", pair.Source, "destination", pair.Destination.Label(), "panic", rec)
		pairFailed.Add(1)
		done := completed.Add(1)
		r.publishFrame(ctx, topic, progressFrame{
			RunID: runID, Source: pair.Source, Destination: pair.Destination.Label(),
			State: "failed", Success: false, Error: fmt.Sprintf("checks: pair panicked: %v", rec),
			Completed: int(done), Total: total,
		})
	}()
	r.runOne(ctx, topic, runID, pair, spec, perPairTimeout, total, completed, pairOK, pairFailed)
}

// runOne dispatches one pair, persists its result, and publishes its
// dispatched and terminal progress frames.
func (r *Runner) runOne(
	ctx context.Context, topic, runID string, pair *Pair, spec *Spec, perPairTimeout time.Duration, total int,
	completed, pairOK, pairFailed *atomic.Int32,
) {
	r.publishFrame(ctx, topic, progressFrame{
		RunID: runID, Source: pair.Source, Destination: pair.Destination.Label(),
		State: "dispatched", Completed: int(completed.Load()), Total: total,
	})

	outcome := r.dispatchPair(ctx, pair, spec, perPairTimeout)

	state, result := "succeeded", "ok"
	if !outcome.success {
		pairFailed.Add(1)
		state, result = "failed", "failed"
		if outcome.timedOut {
			state, result = "timeout", "timeout"
		}
	} else {
		pairOK.Add(1)
	}
	r.metrics.RunPairs.WithLabelValues(result).Inc()

	// A result that already arrived must still be recorded even if ctx
	// (runCtx) has, by now, hit its own deadline -- see terminalOpTimeout's
	// doc comment. resultCtx deliberately outlives ctx's own cancellation.
	resultCtx, resultCancel := context.WithTimeout(context.WithoutCancel(ctx), terminalOpTimeout)
	// check_results.result is NOT NULL: a pair the agent refused (an
	// errorResult carries no CheckResult at all) or a transport failure
	// yields an empty resultJSON, and writing it raw would trade the error
	// row -- the one thing the operator needs to see -- for an SQLSTATE
	// 23502. The error string is already its own column; the payload
	// becomes the honest empty object. (M4 final-gate browser smoke: a
	// denied external pair left runs with pairFailed=1 and zero result rows.)
	resultJSON := outcome.resultJSON
	if len(resultJSON) == 0 {
		resultJSON = json.RawMessage(`{}`)
	}
	if _, err := r.store.UpsertRunResult(resultCtx, store.RunResultInput{
		RunID: runID, SourceNode: pair.Source, DestinationNode: pair.Destination.Label(),
		Success: outcome.success, DurationNs: outcome.durationNs, Error: outcome.errStr, Result: resultJSON,
	}); err != nil {
		slog.Error("checks: upsert run result failed", "run", runID, "error", err)
	} else {
		// Path history is projected only once the result row it describes is
		// durable, and from the RAW outcome payload rather than resultJSON: the
		// `{}` placeholder above exists to satisfy a NOT NULL column, and
		// feeding it to the projector would be asking it to find a trace in a
		// value that was invented here.
		r.projectMTRSnapshot(ctx, runID, pair, spec, outcome.resultJSON)
	}
	resultCancel()

	done := completed.Add(1)
	r.publishFrame(ctx, topic, progressFrame{
		RunID: runID, Source: pair.Source, Destination: pair.Destination.Label(),
		State: state, Success: outcome.success, DurationNs: outcome.durationNs, Error: outcome.errStr,
		Completed: int(done), Total: total,
	})
}

// projectMTRSnapshot records one finished mtr pair's trace in path history
// (M5 Decision 1: the capture point is the Console's existing result-ingest
// path, not a new collector).
//
// It is a PROJECTION and never an authority. check_results already holds the
// trace verbatim; this derives the normalized, content-hashed route from it so
// "when did the route change?" is a query instead of an archaeology exercise.
// Every failure here -- a payload the projector cannot read, an input the
// store rejects, a database error -- is logged, counted on MTRSnapshots'
// error label, and otherwise invisible: the pair stays successful and the run
// keeps its status. Losing one trace out of a route's history is not worth
// failing a diagnostics run over, and the operator can still read the result
// row.
//
// A projector answer of false is NOT counted: a non-mtr pair, a dispatch that
// produced no payload, a trace whose every hop timed out. Those are silences,
// not failures, and counting them would make a controller outage look like a
// path-history outage on the very metric meant to alert on route changes.
//
// ctx is the run's own context, which by this point may already be past its
// deadline (see terminalOpTimeout). The write therefore runs on a
// context.WithoutCancel-derived context with its own bound, exactly like the
// UpsertRunResult it follows -- and on a FRESH one rather than sharing that
// write's: a slow result write would otherwise leave the projection whatever
// was left of a budget it never got to use.
func (r *Runner) projectMTRSnapshot(ctx context.Context, runID string, pair *Pair, spec *Spec, resultJSON json.RawMessage) {
	in, ok := ProjectMTRSnapshot(spec, pair, resultJSON, time.Now().UTC(), runID)
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

// dispatchPair calls controllerclient.Diagnose for one pair. Failures are
// never fatal here -- errors.Is against controllerclient's sentinels only
// distinguishes "timeout" (controller 504) from every other failure mode
// (ErrNoAgent, ErrDispatch, ErrBadRequest, ErrUnavailable, a network error)
// for the progress frame's state; all of them otherwise produce the same
// "failed" pairOutcome so the remaining pairs in the run still execute.
func (r *Runner) dispatchPair(ctx context.Context, pair *Pair, spec *Spec, perPairTimeout time.Duration) pairOutcome {
	req := controllerclient.DiagnoseRequest{
		Source: pair.Source, Destination: pair.Destination.Label(), Type: spec.Type, Plane: spec.Plane,
	}
	// A node destination leaves both external fields at their zero value, and
	// they are `omitempty` -- so the body on the wire is byte-identical to the
	// one M3 sent. That is the compatibility guarantee, asserted in
	// dispatch_test.go, not an incidental property of this branch.
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

// publishFrame marshals frame and publishes it on topic via the bus. It
// never touches the hub directly -- ws.Hub.OpenTopic already subscribed the
// hub to this bus topic, exactly like events.Ingester feeds the "live" topic
// -- so this still runs to completion even when OpenTopic itself returned
// false (there is simply no subscriber to deliver to).
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

// waitForRelay blocks until the hub has relayed (assigned a seq to) at least
// target frames on topic, or relayWaitTimeout passes. The runner is the
// topic's single publisher (Decision 14), so hub.TopicSeq(topic) == frames
// relayed so far. Bounded, never exact-by-force: the in-process bus DROPS
// frames when a subscriber's buffer overflows (documented lossiness), and a
// dropped frame never reaches the hub — after the timeout the close proceeds
// and the terminal frames simply follow whatever did arrive.
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

// finalStatus computes a run's terminal status from its pair counts: not a
// boolean, because a 400-pair run with two failures is the interesting case
// collapsing it to "failed" would hide (task-22-brief.md).
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
