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
	// Port is what the TCP checker dials: the peer's httpPort, or the operator's port for External
	// targets. UDPPort is the peer's echo port; 0 = use the checker's own configured port.
	Port    int
	UDPPort int
	// External marks a destination that is NOT a kconmon peer. Two checkers speak the agent's own
	// protocol once the transport is up -- TCP asks /readyz, UDP probes the agent's echo port -- and
	// against a plain host both of those are the wrong question: the port answered, and the check
	// still reported a failure. For an external target Port is what the OPERATOR asked for, and the
	// transport-level probe IS the check.
	External bool
}

// PeerPorts are the probing agent's OWN listener ports, the fallback for a peer that reported none:
// an agent older than 2.4.0 sends no ports and still assumes the fleet-wide "one port set" contract.
type PeerPorts struct {
	HTTP int
	UDP  int
}

type Checker interface {
	Name() model.CheckType
	Check(ctx context.Context, target Target) model.CheckResult
}
