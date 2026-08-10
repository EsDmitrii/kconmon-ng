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
	return runDeadline(maxPairs, maxPerPairTimeout, MaxRunDuration) + reapSlack
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
	runCtx, cancel := context.WithTimeout(context.Background(), runDeadline(len(pairs), perPairTimeout, spec.Duration))

	// ctx here MUST be runCtx, not the caller's ctx.
	topic := ws.RunTopic(run.ID)
	topicOpen := r.hub.OpenTopic(runCtx, topic)

	// Add happens here, before the goroutine below is scheduled -- not as execute's own first
	// statement.
	r.runControls.Store(run.ID, &runControl{cancel: cancel})
	r.activeRuns.Add(1)
	r.activeCount.Add(1)
	go func() {
		defer r.activeRuns.Done()
		defer r.activeCount.Add(-1)
		r.execute(runCtx, cancel, run.ID, pairs, &spec, perPairTimeout, plannedRounds(spec.Duration), SampleInterval(spec.Duration), topicOpen)
	}()

	return run.ID, nil
}

// Cancel stops the run named by runID; it cancels that run's own context: pairs already dispatched
// still record their outcome (each result write runs on a context.WithoutCancel-derived context of
// its own -- see terminalOpTimeout).
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

// ReapStuckRuns force-finishes runs left in status "running" long past any deadline Start could
// have given them; the cutoff is this package's business, not the caller's.
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

// GetResults returns one run's per-pair results, in insertion order -- the httpapi seam for GET
// /api/v1/runs/{id}.
func (r *Runner) GetResults(ctx context.Context, runID string) ([]store.RunResult, error) {
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
func runDeadline(pairCount int, perPairTimeout, duration time.Duration) time.Duration {
	batches := (pairCount + maxConcurrency - 1) / maxConcurrency
	if batches < 1 {
		batches = 1
	}
	oneRound := time.Duration(batches)*perPairTimeout + runDeadlineSlack
	if duration > 0 {
		return duration + oneRound
	}
	return oneRound
}

// plannedRounds is how many times an interval run intends to probe each pair: the duration divided
// by the sampling cadence.
func plannedRounds(d time.Duration) int {
	if d <= 0 {
		return 1
	}
	n := int(d / SampleInterval(d))
	if n < 1 {
		n = 1
	}
	if n > MaxSamplesPerPair {
		n = MaxSamplesPerPair
	}
	return n
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
	if err := r.store.MarkRunStarted(startedCtx, runID); err != nil {
		slog.Error("checks: mark run started failed", "run", runID, "error", err)
	}
	startedCancel()

	var completed atomic.Int32
	outcomes := newPairOutcomes()

	// An instant run is ONE round.
	if rounds < 1 {
		rounds = 1
	}
	// Total counts PROBES, not pairs, so the browser's progress bar means the same thing for both
	// kinds of run.
	total := len(pairs) * rounds

	for round := 0; round < rounds; round++ {
		r.dispatchRound(runCtx, topic, runID, pairs, spec, perPairTimeout, round, total, &completed, outcomes)
		if round+1 >= rounds || runCtx.Err() != nil {
			break
		}
		// Sleep to the NEXT ROUND BOUNDARY measured from the run's start, not for a fixed interval after
		// the round finished.
		next := started.Add(time.Duration(round+1) * interval)
		wait := time.Until(next)
		if wait <= 0 {
			continue
		}
		timer := time.NewTimer(wait)
		select {
		case <-runCtx.Done():
			timer.Stop()
		case <-timer.C:
			continue
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
	var wg sync.WaitGroup

dispatch:
	// Iterated by index, and each goroutine handed &pairs[i].
	for i := range pairs {
		// A cancelled (or expired) run must not dispatch anything further.
		select {
		case <-runCtx.Done():
			break dispatch
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(pair *Pair) {
			defer wg.Done()
			defer func() { <-sem }()
			r.runOneRecovered(runCtx, topic, runID, pair, spec, perPairTimeout, round, total, completed, outcomes)
		}(&pairs[i])
	}
	wg.Wait()
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
	if _, err := r.store.UpsertRunResult(resultCtx, store.RunResultInput{
		RunID: runID, SourceNode: pair.Source, DestinationNode: pair.Destination.Label(),
		Success: outcome.success, DurationNs: outcome.durationNs, Error: outcome.errStr, Result: resultJSON,
		SampleSeq: int32(round), //nolint:gosec // round < MaxSamplesPerPair (500)
	}); err != nil {
		slog.Error("checks: upsert run result failed", "run", runID, "error", err)
	} else {
		// Path history is projected only once the result row it describes is durable.
		r.projectMTRSnapshot(ctx, runID, pair, spec, outcome.resultJSON)
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
