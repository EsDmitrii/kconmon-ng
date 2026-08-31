package model

import "time"

type NodeInfo struct {
	Name   string            `json:"name"`
	Zone   string            `json:"zone"`
	Labels map[string]string `json:"labels,omitempty"`
	Ready  bool              `json:"ready"`
}

type TopologySnapshot struct {
	Nodes     []NodeInfo  `json:"nodes"`
	Agents    []AgentInfo `json:"agents"`
	Timestamp time.Time   `json:"timestamp"`
	// ProbePlan is present ONLY while a sparse topology plan is in force: source node name to the
	// sorted node names it is planned to probe. Absent (omitempty) means full mesh — every pair is
	// intended — which keeps the pre-M10 payload byte-identical for full-mesh fleets.
	ProbePlan map[string][]string `json:"probePlan,omitempty"`
}
