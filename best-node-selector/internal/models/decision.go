package models

import "time"

// Decision is the precomputed scheduling decision stored in Redis.
// This is the ONLY data structure used by /prioritize.
type Decision struct {
	Namespace     string         `json:"namespace"`
	Service       string         `json:"service"`
	Deployment    string         `json:"deployment,omitempty"`
	PodName       string         `json:"podName,omitempty"`
	Status        string         `json:"status"`
	CurrentNodes  []string       `json:"currentNodes"`
	BestNode      string         `json:"bestNode"`
	PreferredNode string         `json:"preferredNode,omitempty"`
	Scores        map[string]int `json:"scores"`
	EvaluatedAt   time.Time      `json:"evaluatedAt"`
	WindowSeconds int            `json:"windowSeconds"`
}

const (
	StatusScheduled = "Scheduled"
	StatusNoPeers   = "NoPeers"
	StatusNoMetrics = "NoMetrics"
	StatusStale     = "StaleMetrics"
)
