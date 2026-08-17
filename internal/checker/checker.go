package checker

import (
	"context"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

type Target struct {
	AgentID  string
	NodeName string
	PodIP    string
	Zone     string
	Port     int
	// External marks a destination that is NOT a kconmon peer. Two checkers speak the agent's own
	// protocol once the transport is up -- TCP asks /readyz, UDP probes the agent's echo port -- and
	// against a plain host both of those are the wrong question: the port answered, and the check
	// still reported a failure. For an external target Port is what the OPERATOR asked for, and the
	// transport-level probe IS the check.
	External bool
}

type Checker interface {
	Name() model.CheckType
	Check(ctx context.Context, target Target) model.CheckResult
}
