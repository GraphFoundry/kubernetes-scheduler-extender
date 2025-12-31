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

	// 2b. Get node labels for worker detection
	nodeLabels, _ := s.placement.GetNodeLabels(ctx)

	// 3. No peers = neutral decision
	if len(peers) == 0 {
		log.Printf("[SCORER] no peers for service=%s", service)
		s.writeNeutralDecision(ctx, namespace, service, nodes, windowSeconds)
		return
	}

	// 4. Filter out primary/master nodes if workers exist
	workerNodes := filterWorkerNodes(nodes, nodeLabels)
	targetNodes := nodes
	if len(workerNodes) > 0 {
		targetNodes = workerNodes
		log.Printf("[SCORER] filtering to worker nodes only: %v", workerNodes)
	}

	// 5. Simple scoring: count co-located peers per node + worker bonus
	scores := make(map[string]int)
	var maxCount int

	for _, node := range targetNodes {
		count := 0
		for _, p := range peers {
			if hasService(nodeServiceIndex, node, strings.ToLower(p.Service)) {
				count++
			}
		}
		// Add 1 worker bonus point for non-primary nodes
		if isWorkerNode(node, nodeLabels) {
			count++
		}
		scores[node] = count
		if count > maxCount {
			maxCount = count
		}
	}

	// Set score 0 for primary nodes (excluded from scheduling)
	for _, node := range nodes {
		if _, ok := scores[node]; !ok {
			scores[node] = 0
		}
	}

	// 6. Normalize scores to 0-100
	if maxCount > 0 {
		for node, count := range scores {
			if count > 0 {
				scores[node] = int(math.Round(float64(count) / float64(maxCount) * 100))
			}
		}
	} else {
		for _, node := range targetNodes {
			scores[node] = 50
		}
	}

	// 7. Find best node (deterministic tie-break, only from target nodes)
	bestNode := ""
	bestScore := -1
	for _, node := range targetNodes {
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

// filterWorkerNodes returns only worker nodes (excludes primary/master/control-plane nodes)
func filterWorkerNodes(nodes []string, nodeLabels map[string]map[string]string) []string {
	workers := []string{}
	for _, node := range nodes {
		if isWorkerNode(node, nodeLabels) {
			workers = append(workers, node)
		}
	}
	return workers
}

// isWorkerNode returns true if the node is not a primary/master/control-plane node
func isWorkerNode(nodeName string, nodeLabels map[string]map[string]string) bool {
	// Check minikube label first (most reliable)
	if nodeLabels != nil {
		if labels, ok := nodeLabels[nodeName]; ok {
			if primary, ok := labels["minikube.k8s.io/primary"]; ok {
				return primary == "false"
			}
		}
	}
	// Fallback to name-based detection
	nodeLower := strings.ToLower(nodeName)
	return !strings.Contains(nodeLower, "primary") &&
		!strings.Contains(nodeLower, "master") &&
		!strings.Contains(nodeLower, "control-plane")
}

// writeNeutralDecision writes a neutral decision for fallback scenarios
// Still prefers worker nodes over primary nodes
func (s *Scorer) writeNeutralDecision(
	ctx context.Context,
	namespace string,
	service string,
	nodes []string,
	windowSeconds int,
) {
	// Get node labels for worker detection
	nodeLabels, _ := s.placement.GetNodeLabels(ctx)

	// Get current nodes where service is running
	nodeServiceIndex, _ := s.placement.BuildNodeServiceIndex(ctx, []string{namespace})
	currentNodes := getCurrentNodes(nodeServiceIndex, service)

	// Filter to worker nodes if available
	workerNodes := filterWorkerNodes(nodes, nodeLabels)
	targetNodes := nodes
	if len(workerNodes) > 0 {
		targetNodes = workerNodes
	}

	scores := make(map[string]int)
	// Worker nodes get score 50, primary nodes get score 0
	for _, node := range nodes {
		if isWorkerNode(node, nodeLabels) {
			scores[node] = 50
		} else {
			scores[node] = 0
		}
	}

	// Pick best worker node (deterministic)
	bestNode := ""
	if len(targetNodes) > 0 {
		bestNode = targetNodes[0]
		for _, node := range targetNodes {
			if node < bestNode {
				bestNode = node
			}
		}
	}

	decision := &models.Decision{
		Namespace:     namespace,
		Service:       service,
		Status:        models.StatusNoMetrics,
		CurrentNodes:  currentNodes,
		BestNode:      bestNode,
		Scores:        scores,
		EvaluatedAt:   time.Now().UTC(),
		WindowSeconds: windowSeconds,
	}

	if err := s.repo.Save(ctx, decision); err != nil {
		log.Printf("[SCORER][ERROR] failed to save neutral decision service=%s: %v", service, err)
	}
}
