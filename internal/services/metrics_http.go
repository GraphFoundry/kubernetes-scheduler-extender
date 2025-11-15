package services

import (
	"context"
	"encoding/json"
	"fmt"
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
	var out GraphHealth
	if err := p.getJSON(ctx, "/graph/health", &out); err != nil {
		return GraphHealth{}, err
	}
	return out, nil
}

func (p *HTTPMetricsProvider) GetPeers(ctx context.Context, serviceName string, direction string, limit int) (PeersResponse, error) {
	q := url.Values{}
	q.Set("direction", direction)
	q.Set("limit", fmt.Sprintf("%d", limit))

	path := fmt.Sprintf("/services/%s/peers?%s", url.PathEscape(serviceName), q.Encode())

	var out PeersResponse
	if err := p.getJSON(ctx, path, &out); err != nil {
		return PeersResponse{}, err
	}
	return out, nil
}

func (p *HTTPMetricsProvider) GetCentrality(ctx context.Context) (CentralityResponse, error) {
	var out CentralityResponse
	if err := p.getJSON(ctx, "/centrality", &out); err != nil {
		return CentralityResponse{}, err
	}
	return out, nil
}

func (p *HTTPMetricsProvider) GetNodePenalty(ctx context.Context, nodeName string) (float64, error) {
	// Leader API does not provide node penalty yet.
	// Keep same deterministic logic for now.
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
