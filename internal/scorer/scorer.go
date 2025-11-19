package scorer

import (
	"context"
	"log"
	"math"
	"strings"
	"time"

	"scheduler-extender/internal/models"
	"scheduler-extender/internal/services"
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

	//  Metrics health check
	health, err := s.metrics.GetHealth(ctx)
	if err != nil {
		log.Printf("[SCORER][WARN] metrics health failed service=%s error=%v", service, err)
		return
	}
	if health.Stale {
		log.Printf("[SCORER][WARN] metrics stale service=%s", service)
		//return
	}

	//  Fetch peers
	outPeers, err := s.metrics.GetPeers(ctx, service, "out", 5)
	if err != nil {
		log.Printf("[SCORER][WARN] outgoing peers failed service=%s error=%v", service, err)
		return
	}

	inPeers, err := s.metrics.GetPeers(ctx, service, "in", 5)
	if err != nil {
		log.Printf("[SCORER][WARN] incoming peers failed service=%s error=%v", service, err)
		return
	}

	peers := append(outPeers.Peers, inPeers.Peers...)
	if len(peers) == 0 {
		log.Printf("[SCORER][WARN] no peers found service=%s", service)
		return
	}

	for _, p := range peers {
		log.Printf(
			"[SCORER][DEBUG] peer service=%s rate=%.2f p95=%.2f errorRate=%.4f",
			p.Service,
			p.Metrics.Rate,
			p.Metrics.P95,
			p.Metrics.ErrorRate,
		)
	}

	// Placement index
	nodeServiceIndex, err := s.placement.BuildNodeServiceIndex(ctx)
	if err != nil {
		log.Printf("[SCORER][WARN] placement lookup failed service=%s error=%v", service, err)
		return
	}

	for node, svcs := range nodeServiceIndex {
		for svc := range svcs {
			log.Printf(
				"[SCORER][DEBUG] placement node=%s service=%s",
				node,
				svc,
			)
		}
	}

	// Centrality
	centrality, err := s.metrics.GetCentrality(ctx)
	if err != nil {
		log.Printf("[SCORER][WARN] centrality failed service=%s error=%v", service, err)
		return
	}

	pagerank, betweenness := findCentrality(centrality, service)
	importance := clamp01(0.5*pagerank + 0.5*betweenness)

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

		for _, p := range peers {
			peerName := strings.ToLower(p.Service)

			if hasService(nodeServiceIndex, node, peerName) {
				w := peerWeight(p)
				raw += w

				log.Printf(
					"[SCORER][DEBUG] MATCH node=%s peer=%s weight=%.4f",
					node,
					peerName,
					w,
				)
			} else {
				log.Printf(
					"[SCORER][DEBUG] NO_MATCH node=%s peer=%s",
					node,
					peerName,
				)
			}
		}

		log.Printf("[SCORER][DEBUG] raw before importance node=%s raw=%.6f", node, raw)

		raw *= (0.5 + 0.5*importance)

		log.Printf("[SCORER][DEBUG] raw after importance node=%s raw=%.6f", node, raw)

		rawScores[node] = raw

		if raw > maxRaw {
			maxRaw = raw
		}
	}

	if maxRaw <= 0.000001 {
		log.Printf("[SCORER][WARN] all raw scores zero service=%s — writing neutral decision", service)

		scores := make(map[string]int)
		for _, node := range nodes {
			scores[node] = 50
		}

		currentNode := ""
		for node, svcs := range nodeServiceIndex {
			if _, ok := svcs[strings.ToLower(service)]; ok {
				currentNode = node
				break
			}
		}

		decision := &models.Decision{
			Namespace:     namespace,
			Service:       service,
			CurrentNode:   currentNode,
			BestNode:      currentNode,
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

	for node, raw := range rawScores {
		score := int(math.Round(clamp((raw/maxRaw)*100.0, 0, 100)))
		scores[node] = score

		if score > bestScore {
			bestScore = score
			bestNode = node
		}
	}

	//  Determine current node (if already running)
	currentNode := ""
	for node, svcs := range nodeServiceIndex {
		if _, ok := svcs[strings.ToLower(service)]; ok {
			currentNode = node
			break
		}
	}

	decision := &models.Decision{
		Namespace:     namespace,
		Service:       service,
		Status:        "Scheduled",
		CurrentNode:   currentNode,
		BestNode:      bestNode,
		Scores:        scores,
		EvaluatedAt:   time.Now().UTC(),
		WindowSeconds: windowSeconds,
	}

	if err := s.repo.Save(ctx, decision); err != nil {
		log.Printf("[SCORER][ERROR] redis save failed service=%s error=%v", service, err)
		return
	}

	log.Printf(
		"[SCORER] compute done service=%s best=%s current=%s duration_ms=%d",
		service,
		bestNode,
		currentNode,
		time.Since(start).Milliseconds(),
	)
}

/* ================= helpers (unchanged math) ================= */

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

	w := (0.6*rate + 0.4*lat) * (1.0 - 0.7*err)
	return clamp(w, 0.05, 1.0)
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
