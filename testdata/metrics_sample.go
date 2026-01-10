package testdata

import (
	"context"
	"scheduler-extender/internal/services"
	"strings"
)

// SampleMetricsProvider returns stable sample metrics from within this repo.
// Later, replace by implementing HTTPMetricsProvider without touching algorithm code.
type SampleMetricsProvider struct{}

func NewSampleMetricsProvider() *SampleMetricsProvider {
	return &SampleMetricsProvider{}
}

func (p *SampleMetricsProvider) GetHealth(ctx context.Context) (services.GraphHealth, error) {
	// Always healthy and fresh in sample data
	secs := 10
	return services.GraphHealth{
		Status:                "OK",
		LastUpdatedSecondsAgo: &secs,
		WindowMinutes:         5,
		Stale:                 false,
	}, nil
}

func (p *SampleMetricsProvider) GetPeers(ctx context.Context, serviceName string, direction string, limit int) (services.PeersResponse, error) {
	// Normalize direction
	dir := strings.ToLower(direction)
	if dir != "in" && dir != "out" {
		dir = "out"
	}
	if limit <= 0 {
		limit = 5
	}

	// Stable sample peers for demo/testing
	peers := []services.Peer{
		{
			Service: "samplepaymentservice",
			Metrics: services.PeerMetrics{
				Rate:      20,
				P50:       12,
				P95:       80,
				P99:       150,
				ErrorRate: 0.01,
			},
		},
		{
			Service: "shippingservice",
			Metrics: services.PeerMetrics{
				Rate:      8,
				P50:       25,
				P95:       120,
				P99:       260,
				ErrorRate: 0.02,
			},
		},
		{
			Service: "inventoryservice",
			Metrics: services.PeerMetrics{
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

	return services.PeersResponse{
		Service:       serviceName,
		Direction:     dir,
		WindowMinutes: 5,
		Peers:         peers,
	}, nil
}

func (p *SampleMetricsProvider) GetCentrality(ctx context.Context) (services.CentralityResponse, error) {
	// Sample centrality values
	return services.CentralityResponse{
		WindowMinutes: 5,
		Scores: []services.CentralityScore{
			{Service: "frontend", Pagerank: 0.65, Betweenness: 0.40},
			{Service: "checkoutservice", Pagerank: 0.80, Betweenness: 0.55},
			{Service: "samplepaymentservice", Pagerank: 0.70, Betweenness: 0.45},
		},
	}, nil
}
