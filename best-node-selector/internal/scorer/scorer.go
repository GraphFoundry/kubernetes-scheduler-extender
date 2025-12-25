package scorer

import (
	"context"
	"log"
	"math"
	"sort"
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

	log.Printf("[SCORER] compute start service=%s nodes=%d", service, len(nodes))

	// 🔥 SAFE FALLBACK: Always write neutral decision on panic
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[SCORER][PANIC] recovered panic service=%s: %v - writing neutral decision", service, r)
			s.writeNeutralDecision(ctx, namespace, service, nodes, windowSeconds)
		}
	}()

	//  Metrics health check
	health, err := s.metrics.GetHealth(ctx)
	if err != nil {
		log.Printf("[SCORER][WARN] metrics health failed service=%s error=%v - writing neutral decision", service, err)
		s.writeNeutralDecision(ctx, namespace, service, nodes, windowSeconds)
		return
	}
	if health.Stale {
		log.Printf("[SCORER][WARN] metrics stale service=%s", service)
		//return
	}

	//  Fetch peers
	outPeers, err := s.metrics.GetPeers(ctx, service, "out", 5)
	if err != nil {
		log.Printf("[SCORER][WARN] outgoing peers failed service=%s error=%v - writing neutral decision", service, err)
		s.writeNeutralDecision(ctx, namespace, service, nodes, windowSeconds)
		return
	}

	inPeers, err := s.metrics.GetPeers(ctx, service, "in", 5)
	if err != nil {
		log.Printf("[SCORER][WARN] incoming peers failed service=%s error=%v - writing neutral decision", service, err)
		s.writeNeutralDecision(ctx, namespace, service, nodes, windowSeconds)
		return
	}

	peers := append(outPeers.Peers, inPeers.Peers...)

	log.Printf(
		"[SCORER][SCORE_CALC] found_peers service=%s total_peers=%d (outgoing=%d incoming=%d)",
		service,
		len(peers),
		len(outPeers.Peers),
		len(inPeers.Peers),
	)

	if len(peers) == 0 {
		log.Printf("[SCORER][WARN] no peers found service=%s — writing neutral decision", service)

		nodeServiceIndex, _ := s.placement.BuildNodeServiceIndex(ctx, []string{namespace})
		currentNodes := getCurrentNodes(nodeServiceIndex, service)

		scores := make(map[string]int)
		for _, node := range nodes {
			scores[node] = 50
		}

		decision := &models.Decision{
			Namespace:     namespace,
			Service:       service,
			Status:        models.StatusScheduled,
			CurrentNodes:  currentNodes,
			BestNode:      "", //NO PREFERENCE
			Scores:        scores,
			EvaluatedAt:   time.Now().UTC(),
			WindowSeconds: windowSeconds,
		}

		_ = s.repo.Save(ctx, decision)
		return
	}

	//for _, p := range peers {
	//	log.Printf(
	//		"[SCORER][DEBUG] peer service=%s rate=%.2f p95=%.2f errorRate=%.4f",
	//		p.Service,
	//		p.Metrics.Rate,
	//		p.Metrics.P95,
	//		p.Metrics.ErrorRate,
	//	)
	//}

	// Placement index
	nodeServiceIndex, err := s.placement.BuildNodeServiceIndex(ctx, []string{namespace})
	if err != nil {
		log.Printf("[SCORER][WARN] placement lookup failed service=%s error=%v - writing neutral decision", service, err)
		s.writeNeutralDecision(ctx, namespace, service, nodes, windowSeconds)
		return
	}

	//for node, svcs := range nodeServiceIndex {
	//	for svc := range svcs {
	//		log.Printf(
	//			"[SCORER][DEBUG] placement node=%s service=%s",
	//			node,
	//			svc,
	//		)
	//	}
	//}

	// Centrality
	centrality, err := s.metrics.GetCentrality(ctx)
	if err != nil {
		log.Printf("[SCORER][WARN] centrality failed service=%s error=%v - writing neutral decision", service, err)
		s.writeNeutralDecision(ctx, namespace, service, nodes, windowSeconds)
		return
	}

	pagerank, betweenness := findCentrality(centrality, service)
	// 🔥 PRIORITIZE GRAPH THEORY: Increased betweenness weight from 0.5 to 0.6
	// Betweenness centrality identifies critical communication paths
	importance := clamp01(0.4*pagerank + 0.6*betweenness)

	log.Printf(
		"[SCORER][DEBUG] centrality service=%s pagerank=%.4f betweenness=%.4f importance=%.4f",
		service,
		pagerank,
		betweenness,
		importance,
	)

	// ⃣Raw scoring
	rawScores := make(map[string]float64)
	var maxRaw float64

	for _, node := range nodes {
		raw := 0.0

		log.Printf(
			"[SCORER][SCORE_CALC] scoring_node node=%s peers_to_check=%d",
			node,
			len(peers),
		)

		for _, p := range peers {
			peerName := strings.ToLower(p.Service)

			if hasService(nodeServiceIndex, node, peerName) {
				w := peerWeight(p)
				raw += w

				log.Printf(
					"[SCORER][SCORE_CALC] MATCH node=%s peer=%s weight=%.4f (rate=%.2f p95=%.2f err=%.4f)",
					node,
					peerName,
					w,
					p.Metrics.Rate,
					p.Metrics.P95,
					p.Metrics.ErrorRate,
				)
			} else {
				log.Printf(
					"[SCORER][DEBUG] NO_MATCH node=%s peer=%s",
					node,
					peerName,
				)
			}
		}

		log.Printf(
			"[SCORER][SCORE_CALC] raw_before_importance node=%s raw=%.6f (sum of peer weights)",
			node,
			raw,
		)

		raw *= (0.5 + 0.5*importance)

		log.Printf(
			"[SCORER][SCORE_CALC] raw_after_importance node=%s raw=%.6f (multiplied by importance_factor=%.4f)",
			node,
			raw,
			0.5+0.5*importance,
		)

		rawScores[node] = raw

		if raw > maxRaw {
			maxRaw = raw
		}
	}

	log.Printf(
		"[SCORER][SCORE_CALC] normalization max_raw=%.6f service=%s",
		maxRaw,
		service,
	)

	if maxRaw <= 0.000001 {
		log.Printf("[SCORER][WARN] all raw scores zero service=%s — writing neutral decision", service)

		currentNodes := getCurrentNodes(nodeServiceIndex, service)

		scores := make(map[string]int)
		for _, node := range nodes {
			scores[node] = 50
		}

		decision := &models.Decision{
			Namespace:     namespace,
			Service:       service,
			Status:        models.StatusScheduled,
			CurrentNodes:  currentNodes,
			BestNode:      "", // NO PREFERENCE
			Scores:        scores,
			EvaluatedAt:   time.Now().UTC(),
			WindowSeconds: windowSeconds,
		}

		_ = s.repo.Save(ctx, decision)
		return
	}

	//  Normalize to 0–100
	scores := make(map[string]int)
	bestNode := ""
	bestScore := -1
	tiedNodes := []string{}

	for node, raw := range rawScores {
		normalizedRatio := raw / maxRaw
		score := int(math.Round(clamp(normalizedRatio*100.0, 0, 100)))
		scores[node] = score

		log.Printf(
			"[SCORER][SCORE_CALC] final_score node=%s raw=%.6f normalized_ratio=%.4f score=%d",
			node,
			raw,
			normalizedRatio,
			score,
		)

		if score > bestScore {
			bestScore = score
			bestNode = node
			tiedNodes = []string{node}
		} else if score == bestScore {
			tiedNodes = append(tiedNodes, node)
		}
	}

	// 🔥 DETERMINISTIC TIE-BREAKING: Use alphabetical order instead of random/empty
	if len(tiedNodes) > 1 {
		sort.Strings(tiedNodes)
		bestNode = tiedNodes[0]
		log.Printf("[SCORER][DEBUG] tie broken deterministically: selected=%s from=%v", bestNode, tiedNodes)
	}

	currentNodes := getCurrentNodes(nodeServiceIndex, service)

	decision := &models.Decision{
		Namespace:     namespace,
		Service:       service,
		Status:        models.StatusScheduled,
		CurrentNodes:  currentNodes,
		BestNode:      bestNode, // only set if unique winner
		Scores:        scores,
		EvaluatedAt:   time.Now().UTC(),
		WindowSeconds: windowSeconds,
	}

	if err := s.repo.Save(ctx, decision); err != nil {
		log.Printf("[SCORER][ERROR] redis save failed service=%s error=%v", service, err)
		return
	}

	log.Printf(
		"[SCORER] compute done service=%s best=%s current nodes=%s duration_ms=%d",
		service,
		bestNode,
		currentNodes,
		time.Since(start).Milliseconds(),
	)
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

func peerWeight(p services.Peer) float64 {
	rate := clamp(p.Metrics.Rate/50.0, 0, 1)
	lat := clamp(p.Metrics.P95/300.0, 0, 1)
	err := clamp01(p.Metrics.ErrorRate)

	// 🔥 PRIORITIZE LATENCY: Increased latency weight from 0.4 to 0.7
	// Network proximity is critical for performance
	w := (0.3*rate + 0.7*lat) * (1.0 - 0.8*err)
	finalWeight := clamp(w, 0.05, 1.0)

	log.Printf(
		"[SCORER][SCORE_CALC] peer_weight service=%s rate_norm=%.4f lat_norm=%.4f err_norm=%.4f combined_weight=%.4f final_weight=%.4f (formula: (0.3*rate + 0.7*lat) * (1.0 - 0.8*err))",
		p.Service,
		rate,
		lat,
		err,
		w,
		finalWeight,
	)

	return finalWeight
}

func findCentrality(c services.CentralityResponse, service string) (float64, float64) {
	target := strings.ToLower(service)
	for _, s := range c.Scores {
		if strings.ToLower(s.Service) == target {
			return s.Pagerank, s.Betweenness
		}
	}
	return 0.1, 0.1
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
