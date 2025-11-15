package services

import (
	"context"
	"strings"
	"time"
)

// SampleMetricsProvider returns stable sample metrics from within this repo.
// Later, replace by implementing HTTPMetricsProvider without touching algorithm code.
type SampleMetricsProvider struct{}

func NewSampleMetricsProvider() *SampleMetricsProvider {
	return &SampleMetricsProvider{}
}

func (p *SampleMetricsProvider) GetHealth(ctx context.Context) (GraphHealth, error) {
	// Always healthy and fresh in sample data
	secs := 10
	return GraphHealth{
		Status:                "OK",
		LastUpdatedSecondsAgo: &secs,
		WindowMinutes:         5,
		Stale:                 false,
	}, nil
}

func (p *SampleMetricsProvider) GetPeers(ctx context.Context, serviceName string, direction string, limit int) (PeersResponse, error) {
	// Normalize direction
	dir := strings.ToLower(direction)
	if dir != "in" && dir != "out" {
		dir = "out"
	}
	if limit <= 0 {
		limit = 5
	}

	// Stable sample peers for demo/testing
	peers := []Peer{
		{
			Service: "paymentservice",
			Metrics: PeerMetrics{
				Rate:      20,
				P50:       12,
				P95:       80,
				P99:       150,
				ErrorRate: 0.01,
			},
		},
		{
			Service: "shippingservice",
			Metrics: PeerMetrics{
				Rate:      8,
				P50:       25,
				P95:       120,
				P99:       260,
				ErrorRate: 0.02,
			},
		},
		{
			Service: "inventoryservice",
			Metrics: PeerMetrics{
				Rate:      5,
				P50:       15,
				P95:       60,
				P99:       110,
				ErrorRate: 0.00,
			},
		},
	}

	if limit < len(peers) {
		peers = peers[:limit]
	}

	return PeersResponse{
		Service:       serviceName,
		Direction:     dir,
		WindowMinutes: 5,
		Peers:         peers,
	}, nil
}

func (p *SampleMetricsProvider) GetCentrality(ctx context.Context) (CentralityResponse, error) {
	// Sample centrality values
	return CentralityResponse{
		WindowMinutes: 5,
		Scores: []CentralityScore{
			{Service: "frontend", Pagerank: 0.65, Betweenness: 0.40},
			{Service: "checkoutservice", Pagerank: 0.80, Betweenness: 0.55},
			{Service: "paymentservice", Pagerank: 0.70, Betweenness: 0.45},
		},
	}, nil
}

func (p *SampleMetricsProvider) GetNodePenalty(ctx context.Context, nodeName string) (float64, error) {
	// Lower is better. This helps create deterministic scheduling behavior in local tests.
	switch nodeName {
	case "minikube":
		return 0.30, nil
	case "minikube-m02":
		return 0.10, nil
	case "minikube-m03":
		return 0.00, nil
	default:
		return 0.20, nil
	}
}

// Optional helper if you later want a “generated at” feel
func unixNow() int64 {
	return time.Now().Unix()
}
