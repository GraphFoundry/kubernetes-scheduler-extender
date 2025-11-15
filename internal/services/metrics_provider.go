package services

import "context"

// /graph/health
type GraphHealth struct {
	Status                string `json:"status"`
	LastUpdatedSecondsAgo *int   `json:"lastUpdatedSecondsAgo"`
	WindowMinutes         int    `json:"windowMinutes"`
	Stale                 bool   `json:"stale"`
}

// /services/:service/peers
type PeerMetrics struct {
	Rate      float64 `json:"rate"`
	P50       float64 `json:"p50"`
	P95       float64 `json:"p95"`
	P99       float64 `json:"p99"`
	ErrorRate float64 `json:"errorRate"`
}

type Peer struct {
	Service string      `json:"service"`
	Metrics PeerMetrics `json:"metrics"`
}

type PeersResponse struct {
	Service       string `json:"service"`
	Direction     string `json:"direction"` // "out" or "in"
	WindowMinutes int    `json:"windowMinutes"`
	Peers         []Peer `json:"peers"`
}

// /centrality
type CentralityScore struct {
	Service     string  `json:"service"`
	Pagerank    float64 `json:"pagerank"`
	Betweenness float64 `json:"betweenness"`
}

type CentralityResponse struct {
	WindowMinutes int               `json:"windowMinutes"`
	Scores        []CentralityScore `json:"scores"`
}

type MetricsProvider interface {
	GetHealth(ctx context.Context) (GraphHealth, error)
	GetPeers(ctx context.Context, serviceName string, direction string, limit int) (PeersResponse, error)
	GetCentrality(ctx context.Context) (CentralityResponse, error)

	// Optional: keep it for later. For now sample implementation can return fixed values.
	GetNodePenalty(ctx context.Context, nodeName string) (float64, error)
}
