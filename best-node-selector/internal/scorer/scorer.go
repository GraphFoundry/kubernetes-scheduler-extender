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

const (
	defaultTopKPeers       = 3
	targetUtilization      = 0.65
	utilizationBand        = 0.35
	cpuHardCap             = 0.85
	memHardCap             = 0.85
	neutralNormFallback    = 0.5
	localityWeight         = 0.4
	spreadWeight           = 0.2
	utilBaseWeight         = 0.3
	utilCriticalitySlope   = 0.2
	riskBaseWeight         = 0.1
	riskCriticalitySlope   = 0.2
	concentrationWeight    = 0.25
	nodeDensityWeight      = 0.15
)

// CycleState tracks bestNode selections within a single scoring cycle
// to prevent all services from converging on the same node.
type CycleState struct {
	NodeSelections map[string]int // node -> number of times picked as bestNode this cycle
}

type Scorer struct {
	metrics    services.MetricsProvider
	placement  services.PlacementProvider
	repo       DecisionWriter
	topKPeers  int
	prefReader PreferenceReader
}

type DecisionWriter interface {
	Save(ctx context.Context, d *models.Decision) error
}

// PreferenceReader reads user node preferences from Redis
type PreferenceReader interface {
	GetNodePreference(ctx context.Context, namespace, service string) (string, error)
}

func New(
	metrics services.MetricsProvider,
	placement services.PlacementProvider,
	repo DecisionWriter,
	topKPeers int,
) *Scorer {
	if topKPeers <= 0 {
		topKPeers = defaultTopKPeers
	}
	return &Scorer{
		metrics:   metrics,
		placement: placement,
		repo:      repo,
		topKPeers: topKPeers,
	}
}

// SetPreferenceReader sets the preference reader for looking up user preferences
func (s *Scorer) SetPreferenceReader(pr PreferenceReader) {
	s.prefReader = pr
}

func (s *Scorer) ComputeForService(
	ctx context.Context,
	namespace string,
	service string,
	nodes []string,
	windowSeconds int,
	cycleState ...* CycleState,
) {
	var cs *CycleState
	if len(cycleState) > 0 && cycleState[0] != nil {
		cs = cycleState[0]
	}
	start := time.Now()
	log.Printf("[SCORER] scoring service=%s nodes=%d", service, len(nodes))

	defer func() {
		if r := recover(); r != nil {
			log.Printf("[SCORER][PANIC] service=%s: %v", service, r)
			s.writeNeutralDecision(ctx, namespace, service, nodes, windowSeconds)
		}
	}()

	// 1. Fetch graph data
	centrality, err := s.metrics.GetCentrality(ctx)
	degradedMode := false
	if err != nil {
		log.Printf("[SCORER][WARN] centrality fetch failed service=%s: %v", service, err)
		degradedMode = true
		centrality = services.CentralityResponse{}
	}
	servicesSnapshot, err := s.metrics.GetServices(ctx)
	if err != nil {
		log.Printf("[SCORER][WARN] services catalog fetch failed service=%s: %v", service, err)
		degradedMode = true
		servicesSnapshot = services.ServicesResponse{}
	}

	// 2. Get top peers from traffic
	outPeers, err := s.metrics.GetPeers(ctx, service, "out", 50)
	if err != nil {
		log.Printf("[SCORER][WARN] peers failed service=%s: %v", service, err)
		degradedMode = true
		outPeers = services.PeersResponse{}
	}
	inPeers, err := s.metrics.GetPeers(ctx, service, "in", 50)
	if err != nil {
		log.Printf("[SCORER][WARN] inbound peers failed service=%s: %v", service, err)
		degradedMode = true
		inPeers = services.PeersResponse{}
	}
	peerWeights := aggregatePeerTraffic(outPeers.Peers, inPeers.Peers)
	topPeers := topKPeerWeights(peerWeights, s.topKPeers)

	// 3. Get runtime placement data
	nodeRuntime, err := s.placement.GetNodeRuntime(ctx, []string{namespace})
	if err != nil {
		log.Printf("[SCORER][WARN] runtime fetch failed service=%s: %v", service, err)
		s.writeNeutralDecision(ctx, namespace, service, nodes, windowSeconds)
		return
	}

	// 3b. Get node labels for worker detection
	nodeLabels, _ := s.placement.GetNodeLabels(ctx)

	// 4. Filter out control-plane nodes only if they have NoSchedule/NoExecute taints
	workerNodes := filterSchedulableNodes(nodes, nodeLabels, nodeRuntime)
	targetNodes := nodes
	if len(workerNodes) > 0 {
		targetNodes = workerNodes
		log.Printf("[SCORER] filtering to schedulable nodes: %v", workerNodes)
	}

	criticalityByService, criticality := computeCriticality(service, centrality, servicesSnapshot)
	podReqCPU, podReqMem := estimateIncomingRequest(service, nodeRuntime)

	feasibleNodes := make([]string, 0, len(targetNodes))
	projectedCPU := map[string]float64{}
	projectedMem := map[string]float64{}
	for _, node := range targetNodes {
		rt, ok := nodeRuntime[node]
		if !ok || !rt.Ready || !rt.Schedulable || rt.AllocCPUMilli <= 0 || rt.AllocMemBytes <= 0 {
			continue
		}

		projCPU := float64(rt.UsedCPUMilli+podReqCPU) / float64(rt.AllocCPUMilli)
		projMem := float64(rt.UsedMemBytes+podReqMem) / float64(rt.AllocMemBytes)
		if projCPU > cpuHardCap || projMem > memHardCap {
			continue
		}
		projectedCPU[node] = projCPU
		projectedMem[node] = projMem
		feasibleNodes = append(feasibleNodes, node)
	}

	if len(feasibleNodes) == 0 {
		log.Printf("[SCORER][WARN] no feasible nodes for service=%s", service)
		s.writeNeutralDecision(ctx, namespace, service, nodes, windowSeconds)
		return
	}

	localityRaw := make(map[string]float64, len(feasibleNodes))
	utilScore := make(map[string]float64, len(feasibleNodes))
	spreadScore := make(map[string]float64, len(feasibleNodes))
	nodeCriticalMass := make(map[string]float64, len(feasibleNodes))

	for _, node := range feasibleNodes {
		rt := nodeRuntime[node]

		var lRaw float64
		for _, peer := range topPeers {
			peerPods := rt.PodsByService[peer.Service]
			lRaw += peer.Weight * math.Log1p(float64(peerPods))
		}
		localityRaw[node] = lRaw

		cpuScore := math.Max(0, 1-math.Abs(projectedCPU[node]-targetUtilization)/utilizationBand)
		memScore := math.Max(0, 1-math.Abs(projectedMem[node]-targetUtilization)/utilizationBand)
		utilScore[node] = (cpuScore + memScore) / 2.0

		podsSOnN := rt.PodsByService[strings.ToLower(service)]
		switch {
		case podsSOnN == 0:
			spreadScore[node] = 1.0
		case podsSOnN == 1:
			spreadScore[node] = 0.6
		default:
			spreadScore[node] = 0.2
		}

		for svc := range rt.Services {
			nodeCriticalMass[node] += criticalityByService[svc]
		}
	}

	// Compute node density score: penalise nodes running many services
	nodeDensityScore := make(map[string]float64, len(feasibleNodes))
	maxSvcCount := 0
	for _, node := range feasibleNodes {
		count := len(nodeRuntime[node].Services)
		if count > maxSvcCount {
			maxSvcCount = count
		}
	}
	for _, node := range feasibleNodes {
		if maxSvcCount > 0 {
			// Fewer services → higher score (more room)
			nodeDensityScore[node] = 1.0 - float64(len(nodeRuntime[node].Services))/float64(maxSvcCount+1)
		} else {
			nodeDensityScore[node] = 1.0
		}
	}

	// Compute concentration penalty from cycle state
	concentrationScore := make(map[string]float64, len(feasibleNodes))
	for _, node := range feasibleNodes {
		concentrationScore[node] = 1.0 // no penalty by default
	}
	if cs != nil && len(cs.NodeSelections) > 0 {
		maxSel := 0
		for _, count := range cs.NodeSelections {
			if count > maxSel {
				maxSel = count
			}
		}
		if maxSel > 0 {
			for _, node := range feasibleNodes {
				selCount := cs.NodeSelections[node]
				concentrationScore[node] = 1.0 - float64(selCount)/float64(maxSel+1)
			}
		}
	}

	localityScore := normalizeMapValues(localityRaw, feasibleNodes)
	nodeCriticalNorm := normalizeMapValues(nodeCriticalMass, feasibleNodes)

	utilWeight := utilBaseWeight - utilCriticalitySlope*criticality
	riskWeight := riskBaseWeight + riskCriticalitySlope*criticality

	rawScores := make(map[string]float64, len(feasibleNodes))
	for _, node := range feasibleNodes {
		riskPenalty := nodeCriticalNorm[node] * criticality
		rawScores[node] =
			localityWeight*localityScore[node] +
				utilWeight*utilScore[node] +
				spreadWeight*spreadScore[node] +
				nodeDensityWeight*nodeDensityScore[node] +
				concentrationWeight*concentrationScore[node] -
				riskWeight*riskPenalty
	}

	normalized := normalizeMapValues(rawScores, feasibleNodes)
	scores := make(map[string]int, len(nodes))
	for _, node := range nodes {
		scores[node] = 0
	}
	for _, node := range feasibleNodes {
		scores[node] = int(math.Round(normalized[node] * 100))
	}

	// 7. Find best node (deterministic tie-break, only from feasible nodes)
	bestNode := ""
	bestScore := -1
	for _, node := range feasibleNodes {
		if scores[node] > bestScore || (scores[node] == bestScore && node < bestNode) {
			bestScore = scores[node]
			bestNode = node
		}
	}

	currentNodes := getCurrentNodesFromRuntime(nodeRuntime, service)

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

	// Apply user preference: if a preference exists and the node is feasible, override BestNode
	if s.prefReader != nil {
		prefNode, prefErr := s.prefReader.GetNodePreference(ctx, namespace, service)
		if prefErr == nil && prefNode != "" {
			decision.PreferredNode = prefNode
			if _, ok := scores[prefNode]; ok && scores[prefNode] > 0 {
				decision.BestNode = prefNode
				log.Printf("[SCORER] user preference applied: bestNode overridden to %s for service=%s", prefNode, service)
			} else {
				log.Printf("[SCORER] user preference node=%s not feasible for service=%s, keeping bestNode=%s", prefNode, service, bestNode)
			}
		}
	}

	if err := s.repo.Save(ctx, decision); err != nil {
		log.Printf("[SCORER][ERROR] save failed service=%s: %v", service, err)
		return
	}

	// Record this selection in cycle state so subsequent services avoid piling on the same node
	if cs != nil {
		cs.NodeSelections[decision.BestNode]++
	}

	log.Printf("[SCORER] done service=%s best=%s peers=%d duration=%dms",
		service, bestNode, len(topPeers), time.Since(start).Milliseconds())
	if degradedMode {
		log.Printf("[SCORER][WARN] degraded_mode=true service=%s", service)
	}
}

/* ================= helpers (unchanged math) ================= */

type peerWeight struct {
	Service string
	Weight  float64
}

func aggregatePeerTraffic(out []services.Peer, in []services.Peer) map[string]float64 {
	agg := map[string]float64{}
	for _, p := range out {
		agg[strings.ToLower(p.Service)] += p.Metrics.Rate
	}
	for _, p := range in {
		agg[strings.ToLower(p.Service)] += p.Metrics.Rate
	}
	return agg
}

func topKPeerWeights(agg map[string]float64, k int) []peerWeight {
	if k <= 0 || len(agg) == 0 {
		return nil
	}

	vals := make([]float64, 0, len(agg))
	for _, v := range agg {
		vals = append(vals, v)
	}
	p05, p95 := percentileBand(vals, 0.05, 0.95)

	peers := make([]peerWeight, 0, len(agg))
	for svc, v := range agg {
		peers = append(peers, peerWeight{Service: svc, Weight: norm(v, p05, p95)})
	}

	sort.Slice(peers, func(i, j int) bool {
		if peers[i].Weight == peers[j].Weight {
			return peers[i].Service < peers[j].Service
		}
		return peers[i].Weight > peers[j].Weight
	})

	if len(peers) > k {
		peers = peers[:k]
	}
	return peers
}

func computeCriticality(service string, c services.CentralityResponse, svcSnapshot services.ServicesResponse) (map[string]float64, float64) {
	prVals := make([]float64, 0, len(c.Scores))
	bcVals := make([]float64, 0, len(c.Scores))
	availabilityByService := map[string]float64{}
	availVals := make([]float64, 0, len(svcSnapshot.Services))
	for _, svc := range svcSnapshot.Services {
		key := strings.ToLower(svc.Name)
		avail := 0.0
		if svc.Availability > 0 {
			avail = 1.0
		}
		availabilityByService[key] = avail
		availVals = append(availVals, avail)
	}

	for _, score := range c.Scores {
		prVals = append(prVals, score.Pagerank)
		bcVals = append(bcVals, score.Betweenness)
		if score.Availability != nil {
			availabilityByService[strings.ToLower(score.Service)] = *score.Availability
			availVals = append(availVals, *score.Availability)
		}
	}
	pr05, pr95 := percentileBand(prVals, 0.05, 0.95)
	bc05, bc95 := percentileBand(bcVals, 0.05, 0.95)
	avail05, avail95 := percentileBand(availVals, 0.05, 0.95)

	byService := make(map[string]float64, len(c.Scores))
	for _, score := range c.Scores {
		prN := norm(score.Pagerank, pr05, pr95)
		bcN := norm(score.Betweenness, bc05, bc95)
		availN := neutralNormFallback
		if avail, ok := availabilityByService[strings.ToLower(score.Service)]; ok {
			availN = norm(avail, avail05, avail95)
		}
		crit := 0.4*prN + 0.4*bcN + 0.2*(1-availN)
		byService[strings.ToLower(score.Service)] = crit
	}

	target := byService[strings.ToLower(service)]
	if target == 0 {
		target = neutralNormFallback
	}
	return byService, target
}

func estimateIncomingRequest(service string, runtime map[string]services.NodeRuntime) (int64, int64) {
	svcKey := strings.ToLower(service)
	var cpuSamples []int64
	var memSamples []int64
	for _, node := range runtime {
		for _, req := range node.ServiceResources[svcKey] {
			cpuSamples = append(cpuSamples, req.CPUMilli)
			memSamples = append(memSamples, req.MemBytes)
		}
	}
	if len(cpuSamples) == 0 || len(memSamples) == 0 {
		return 100, 128 * 1024 * 1024
	}
	return medianInt64(cpuSamples), medianInt64(memSamples)
}

func getCurrentNodesFromRuntime(runtime map[string]services.NodeRuntime, service string) []string {
	var nodes []string
	svc := strings.ToLower(service)
	for node, st := range runtime {
		if st.PodsByService[svc] > 0 {
			nodes = append(nodes, node)
		}
	}
	sort.Strings(nodes)
	return nodes
}

func normalizeMapValues(input map[string]float64, nodes []string) map[string]float64 {
	vals := make([]float64, 0, len(nodes))
	for _, node := range nodes {
		vals = append(vals, input[node])
	}
	p05, p95 := percentileBand(vals, 0.05, 0.95)
	out := make(map[string]float64, len(nodes))
	for _, node := range nodes {
		out[node] = norm(input[node], p05, p95)
	}
	return out
}

func percentileBand(vals []float64, low, high float64) (float64, float64) {
	if len(vals) == 0 {
		return 0, 1
	}
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	return percentile(sorted, low), percentile(sorted, high)
}

func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[len(sorted)-1]
	}
	pos := q * float64(len(sorted)-1)
	low := int(math.Floor(pos))
	high := int(math.Ceil(pos))
	if low == high {
		return sorted[low]
	}
	frac := pos - float64(low)
	return sorted[low] + frac*(sorted[high]-sorted[low])
}

func norm(x, p05, p95 float64) float64 {
	if p95 == p05 {
		return neutralNormFallback
	}
	v := (x - p05) / (p95 - p05)
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func medianInt64(vals []int64) int64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]int64(nil), vals...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

// filterSchedulableNodes returns nodes eligible for scheduling.
// Control-plane nodes are included if they have no NoSchedule/NoExecute taints.
func filterSchedulableNodes(nodes []string, nodeLabels map[string]map[string]string, nodeRuntime map[string]services.NodeRuntime) []string {
	var schedulable []string
	for _, node := range nodes {
		if isControlPlaneNode(node, nodeLabels) && hasBlockingTaints(node, nodeRuntime) {
			continue
		}
		schedulable = append(schedulable, node)
	}
	return schedulable
}

// isControlPlaneNode returns true if the node is a primary/master/control-plane node
func isControlPlaneNode(nodeName string, nodeLabels map[string]map[string]string) bool {
	if nodeLabels != nil {
		if labels, ok := nodeLabels[nodeName]; ok {
			if primary, ok := labels["minikube.k8s.io/primary"]; ok {
				return primary == "true"
			}
			if _, ok := labels["node-role.kubernetes.io/control-plane"]; ok {
				return true
			}
			if _, ok := labels["node-role.kubernetes.io/master"]; ok {
				return true
			}
		}
	}
	nodeLower := strings.ToLower(nodeName)
	return strings.Contains(nodeLower, "primary") ||
		strings.Contains(nodeLower, "master") ||
		strings.Contains(nodeLower, "control-plane") ||
		strings.HasSuffix(nodeLower, "-cp") ||
		strings.Contains(nodeLower, "-cp-") ||
		matchControlPlanePattern(nodeLower)
}

// hasBlockingTaints returns true if the node has NoSchedule or NoExecute taints
func hasBlockingTaints(nodeName string, nodeRuntime map[string]services.NodeRuntime) bool {
	rt, ok := nodeRuntime[nodeName]
	if !ok {
		return false
	}
	for _, t := range rt.Taints {
		if t.Effect == "NoSchedule" || t.Effect == "NoExecute" {
			return true
		}
	}
	return false
}

// isWorkerNode returns true if the node is not a control-plane node (used for scoring bonus)
func isWorkerNode(nodeName string, nodeLabels map[string]map[string]string) bool {
	return !isControlPlaneNode(nodeName, nodeLabels)
}

// matchControlPlanePattern checks for patterns like k8s-cp1, node-cp2 etc.
func matchControlPlanePattern(nodeLower string) bool {
	// Match patterns like "cp1", "cp2" at end of name segments
	parts := strings.Split(nodeLower, "-")
	for _, part := range parts {
		if len(part) >= 2 && strings.HasPrefix(part, "cp") {
			isDigitSuffix := true
			for _, ch := range part[2:] {
				if ch < '0' || ch > '9' {
					isDigitSuffix = false
					break
				}
			}
			if isDigitSuffix {
				return true
			}
		}
	}
	return false
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
	nodeRuntime, _ := s.placement.GetNodeRuntime(ctx, []string{namespace})
	currentNodes := getCurrentNodesFromRuntime(nodeRuntime, service)

	// Filter to schedulable nodes (include untainted CP nodes)
	workerNodes := filterSchedulableNodes(nodes, nodeLabels, nodeRuntime)
	targetNodes := nodes
	if len(workerNodes) > 0 {
		targetNodes = workerNodes
	}

	scores := make(map[string]int)
	// Worker nodes get score 50, control-plane nodes get 30 (lower preference but still viable)
	for _, node := range nodes {
		if isWorkerNode(node, nodeLabels) {
			scores[node] = 50
		} else {
			scores[node] = 30
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
