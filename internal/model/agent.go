package model

import "time"

type AgentInfo struct {
	ID       string            `json:"id"`
	NodeName string            `json:"nodeName"`
	PodName  string            `json:"podName"`
	PodIP    string            `json:"podIP"`
	Zone     string            `json:"zone"`
	Labels   map[string]string `json:"labels,omitempty"`
	JoinedAt time.Time         `json:"joinedAt"`
	LastSeen time.Time         `json:"lastSeen"`
}

type PeerList struct {
	Peers     []AgentInfo `json:"peers"`
	UpdatedAt time.Time   `json:"updatedAt"`
}
