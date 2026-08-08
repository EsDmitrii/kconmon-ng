package model

import "time"

type AgentInfo struct {
	ID       string            `json:"id"`
	NodeName string            `json:"nodeName"`
	PodName  string            `json:"podName"`
	PodIP    string            `json:"podIP"`
	Zone     string            `json:"zone"`
	Labels   map[string]string `json:"labels,omitempty"`
	// Capabilities are the opt-in feature flags this agent build advertised at
	// registration (AgentMeta.capabilities), e.g. "external-checks". A pre-M4
	// agent sends none, and the controller refuses to dispatch anything that
	// needs one rather than sending a field the old agent silently ignores.
	// omitempty keeps the JSON of an agent that advertises nothing byte-identical
	// to what pre-M4 clients already parse.
	Capabilities []string  `json:"capabilities,omitempty"`
	JoinedAt     time.Time `json:"joinedAt"`
	LastSeen     time.Time `json:"lastSeen"`
}

type PeerList struct {
	Peers     []AgentInfo `json:"peers"`
	UpdatedAt time.Time   `json:"updatedAt"`
}
