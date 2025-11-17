package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"
)

type HTTPMetricsProvider struct {
	baseURL string
	client  *http.Client
}

func NewHTTPMetricsProvider(baseURL string, timeout time.Duration) *HTTPMetricsProvider {
	return &HTTPMetricsProvider{
		baseURL: baseURL,
		client:  &http.Client{Timeout: timeout},
	}
}

func (p *HTTPMetricsProvider) GetHealth(ctx context.Context) (GraphHealth, error) {
	log.Printf("[METRICS][HTTP] fetching metrics health")

	var out GraphHealth
	if err := p.getJSON(ctx, "/graph/health", &out); err != nil {
		log.Printf("[METRICS][HTTP][WARN] health check failed error=%v", err)
		return GraphHealth{}, err
	}

	log.Printf("[METRICS][HTTP] health status=%s stale=%v", out.Status, out.Stale)
	return out, nil
}

func (p *HTTPMetricsProvider) GetPeers(ctx context.Context, serviceName string, direction string, limit int) (PeersResponse, error) {
	log.Printf(
		"[METRICS][HTTP] fetching peers service=%s direction=%s limit=%d",
		serviceName,
		direction,
		limit,
	)

	q := url.Values{}
	q.Set("direction", direction)
	q.Set("limit", fmt.Sprintf("%d", limit))

	path := fmt.Sprintf("/services/%s/peers?%s", url.PathEscape(serviceName), q.Encode())

	var out PeersResponse
	if err := p.getJSON(ctx, path, &out); err != nil {
		log.Printf("[METRICS][HTTP][WARN] failed to fetch peers error=%v", err)
		return PeersResponse{}, err
	}

	log.Printf("[METRICS][HTTP] peers fetched count=%d", len(out.Peers))
	return out, nil
}

func (p *HTTPMetricsProvider) GetCentrality(ctx context.Context) (CentralityResponse, error) {
	log.Printf("[METRICS][HTTP] fetching centrality scores")

	var out CentralityResponse
	if err := p.getJSON(ctx, "/centrality", &out); err != nil {
		log.Printf("[METRICS][HTTP][WARN] failed to fetch centrality error=%v", err)
		return CentralityResponse{}, err
	}

	log.Printf("[METRICS][HTTP] centrality entries=%d", len(out.Scores))
	return out, nil
}

func (p *HTTPMetricsProvider) GetNodePenalty(ctx context.Context, nodeName string) (float64, error) {
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

func (p *HTTPMetricsProvider) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+path, nil)
	if err != nil {
		return err
	}

	res, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("metrics api %s returned status %d", path, res.StatusCode)
	}

	return json.NewDecoder(res.Body).Decode(out)
}
