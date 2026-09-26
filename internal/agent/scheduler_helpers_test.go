package agent

import (
	"context"

	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

// Test entry points over the scheduler's live API: production swaps peers with ReplacePeers, runs
// rounds with runRound and traces with triggerMTRAt.

func (s *Scheduler) UpdatePeers(peers []checker.Target) {
	s.ReplacePeers(peers, nil)
}

func (s *Scheduler) runCheckerOnce(ctx context.Context, c checker.Checker) {
	s.runRound(ctx, c, s.configFor(c.Name()))
}

func (s *Scheduler) triggerMTR(ctx context.Context, peer checker.Target, failedResult *model.CheckResult) {
	s.mu.RLock()
	gen := s.peerGen
	s.mu.RUnlock()
	s.triggerMTRAt(ctx, peer, failedResult, gen)
}
