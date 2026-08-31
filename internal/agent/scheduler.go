package agent

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

/*
peerProbeConcurrency bounds simultaneous probes inside ONE checker's round, mirroring
externalProbeConcurrency in internal/checker/external.go.

A constant, not a knob, sized from the worst case the bound exists for: a partition where every
probe waits out its full timeout. A round is then ceil(N/C) x timeout — serially (C=1) a 100-node
fleet at the default 1s timeout took 100s per "5s" round, and the fleet stopped measuring exactly
when it mattered. At C=32 that worst case is 4s, inside the default 5s interval with headroom;
external.go's 8 would give 13s and still overrun. The cost is at most 32 short-lived IO-bound
goroutines per checker, which is nothing next to the probes themselves even at the chart's 200m
CPU limit.
*/
const peerProbeConcurrency = 32

/*
maxConcurrentReactiveTraces bounds reactive MTR traces across ALL checkers, the same figure as
maxConcurrentTasks in agent.go — four concurrent traces is the load the on-demand executor already
runs at the 200m CPU limit. The per-pair cooldown never bounded this: a partition fails M DISTINCT
destinations at once and each launched its own trace goroutine, each up to mtrTraceBudget long.
A failure refused a slot is not lost — the pair's cooldown stays unstamped, so its next failed
probe (one interval later) tries again as slots free.
*/
const maxConcurrentReactiveTraces = 4

type SchedulerConfig struct {
	Interval time.Duration
	Jitter   time.Duration
	// NodeLocal indicates that this checker does not probe individual peers.
	// When true, the checker runs once per interval against an empty target,
	// rather than once per peer. Used for DNS and HTTP external checks.
	NodeLocal bool
}

type ResultHandler func(model.CheckResult)

type Scheduler struct {
	mu         sync.RWMutex
	checkers   []checker.Checker
	peers      []checker.Target
	configs    map[model.CheckType]SchedulerConfig
	handler    ResultHandler
	source     checker.Target
	paused     bool
	pauseCh    chan struct{}
	mtrChecker *checker.MTRChecker
	// traceFn runs one reactive trace; nil means the MTR checker's own Check.
	// Seam for tests only: MTRChecker is concrete and its Check opens sockets.
	traceFn func(ctx context.Context, target checker.Target) model.CheckResult
	// mtrSem is the global reactive-trace semaphore; see maxConcurrentReactiveTraces.
	mtrSem chan struct{}
	// selfMetrics carries the agent self-observation series; nil (tests) records nothing.
	selfMetrics *metrics.PrometheusMetrics
	// peersUpdatedAt is when UpdatePeers last ran, feeding agent_peer_list_age_seconds.
	peersUpdatedAt time.Time
}

func NewScheduler(source checker.Target, handler ResultHandler) *Scheduler { //nolint:gocritic // hugeParam: Target is a VALUE by design -- a checker must not be able to mutate the caller's copy, and one 80-byte copy per probe is nothing next to the probe itself
	return &Scheduler{
		configs: make(map[model.CheckType]SchedulerConfig),
		handler: handler,
		source:  source,
		pauseCh: make(chan struct{}),
		mtrSem:  make(chan struct{}, maxConcurrentReactiveTraces),
	}
}

// SetSelfMetrics wires the agent self-observation series; call before Run, like AddChecker.
func (s *Scheduler) SetSelfMetrics(m *metrics.PrometheusMetrics) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.selfMetrics = m
}

func (s *Scheduler) getSelfMetrics() *metrics.PrometheusMetrics {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.selfMetrics
}

// PeerListUpdatedAt reports when the peer list last changed hands; zero means never.
func (s *Scheduler) PeerListUpdatedAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.peersUpdatedAt
}

func (s *Scheduler) Pause() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.paused {
		s.paused = true
		s.pauseCh = make(chan struct{})
		slog.Info("scheduler paused")
	}
}

func (s *Scheduler) Resume() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.paused {
		s.paused = false
		close(s.pauseCh)
		slog.Info("scheduler resumed")
	}
}

func (s *Scheduler) waitIfPaused(ctx context.Context) bool {
	s.mu.RLock()
	paused := s.paused
	ch := s.pauseCh
	s.mu.RUnlock()

	if !paused {
		return true
	}

	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *Scheduler) SetMTRChecker(c *checker.MTRChecker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mtrChecker = c
}

func (s *Scheduler) AddChecker(c checker.Checker, cfg SchedulerConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkers = append(s.checkers, c)
	s.configs[c.Name()] = cfg
}

// SetSourceZone updates the source zone reported on every emitted result.
// Intended to be called once after registration, before Run starts.
func (s *Scheduler) SetSourceZone(zone string) {
	s.mu.Lock()
	s.source.Zone = zone
	s.mu.Unlock()
}

func (s *Scheduler) sourceZone() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.source.Zone
}

func (s *Scheduler) UpdatePeers(peers []checker.Target) {
	s.mu.Lock()
	defer s.mu.Unlock()

	filtered := make([]checker.Target, 0, len(peers))
	for _, p := range peers {
		isSelf := p.AgentID == s.source.AgentID ||
			(s.source.NodeName != "" && p.NodeName == s.source.NodeName) ||
			(s.source.PodIP != "" && p.PodIP == s.source.PodIP)
		if isSelf {
			slog.Debug("skipping self from peer list", "agentID", p.AgentID, "node", p.NodeName, "podIP", p.PodIP)
			continue
		}
		filtered = append(filtered, p)
	}
	s.peers = filtered
	s.peersUpdatedAt = time.Now()
}

// Peers returns a copy of the peer list actually probed, which is the registered set minus this
// agent itself.
func (s *Scheduler) Peers() []checker.Target {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]checker.Target(nil), s.peers...)
}

func (s *Scheduler) Run(ctx context.Context) {
	s.mu.RLock()
	checkersCopy := make([]checker.Checker, len(s.checkers))
	copy(checkersCopy, s.checkers)
	s.mu.RUnlock()

	var wg sync.WaitGroup
	for _, c := range checkersCopy {
		wg.Add(1)
		go func(c checker.Checker) {
			defer wg.Done()
			s.runChecker(ctx, c)
		}(c)
	}
	wg.Wait()
}

func (s *Scheduler) runChecker(ctx context.Context, c checker.Checker) {
	cfg := s.configs[c.Name()]
	jitter := cfg.Jitter
	if jitter == 0 {
		jitter = cfg.Interval / 10
	}

	initialDelay := time.Duration(rand.Int64N(int64(jitter))) //nolint:gosec // G404: non-security jitter
	select {
	case <-time.After(initialDelay):
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	for {
		if !s.waitIfPaused(ctx) {
			return
		}
		s.runCheckerOnce(ctx, c)

		select {
		case <-ticker.C:
			jitterDelay := time.Duration(rand.Int64N(int64(jitter))) //nolint:gosec // G404: non-security jitter
			select {
			case <-time.After(jitterDelay):
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (s *Scheduler) runCheckerOnce(ctx context.Context, c checker.Checker) {
	cfg := s.configs[c.Name()]

	/* Self-observation: the round's wall clock, and whether it blew its own interval. An overrun is
	   the cadence-collapse signal M9-1 exists to prevent — a counter an operator can alert on
	   instead of inferring it from probe-result gaps. A round truncated by shutdown is not a
	   reading: observing it would record the cancellation, not the network. */
	start := time.Now()
	defer func() {
		m := s.getSelfMetrics()
		if m == nil || ctx.Err() != nil {
			return
		}
		elapsed := time.Since(start)
		name := string(c.Name())
		m.AgentProbeCycleDuration.WithLabelValues(name).Observe(elapsed.Seconds())
		if cfg.Interval > 0 && elapsed > cfg.Interval {
			m.AgentProbeCycleOverruns.WithLabelValues(name).Inc()
		}
	}()

	if cfg.NodeLocal {
		result := c.Check(ctx, checker.Target{})
		result.Source = s.source.NodeName
		result.SourceZone = s.sourceZone()

		if s.handler != nil {
			s.handler(result)
		}

		if !result.Success {
			slog.Warn("check failed",
				"type", result.Type,
				"source", result.Source,
				"error", result.Error,
			)
		}
		return
	}

	s.mu.RLock()
	peers := make([]checker.Target, len(s.peers))
	copy(peers, s.peers)
	s.mu.RUnlock()

	/* Bounded fan-out, the shape of externalProbeConcurrency in internal/checker/external.go.
	   Probed one after another, every unreachable peer cost its full probe timeout SERIALLY, so a
	   partition stretched the round to N x timeout and the cadence collapsed exactly when the
	   measurements mattered (see peerProbeConcurrency for the numbers). The handler was already
	   called concurrently across checkers, so nothing new is demanded of it; results within a round
	   simply arrive in completion order now instead of peer-list order. */
	sem := make(chan struct{}, peerProbeConcurrency)
	var wg sync.WaitGroup
	for i := range peers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				// Shutting down: an unprobed peer emits no result, same as the serial loop's exit.
				return
			}
			defer func() { <-sem }()

			peer := peers[i]
			result := c.Check(ctx, peer)
			result.Source = s.source.NodeName
			result.SourceZone = s.sourceZone()
			result.Destination = peer.NodeName
			result.DestZone = peer.Zone

			if s.handler != nil {
				s.handler(result)
			}

			if !result.Success {
				slog.Warn("check failed",
					"type", result.Type,
					"source", result.Source,
					"destination", result.Destination,
					"error", result.Error,
				)
				s.triggerMTR(ctx, peer, &result)
			}
		}(i)
	}
	wg.Wait()
}

// mtrTraceBudget is the whole trace's ceiling, on top of the tracer's own per-hop timeout: 30 hops
// that each wait out a second is half a minute of tracing, and a reverse lookup per answering hop on
// top of that. A trace that has not finished inside this is not going to say anything useful.
const mtrTraceBudget = 90 * time.Second

func (s *Scheduler) triggerMTR(ctx context.Context, peer checker.Target, failedResult *model.CheckResult) { //nolint:gocritic // hugeParam: Target is a VALUE by design -- a checker must not be able to mutate the caller's copy, and one 80-byte copy per probe is nothing next to the probe itself
	s.mu.RLock()
	mtr := s.mtrChecker
	trace := s.traceFn
	m := s.selfMetrics
	s.mu.RUnlock()

	if mtr == nil {
		return
	}
	if trace == nil {
		trace = mtr.Check
	}

	// Check types that must never trigger a trace.
	if failedResult.Type == model.CheckDNS || failedResult.Type == model.CheckHTTP ||
		failedResult.Type == model.CheckMTR || failedResult.Type == model.CheckExternal {
		return
	}

	/* The GLOBAL bound goes before the pair cooldown, deliberately: a failure refused a slot must
	   not stamp the pair's cooldown, or a partition-wide burst would consume every affected pair's
	   one token on traces that never ran and silence them for the whole cooldown. Left unstamped,
	   the pair's next failed probe retries one interval later, so a big partition trickles its
	   traces out at maxConcurrentReactiveTraces instead of launching one goroutine per broken pair. */
	select {
	case s.mtrSem <- struct{}{}:
	default:
		if m != nil {
			m.AgentMTRReactiveCoalesced.WithLabelValues("saturated").Inc()
		}
		return
	}

	if !mtr.TryAcquire(s.source.NodeName, peer.NodeName) {
		<-s.mtrSem
		if m != nil {
			m.AgentMTRReactiveCoalesced.WithLabelValues("cooldown").Inc()
		}
		return
	}

	slog.Info("triggering MTR trace",
		"reason", failedResult.Type,
		"source", s.source.NodeName,
		"destination", peer.NodeName,
	)

	/* In its OWN goroutine, because a trace is slow by construction and this is the peer-probe loop.
	   maxHops x the tracer's timeout is thirty seconds to an unreachable peer, plus an unbounded
	   reverse lookup per answering hop — and it ran inline, in the checker's goroutine, between one
	   peer and the next. During an outage, which is exactly when traces fire, every remaining peer's
	   probe waited behind them: the fleet stopped measuring at the moment the measurements mattered.
	   mtrSem (held here, released by the goroutine) bounds how many run at once; the cooldown
	   (TryAcquire above) keeps one destination to one trace per window.
	   context.WithoutCancel: a trace that has started is finished and reported. The round it was
	   triggered from may end at any moment, and a half-written trace is worse than a slow one. */
	if m != nil {
		m.AgentMTRReactiveInflight.WithLabelValues().Inc()
	}
	go func() {
		defer func() {
			<-s.mtrSem
			if m != nil {
				m.AgentMTRReactiveInflight.WithLabelValues().Dec()
			}
		}()
		traceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mtrTraceBudget)
		defer cancel()

		mtrResult := trace(traceCtx, peer)
		mtrResult.Source = s.source.NodeName
		mtrResult.SourceZone = s.sourceZone()
		mtrResult.Destination = peer.NodeName
		mtrResult.DestZone = peer.Zone

		if s.handler != nil {
			s.handler(mtrResult)
		}
	}()
}
