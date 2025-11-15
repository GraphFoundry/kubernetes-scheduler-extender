package services

import (
	"context"
	"log"
	"math"
	"sort"
	"strings"

	"scheduler-extender/internal/models"
)

type SchedulerService struct {
	metrics   MetricsProvider
	placement PlacementProvider
}

func NewSchedulerService(metrics MetricsProvider, placement PlacementProvider) *SchedulerService {
	return &SchedulerService{metrics: metrics, placement: placement}
}

// podObj: the pod being scheduled (raw JSON decoded object)
// fallbackService: used only if pod labels don't contain service name
func (s *SchedulerService) PrioritizeNodes(
	ctx context.Context,
	podObj any,
	fallbackService string,
	nodeNames []string,
	topK int,
) []models.HostPriority {

	// 1) Determine the service name for this pod
	targetService := extractServiceFromPod(podObj)
	if targetService == "" {
		targetService = fallbackService
	}
	if targetService == "" {
		return neutral(nodeNames)
	}

	// 2) Health check (avoid stale metrics)
	health, err := s.metrics.GetHealth(ctx)
	if err != nil || health.Stale {
		return neutral(nodeNames)
	}

	// 3) Get top peers (network-relevant edges)
	outPeers, err := s.metrics.GetPeers(ctx, targetService, "out", topK)
	if err != nil {
		return neutral(nodeNames)
	}
	inPeers, err := s.metrics.GetPeers(ctx, targetService, "in", topK)
	if err != nil {
		return neutral(nodeNames)
	}

	peers := append(outPeers.Peers, inPeers.Peers...)
	if len(peers) == 0 {
		return neutral(nodeNames)
	}

	// 4) Build node -> set(service) index from Kubernetes
	nodeServiceIndex, err := s.placement.BuildNodeServiceIndex(ctx)
	if err != nil {
		return neutral(nodeNames)
	}

	// 5) Optional: centrality can scale impact, but colocation is the main factor.
	centrality, err := s.metrics.GetCentrality(ctx)
	if err != nil {
		return neutral(nodeNames)
	}
	pagerank, betweenness := findCentrality(centrality, targetService)
	importance := clamp01(0.5*pagerank + 0.5*betweenness)

	// 6) Score each node by colocated peer weight
	// scoreRaw(node) = sum(peerWeight if peer service exists on node)
	rawScores := make(map[string]float64)
	var maxRaw float64

	for _, n := range nodeNames {
		raw := 0.0
		for _, p := range peers {
			peerName := strings.ToLower(p.Service)
			if hasService(nodeServiceIndex, n, peerName) {
				raw += peerWeight(p)
			}
		}
		// importance scales how strongly we care
		raw = raw * (0.5 + 0.5*importance)

		rawScores[n] = raw
		if raw > maxRaw {
			maxRaw = raw
		}
	}

	// 7) Convert raw scores to 0..100 (normalize)
	// If maxRaw is 0, it means no node has any peer → neutral.
	if maxRaw <= 0.000001 {
		return neutral(nodeNames)
	}

	out := make([]models.HostPriority, 0, len(nodeNames))
	for _, n := range nodeNames {
		normalized := (rawScores[n] / maxRaw) * 100.0
		out = append(out, models.HostPriority{
			Host:  n,
			Score: int(math.Round(clamp(normalized, 0, 100))),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	log.Printf("prioritize result: %+v", out)
	return out
}

func hasService(index map[string]map[string]struct{}, nodeName string, serviceName string) bool {
	set, ok := index[nodeName]
	if !ok {
		return false
	}
	_, ok = set[serviceName]
	if ok {
		return true
	}
	// also try exact (case sensitive) if labels were not lowered
	_, ok = set[serviceName]
	return ok
}

func peerWeight(p Peer) float64 {
	// Higher rate + higher p95 => more important to colocate.
	// Higher error rate reduces confidence/benefit.
	rate := clamp(p.Metrics.Rate/50.0, 0, 1)
	lat := clamp(p.Metrics.P95/300.0, 0, 1)
	err := clamp01(p.Metrics.ErrorRate)
	w := (0.6*rate + 0.4*lat) * (1.0 - 0.7*err)

	// Keep it never zero, so one colocated peer always helps a bit.
	return clamp(w, 0.05, 1.0)
}

func findCentrality(c CentralityResponse, serviceName string) (pagerank float64, betweenness float64) {
	target := strings.ToLower(serviceName)
	for _, s := range c.Scores {
		if strings.ToLower(s.Service) == target {
			return s.Pagerank, s.Betweenness
		}
	}
	return 0.1, 0.1
}

func extractServiceFromPod(podObj any) string {
	// We expect podObj decoded as map[string]any (because ExtenderArgs.Pod is interface{}).
	m, ok := podObj.(map[string]any)
	if !ok {
		return ""
	}
	meta, _ := m["metadata"].(map[string]any)
	labels, _ := meta["labels"].(map[string]any)

	// Try common label keys
	for _, key := range []string{"app.kubernetes.io/name", "app", "k8s-app"} {
		if v, ok := labels[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func neutral(nodes []string) []models.HostPriority {
	out := make([]models.HostPriority, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, models.HostPriority{Host: n, Score: 50})
	}
	return out
}

func clamp01(x float64) float64 { return clamp(x, 0, 1) }
func clamp(x, lo, hi float64) float64 {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}
