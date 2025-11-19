package models

import "time"

// Decision is the precomputed scheduling decision stored in Redis.
// This is the ONLY data structure used by /prioritize.
type Decision struct {
	Namespace     string         `json:"namespace"`
	Service       string         `json:"service"`
	Status        string         `json:"status"`
	CurrentNode   string         `json:"currentNode"`
	BestNode      string         `json:"bestNode"`
	Scores        map[string]int `json:"scores"`
	EvaluatedAt   time.Time      `json:"evaluatedAt"`
	WindowSeconds int            `json:"windowSeconds"`
}
