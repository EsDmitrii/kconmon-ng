package agent

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

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
}

func NewScheduler(source checker.Target, handler ResultHandler) *Scheduler {
	return &Scheduler{
		configs: make(map[model.CheckType]SchedulerConfig),
		handler: handler,
		source:  source,
		pauseCh: make(chan struct{}),
	}
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

	if cfg.NodeLocal {
		result := c.Check(ctx, checker.Target{})
		result.Source = s.source.NodeName
		result.SourceZone = s.source.Zone

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

	for _, peer := range peers {
		result := c.Check(ctx, peer)
		result.Source = s.source.NodeName
		result.SourceZone = s.source.Zone
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
	}
}

func (s *Scheduler) triggerMTR(ctx context.Context, peer checker.Target, failedResult *model.CheckResult) {
	s.mu.RLock()
	mtr := s.mtrChecker
	s.mu.RUnlock()

	if mtr == nil {
		return
	}

	if failedResult.Type == model.CheckDNS || failedResult.Type == model.CheckHTTP || failedResult.Type == model.CheckMTR {
		return
	}

	if !mtr.TryAcquire(s.source.NodeName, peer.NodeName) {
		return
	}

	slog.Info("triggering MTR trace",
		"reason", failedResult.Type,
		"source", s.source.NodeName,
		"destination", peer.NodeName,
	)

	mtrResult := mtr.Check(ctx, peer)
	mtrResult.Source = s.source.NodeName
	mtrResult.SourceZone = s.source.Zone
	mtrResult.Destination = peer.NodeName
	mtrResult.DestZone = peer.Zone

	if s.handler != nil {
		s.handler(mtrResult)
	}
}
