package model

import "time"

// Shared vocabulary for agent labels and capabilities, defined once so the agent, the controller,
// the Console and the CLI never disagree on a wire string.
const (
	// LabelExternal marks an agent running outside any Pod; identity.go sets it to "true".
	LabelExternal = "kconmon-ng.io/external"
	// LabelHostNetwork marks an in-cluster agent that runs with hostNetwork and so advertises the
	// node's address rather than a pod IP.
	LabelHostNetwork = "kconmon-ng.io/host-network"
	// CapabilityPlanePrefix prefixes the per-protocol capabilities ("plane:tcp", "plane:udp", ...);
	// an agent advertising none is read as "unknown, all planes", never as unsupported.
	CapabilityPlanePrefix = "plane:"
)

type AgentInfo struct {
	ID       string            `json:"id"`
	NodeName string            `json:"nodeName"`
	PodName  string            `json:"podName"`
	PodIP    string            `json:"podIP"`
	Zone     string            `json:"zone"`
	Labels   map[string]string `json:"labels,omitempty"`
	// Capabilities are the opt-in feature flags this agent build advertised at registration
	// (AgentMeta.capabilities); a pre-M4 agent sends none, and the controller refuses to dispatch
	// anything that needs one rather than sending a field the old agent silently ignores.
	Capabilities []string `json:"capabilities,omitempty"`
	// Listener ports the agent serves peers on, as reported at registration; 0 = unknown (agent
	// older than 2.4.0), in which case a probing peer falls back to its own configured port.
	HTTPPort    int       `json:"httpPort,omitempty"`
	UDPPort     int       `json:"udpPort,omitempty"`
	MetricsPort int       `json:"metricsPort,omitempty"`
	JoinedAt    time.Time `json:"joinedAt"`
	LastSeen    time.Time `json:"lastSeen"`
}

// IsExternal reports whether the agent registered from outside any Pod (bare host through the
// external gateway), which is exactly the LabelExternal="true" marker identity.go writes.
func (a *AgentInfo) IsExternal() bool {
	return a.Labels[LabelExternal] == "true"
}

type PeerList struct {
	Peers     []AgentInfo `json:"peers"`
	UpdatedAt time.Time   `json:"updatedAt"`
}
