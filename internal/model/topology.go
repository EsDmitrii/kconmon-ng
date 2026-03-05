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
}
