package scorer

import (
	"context"
	"log"
	"math"
	"strings"
	"time"

	"best-node-selector/internal/models"
	"best-node-selector/internal/services"
)

type Scorer struct {
	metrics   services.MetricsProvider
	placement services.PlacementProvider
	repo      DecisionWriter
}

type DecisionWriter interface {
	Save(ctx context.Context, d *models.Decision) error
}

func New(
	metrics services.MetricsProvider,
	placement services.PlacementProvider,
	repo DecisionWriter,
) *Scorer {
	return &Scorer{
		metrics:   metrics,
		placement: placement,
		repo:      repo,
	}
}

func (s *Scorer) ComputeForService(
	ctx context.Context,
	namespace string,
	service string,
	nodes []string,
	windowSeconds int,
) {
	start := time.Now()
	log.Printf("[SCORER] scoring service=%s nodes=%d", service, len(nodes))

	defer func() {
		if r := recover(); r != nil {
			log.Printf("[SCORER][PANIC] service=%s: %v", service, r)
			s.writeNeutralDecision(ctx, namespace, service, nodes, windowSeconds)
		}
	}()

	// 1. Get peers (services this service communicates with)
	outPeers, err := s.metrics.GetPeers(ctx, service, "out", 1)
	if err != nil {
		log.Printf("[SCORER][WARN] peers failed service=%s: %v", service, err)
		s.writeNeutralDecision(ctx, namespace, service, nodes, windowSeconds)
		return
	}
	inPeers, _ := s.metrics.GetPeers(ctx, service, "in", 1)
	peers := append(outPeers.Peers, inPeers.Peers...)

	// 2. Get placement index (which services run on which nodes)
	nodeServiceIndex, err := s.placement.BuildNodeServiceIndex(ctx, []string{namespace})
	if err != nil {
		s.writeNeutralDecision(ctx, namespace, service, nodes, windowSeconds)
		return
	}

	// 3. No peers = neutral decision
	if len(peers) == 0 {
		log.Printf("[SCORER] no peers for service=%s", service)
		s.writeNeutralDecision(ctx, namespace, service, nodes, windowSeconds)
		return
	}

	// 4. Simple scoring: count co-located peers per node
	scores := make(map[string]int)
	var maxCount int

	for _, node := range nodes {
		count := 0
		for _, p := range peers {
			if hasService(nodeServiceIndex, node, strings.ToLower(p.Service)) {
				count++
			}
		}
		scores[node] = count
		if count > maxCount {
			maxCount = count
		}
	}

	// 5. Normalize scores to 0-100
	if maxCount > 0 {
		for node, count := range scores {
			scores[node] = int(math.Round(float64(count) / float64(maxCount) * 100))
		}
	} else {
		for _, node := range nodes {
			scores[node] = 50
		}
	}

	// 6. Find best node (deterministic tie-break)
	bestNode := ""
	bestScore := -1
	for _, node := range nodes {
		if scores[node] > bestScore || (scores[node] == bestScore && node < bestNode) {
			bestScore = scores[node]
			bestNode = node
		}
	}

	currentNodes := getCurrentNodes(nodeServiceIndex, service)

	decision := &models.Decision{
		Namespace:     namespace,
		Service:       service,
		Status:        models.StatusScheduled,
		CurrentNodes:  currentNodes,
		BestNode:      bestNode,
		Scores:        scores,
		EvaluatedAt:   time.Now().UTC(),
		WindowSeconds: windowSeconds,
	}

	if err := s.repo.Save(ctx, decision); err != nil {
		log.Printf("[SCORER][ERROR] save failed service=%s: %v", service, err)
		return
	}

	log.Printf("[SCORER] done service=%s best=%s peers=%d duration=%dms",
		service, bestNode, len(peers), time.Since(start).Milliseconds())
}

/* ================= helpers (unchanged math) ================= */

func getCurrentNodes(
	index map[string]map[string]struct{},
	service string,
) []string {
	nodes := []string{}
	for node, svcs := range index {
		if _, ok := svcs[strings.ToLower(service)]; ok {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

func hasService(index map[string]map[string]struct{}, node, service string) bool {
	set, ok := index[node]
	if !ok {
		return false
	}
	_, ok = set[service]
	return ok
}



// writeNeutralDecision writes a neutral decision for fallback scenarios
// 🔥 SAFE FALLBACK: Used when metrics/scoring fails to let default scheduler decide
func (s *Scorer) writeNeutralDecision(
	ctx context.Context,
	namespace string,
	service string,
	nodes []string,
	windowSeconds int,
) {
	scores := make(map[string]int)
	for _, node := range nodes {
		scores[node] = 50 // Neutral score - no preference
	}

	decision := &models.Decision{
		Namespace:     namespace,
		Service:       service,
		Status:        models.StatusNoMetrics,
		CurrentNodes:  []string{},
		BestNode:      "", // No preference - default scheduler decides
		Scores:        scores,
		EvaluatedAt:   time.Now().UTC(),
		WindowSeconds: windowSeconds,
	}

	if err := s.repo.Save(ctx, decision); err != nil {
		log.Printf("[SCORER][ERROR] failed to save neutral decision service=%s: %v", service, err)
	}
}
