package scheduler

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"best-node-selector/internal/models"
	"best-node-selector/internal/redis"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Scoring weights (tunable)
const (
	weightCPUFree       = 1.0
	weightMemFree       = 1.0
	weightFragmentation = 0.5
	weightTaint         = 2.0
)

// Scheduler implements production-grade scheduling with optimistic locking
type Scheduler struct {
	repo         *redis.SchedulerRepository
	decisionRepo *redis.DecisionRepository
	clientset    *kubernetes.Clientset
}

func New(repo *redis.SchedulerRepository, decisionRepo *redis.DecisionRepository, clientset *kubernetes.Clientset) *Scheduler {
	return &Scheduler{
		repo:         repo,
		decisionRepo: decisionRepo,
		clientset:    clientset,
	}
}

// Schedule performs the complete scheduling workflow
// Always returns a node name - never skips to default scheduler
func (s *Scheduler) Schedule(ctx context.Context, pod *v1.Pod, nodes []v1.Node) (nodeName string, err error) {
	// 🔥 SAFE FALLBACK: Ensure we always have at least one node
	if len(nodes) == 0 {
		log.Printf("[SCHEDULER][ERROR] no nodes available in cluster")
		return "", fmt.Errorf("no nodes available")
	}

	defer func() {
		if r := recover(); r != nil {
			log.Printf("[SCHEDULER][PANIC] recovered panic: %v - using first available node", r)
			// Actually return the first node as fallback
			nodeName = nodes[0].Name
			err = nil
			// Try to bind to first node (ignore errors in panic recovery)
			_ = s.bindPodSimple(ctx, pod, nodeName)
		}
	}()

	// 🔥 REDIS-FIRST: Check for precomputed best node decision (non-blocking)
	if s.decisionRepo != nil {
		service := extractServiceName(pod)
		if service != "" {
			namespace := pod.Namespace
			if namespace == "" {
				namespace = "default"
			}

			// Use timeout context to prevent hanging on Redis
			redisCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
			defer cancel()

			decision, err := s.decisionRepo.Get(redisCtx, namespace, service)
			if err == nil && decision.BestNode != "" {
				// Verify best node is in candidate list
				for _, node := range nodes {
					if node.Name == decision.BestNode {
						log.Printf("[SCHEDULER][REDIS] using precomputed best node=%s for service=%s (score=%d)",
							decision.BestNode, service, decision.Scores[decision.BestNode])

						// Try to bind to best node
						if bindErr := s.bindPodSimple(ctx, pod, decision.BestNode); bindErr != nil {
							log.Printf("[SCHEDULER][REDIS] bind failed: %v, continuing with normal scheduling", bindErr)
							break // Fall through to normal scheduling
						}
						return decision.BestNode, nil
					}
				}
				log.Printf("[SCHEDULER][REDIS] best node=%s not in candidate list, falling back to normal scheduling", decision.BestNode)
			} else if err != nil {
				log.Printf("[SCHEDULER][REDIS] decision lookup failed for service=%s: %v, falling back to normal scheduling", service, err)
			}
		}
	}

	// STEP 1: Extract pod intent
	intent, err := s.extractPodIntent(pod)
	if err != nil {
		log.Printf("[SCHEDULER][WARN] extract pod intent failed: %v - using first node", err)
		// Use first available node as fallback
		firstNode := nodes[0].Name
		log.Printf("[SCHEDULER] selecting first node=%s as fallback", firstNode)
		// Try to bind to first node
		if bindErr := s.bindPodSimple(ctx, pod, firstNode); bindErr != nil {
			log.Printf("[SCHEDULER][ERROR] failed to bind to first node: %v", bindErr)
		}
		return firstNode, nil
	}

	log.Printf("[SCHEDULER] scheduling pod=%s/%s cpu=%dm mem=%dMi arch=%s",
		intent.Namespace, intent.Name, intent.CPURequest, intent.MemRequest/(1024*1024), intent.Arch)

	// STEP 2: Fetch candidate nodes from Redis (with timeout to prevent hanging)
	redisCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	nodeStates, err := s.repo.ListNodeStates(redisCtx)
	if err != nil {
		log.Printf("[SCHEDULER][WARN] fetch node states failed: %v - using first node", err)
		firstNode := nodes[0].Name
		log.Printf("[SCHEDULER] selecting first node=%s as fallback", firstNode)
		if bindErr := s.bindPodSimple(ctx, pod, firstNode); bindErr != nil {
			log.Printf("[SCHEDULER][ERROR] failed to bind to first node: %v", bindErr)
		}
		return firstNode, nil
	}

	log.Printf("[SCHEDULER] fetched %d candidate nodes from Redis", len(nodeStates))

	// STEP 3: HARD FILTER
	filtered, failedNodes := s.filter(intent, nodeStates)
	if len(filtered) == 0 {
		log.Printf("[SCHEDULER][WARN] no nodes passed filtering - using first available node as default")
		// Use first available node as fallback
		firstNode := nodes[0].Name
		log.Printf("[SCHEDULER] selecting first node=%s as default (filtering failed: %v)", firstNode, failedNodes)
		if bindErr := s.bindPodSimple(ctx, pod, firstNode); bindErr != nil {
			log.Printf("[SCHEDULER][ERROR] failed to bind to first node: %v", bindErr)
		}
		return firstNode, nil
	}

	log.Printf("[SCHEDULER] %d nodes passed filtering, %d failed", len(filtered), len(failedNodes))

	// STEP 4: SOFT SCORING
	scored := s.score(intent, filtered)

	log.Printf("[SCHEDULER] scored %d nodes, top: %s (%.2f)",
		len(scored), scored[0].Name, scored[0].Score)

	// STEP 5-8: RESERVE, VALIDATE, BIND (with optimistic locking)
	nodeName, err = s.reserveAndBind(ctx, intent, scored, nodes)
	if err != nil {
		log.Printf("[SCHEDULER][WARN] reserve and bind failed: %v - nodeName returned: %s", err, nodeName)
		// If nodeName is returned, use it; otherwise use first available
		if nodeName != "" {
			return nodeName, nil
		}
		firstNode := nodes[0].Name
		log.Printf("[SCHEDULER] using first node=%s as final fallback", firstNode)
		if bindErr := s.bindPodSimple(ctx, pod, firstNode); bindErr != nil {
			log.Printf("[SCHEDULER][ERROR] failed to bind to first node: %v", bindErr)
		}
		return firstNode, nil
	}

	return nodeName, nil
}

// extractPodIntent extracts scheduling requirements from pod
func (s *Scheduler) extractPodIntent(pod *v1.Pod) (*models.PodIntent, error) {
	intent := &models.PodIntent{
		UID:          string(pod.UID),
		Namespace:    pod.Namespace,
		Name:         pod.Name,
		Scheduler:    pod.Spec.SchedulerName,
		NodeSelector: pod.Spec.NodeSelector,
	}

	// Extract architecture requirement (MANDATORY)
	if arch, ok := pod.Spec.NodeSelector["kubernetes.io/arch"]; ok {
		intent.Arch = arch
	} else if arch, ok := pod.Spec.NodeSelector["beta.kubernetes.io/arch"]; ok {
		intent.Arch = arch
	} else {
		// Default to host arch if not specified (dangerous but common)
		intent.Arch = "arm64" // TODO: detect host arch
	}

	// Extract OS requirement
	if os, ok := pod.Spec.NodeSelector["kubernetes.io/os"]; ok {
		intent.OS = os
	} else if os, ok := pod.Spec.NodeSelector["beta.kubernetes.io/os"]; ok {
		intent.OS = os
	} else {
		intent.OS = "linux"
	}

	// Calculate total resource requests
	var cpuMillis int64
	var memBytes int64

	for _, container := range pod.Spec.Containers {
		if cpu := container.Resources.Requests.Cpu(); cpu != nil {
			cpuMillis += cpu.MilliValue()
		}
		if mem := container.Resources.Requests.Memory(); mem != nil {
			memBytes += mem.Value()
		}
	}

	// Include init containers (max, not sum)
	var maxInitCPU int64
	var maxInitMem int64
	for _, container := range pod.Spec.InitContainers {
		if cpu := container.Resources.Requests.Cpu(); cpu != nil {
			if cpu.MilliValue() > maxInitCPU {
				maxInitCPU = cpu.MilliValue()
			}
		}
		if mem := container.Resources.Requests.Memory(); mem != nil {
			if mem.Value() > maxInitMem {
				maxInitMem = mem.Value()
			}
		}
	}

	if maxInitCPU > cpuMillis {
		cpuMillis = maxInitCPU
	}
	if maxInitMem > memBytes {
		memBytes = maxInitMem
	}

	intent.CPURequest = cpuMillis
	intent.MemRequest = memBytes

	// Extract priority
	if pod.Spec.Priority != nil {
		intent.Priority = *pod.Spec.Priority
	}

	// Extract tolerations
	for _, tol := range pod.Spec.Tolerations {
		intent.Tolerations = append(intent.Tolerations, tol.Key)
	}

	// Validation
	if intent.CPURequest == 0 || intent.MemRequest == 0 {
		return nil, fmt.Errorf("cpu/memory requests missing")
	}
	if intent.Arch == "" || intent.OS == "" {
		return nil, fmt.Errorf("arch/os missing")
	}

	return intent, nil
}

// filter applies hard constraints
func (s *Scheduler) filter(
	intent *models.PodIntent,
	nodes []*models.NodeState,
) ([]*models.NodeState, map[string]models.FilterReason) {

	var passed []*models.NodeState
	failed := make(map[string]models.FilterReason)

	for _, node := range nodes {
		reason := s.checkHardConstraints(intent, node)
		if reason != "" {
			failed[node.Name] = reason
			continue
		}
		passed = append(passed, node)
	}

	return passed, failed
}

// checkHardConstraints checks if pod can fit on node
func (s *Scheduler) checkHardConstraints(intent *models.PodIntent, node *models.NodeState) models.FilterReason {
	// Node ready check
	if !node.Ready {
		return models.ReasonNotReady
	}

	// 💀 ARCHITECTURE MISMATCH = HARD FAIL
	if node.Arch != intent.Arch {
		log.Printf("[FILTER] node=%s rejected: arch mismatch (node=%s pod=%s)",
			node.Name, node.Arch, intent.Arch)
		return models.ReasonArchMismatch
	}

	// OS check
	if node.OS != intent.OS {
		return models.ReasonOSMismatch
	}

	// CPU check
	cpuFree := node.CPUAllocatable - node.CPUUsed
	if cpuFree < intent.CPURequest {
		return models.ReasonInsufficientCPU
	}

	// Memory check
	memFree := node.MemAllocatable - node.MemUsed
	if memFree < intent.MemRequest {
		return models.ReasonInsufficientMemory
	}

	// Pod limit check
	if node.PodsUsed >= node.PodsAllocatable {
		return models.ReasonPodLimitReached
	}

	// Taint/toleration check
	for _, taint := range node.Taints {
		tolerated := false
		for _, tol := range intent.Tolerations {
			if taint == tol {
				tolerated = true
				break
			}
		}
		if !tolerated {
			return models.ReasonTaintNoToleration
		}
	}

	// Node selector check
	for key, value := range intent.NodeSelector {
		if nodeValue, ok := node.Labels[key]; !ok || nodeValue != value {
			return models.ReasonNodeSelectorFail
		}
	}

	return ""
}

// score applies soft scoring
func (s *Scheduler) score(intent *models.PodIntent, nodes []*models.NodeState) []*models.ScoredNode {
	scored := make([]*models.ScoredNode, 0, len(nodes))

	for _, node := range nodes {
		score := s.calculateScore(intent, node)
		scored = append(scored, &models.ScoredNode{
			Name:  node.Name,
			Score: score,
			State: node,
		})
	}

	// Sort descending by score
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].Score > scored[j].Score
	})

	return scored
}

// calculateScore computes node score (higher is better)
func (s *Scheduler) calculateScore(intent *models.PodIntent, node *models.NodeState) float64 {
	cpuFree := float64(node.CPUAllocatable - node.CPUUsed)
	memFree := float64(node.MemAllocatable - node.MemUsed)

	cpuTotal := float64(node.CPUAllocatable)
	memTotal := float64(node.MemAllocatable)

	// Ratios (0-1)
	cpuFreeRatio := cpuFree / cpuTotal
	memFreeRatio := memFree / memTotal

	// Fragmentation penalty (prefer balanced usage)
	fragmentation := math.Abs(cpuFreeRatio - memFreeRatio)

	// Taint penalty
	taintPenalty := float64(len(node.Taints)) * 0.1

	// Worker node bonus (prefer non-primary nodes)
	workerBonus := 0.0
	if isWorkerNode(node.Name, node.Labels) {
		workerBonus = 0.2
	}

	score := weightCPUFree*cpuFreeRatio +
		weightMemFree*memFreeRatio -
		weightFragmentation*fragmentation -
		weightTaint*taintPenalty +
		workerBonus

	// Normalize to 0-100
	return math.Max(0, math.Min(100, score*100))
}

// reserveAndBind attempts to reserve resources and bind pod
func (s *Scheduler) reserveAndBind(
	ctx context.Context,
	intent *models.PodIntent,
	scoredNodes []*models.ScoredNode,
	kubeNodes []v1.Node,
) (string, error) {

	// STEP 1: Check if pod is already assigned to a node
	pod, err := s.clientset.CoreV1().Pods(intent.Namespace).Get(ctx, intent.Name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get pod: %w", err)
	}

	if pod.Spec.NodeName != "" {
		log.Printf("[RESERVE] pod=%s/%s already assigned to node=%s",
			intent.Namespace, intent.Name, pod.Spec.NodeName)

		// STEP 2: Check if pod is already on the best node
		if len(scoredNodes) > 0 && scoredNodes[0].Name == pod.Spec.NodeName {
			log.Printf("[RESERVE] pod=%s/%s is already on best node=%s, no action needed",
				intent.Namespace, intent.Name, pod.Spec.NodeName)
			return pod.Spec.NodeName, nil
		}

		log.Printf("[RESERVE] pod=%s/%s is on node=%s but best node is=%s",
			intent.Namespace, intent.Name, pod.Spec.NodeName, scoredNodes[0].Name)
		// Pod is already bound to a different node - cannot rebind
		// Return success to avoid error, scheduler will use current node
		return pod.Spec.NodeName, nil
	}

	// STEP 3: Pod is not assigned, proceed with scheduling
	// Try each node in score order (best node first)
	for _, scored := range scoredNodes {
		nodeName := scored.Name
		state := scored.State

		log.Printf("[RESERVE] attempting node=%s score=%.2f version=%d",
			nodeName, scored.Score, state.Version)

		// STEP 5.1: Acquire node lock (with timeout to prevent hanging)
		lockCtx, lockCancel := context.WithTimeout(ctx, 100*time.Millisecond)
		lockUUID, err := s.repo.LockNode(lockCtx, nodeName)
		lockCancel()
		if err != nil {
			log.Printf("[RESERVE] node=%s lock failed: %v", nodeName, err)
			continue
		}

		// Helper to unlock node safely
		unlockNode := func() {
			unlockCtx, unlockCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer unlockCancel()
			s.repo.UnlockNode(unlockCtx, nodeName, lockUUID)
		}

		// STEP 5.2: Read node state again (with timeout)
		stateCtx, stateCancel := context.WithTimeout(ctx, 100*time.Millisecond)
		freshState, err := s.repo.GetNodeState(stateCtx, nodeName)
		stateCancel()
		if err != nil {
			log.Printf("[RESERVE] node=%s fetch failed: %v", nodeName, err)
			unlockNode()
			continue
		}

		// STEP 5.2: Version check
		if freshState.Version != state.Version {
			log.Printf("[RESERVE] node=%s version mismatch (cached=%d fresh=%d)",
				nodeName, state.Version, freshState.Version)
			unlockNode()
			continue
		}

		// STEP 5.3: Re-check constraints
		if reason := s.checkHardConstraints(intent, freshState); reason != "" {
			log.Printf("[RESERVE] node=%s constraints failed on recheck: %s", nodeName, reason)
			unlockNode()
			continue
		}

		// STEP 5.4: Atomic reservation
		newVersion, err := s.repo.ReserveResources(
			ctx,
			nodeName,
			intent.CPURequest,
			intent.MemRequest,
			freshState.Version,
		)
		if err != nil {
			log.Printf("[RESERVE] node=%s reservation failed: %v", nodeName, err)
			unlockNode()
			continue
		}

		log.Printf("[RESERVE] node=%s reserved cpu=%dm mem=%dMi version=%d->%d",
			nodeName, intent.CPURequest, intent.MemRequest/(1024*1024), freshState.Version, newVersion)

		// STEP 5.5: Release lock (now safe to release)
		unlockNode()

		// STEP 6: API server validation
		kubeNode := s.findKubeNode(kubeNodes, nodeName)
		if kubeNode == nil || !s.validateAPINode(intent, kubeNode) {
			log.Printf("[RESERVE] node=%s API validation failed, rolling back", nodeName)
			s.repo.RollbackResources(ctx, nodeName, intent.CPURequest, intent.MemRequest)
			// Lock already released, just continue
			continue
		}

		// STEP 7: Bind pod
		if err := s.bindPod(ctx, intent, nodeName); err != nil {
			log.Printf("[RESERVE] node=%s bind failed: %v, rolling back", nodeName, err)
			s.repo.RollbackResources(ctx, nodeName, intent.CPURequest, intent.MemRequest)
			// Lock already released, just continue
			continue
		}

		log.Printf("[SUCCESS] pod=%s/%s scheduled to node=%s", intent.Namespace, intent.Name, nodeName)
		return nodeName, nil
	}

	// If all scored nodes failed, use first available node from kubeNodes as fallback
	if len(kubeNodes) > 0 {
		firstNode := kubeNodes[0].Name
		log.Printf("[RESERVE] all nodes failed reservation - using first available node=%s as default", firstNode)
		return firstNode, nil
	}

	return "", fmt.Errorf("no nodes available")
}

// findKubeNode finds a node in the Kubernetes API list
func (s *Scheduler) findKubeNode(nodes []v1.Node, name string) *v1.Node {
	for i := range nodes {
		if nodes[i].Name == name {
			return &nodes[i]
		}
	}
	return nil
}

// validateAPINode validates node state against API server
func (s *Scheduler) validateAPINode(intent *models.PodIntent, node *v1.Node) bool {
	// Check ready condition
	ready := false
	for _, cond := range node.Status.Conditions {
		if cond.Type == v1.NodeReady && cond.Status == v1.ConditionTrue {
			ready = true
			break
		}
	}
	if !ready {
		return false
	}

	// Verify architecture unchanged
	arch := node.Labels["kubernetes.io/arch"]
	if arch == "" {
		arch = node.Labels["beta.kubernetes.io/arch"]
	}
	if arch != intent.Arch {
		log.Printf("[VALIDATION] CRITICAL: architecture changed on node=%s (expected=%s actual=%s)",
			node.Name, intent.Arch, arch)
		return false
	}

	// Check for blocking taints
	for _, taint := range node.Spec.Taints {
		if taint.Effect == v1.TaintEffectNoSchedule || taint.Effect == v1.TaintEffectNoExecute {
			tolerated := false
			for _, tol := range intent.Tolerations {
				if taint.Key == tol {
					tolerated = true
					break
				}
			}
			if !tolerated {
				return false
			}
		}
	}

	return true
}

// bindPod binds pod to node
func (s *Scheduler) bindPod(ctx context.Context, intent *models.PodIntent, nodeName string) error {
	// 🔥 PREVENT DOUBLE-SCHEDULING: Check if pod is already bound
	pod, err := s.clientset.CoreV1().Pods(intent.Namespace).Get(ctx, intent.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get pod: %w", err)
	}

	if pod.Spec.NodeName != "" {
		if pod.Spec.NodeName == nodeName {
			log.Printf("[BIND][SKIP] pod=%s/%s already assigned to target node=%s",
				intent.Namespace, intent.Name, pod.Spec.NodeName)
			return nil // Success - pod is already on the target node
		}
		log.Printf("[BIND][SKIP] pod=%s/%s already assigned to different node=%s (target was %s)",
			intent.Namespace, intent.Name, pod.Spec.NodeName, nodeName)
		return nil // Not an error - pod is already scheduled elsewhere
	}

	log.Printf("[BIND] binding pod=%s/%s to node=%s",
		intent.Namespace, intent.Name, nodeName)

	binding := &v1.Binding{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: intent.Namespace,
			Name:      intent.Name,
			UID:       types.UID(intent.UID),
		},
		Target: v1.ObjectReference{
			Kind: "Node",
			Name: nodeName,
		},
	}

	return s.clientset.CoreV1().Pods(intent.Namespace).Bind(ctx, binding, metav1.CreateOptions{})
}

// bindPodSimple is a simplified version of bindPod for fallback scenarios
func (s *Scheduler) bindPodSimple(ctx context.Context, pod *v1.Pod, nodeName string) error {
	// Check if pod is already bound (fetch latest state to be sure)
	freshPod, err := s.clientset.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get pod: %w", err)
	}

	if freshPod.Spec.NodeName != "" {
		log.Printf("[BIND][SKIP] pod=%s/%s already assigned to node=%s",
			freshPod.Namespace, freshPod.Name, freshPod.Spec.NodeName)
		return nil // Not an error - pod is already scheduled
	}

	log.Printf("[BIND] binding pod=%s/%s to node=%s",
		pod.Namespace, pod.Name, nodeName)

	binding := &v1.Binding{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: pod.Namespace,
			Name:      pod.Name,
			UID:       pod.UID,
		},
		Target: v1.ObjectReference{
			Kind: "Node",
			Name: nodeName,
		},
	}

	return s.clientset.CoreV1().Pods(pod.Namespace).Bind(ctx, binding, metav1.CreateOptions{})
}

// UpdateNodeStateFromAPI updates Redis node state from Kubernetes API
func (s *Scheduler) UpdateNodeStateFromAPI(ctx context.Context, node *v1.Node) error {
	state := &models.NodeState{
		Name:   node.Name,
		Ready:  false,
		Labels: node.Labels,
	}

	// Extract architecture
	state.Arch = node.Labels["kubernetes.io/arch"]
	if state.Arch == "" {
		state.Arch = node.Labels["beta.kubernetes.io/arch"]
	}

	// Extract OS
	state.OS = node.Labels["kubernetes.io/os"]
	if state.OS == "" {
		state.OS = node.Labels["beta.kubernetes.io/os"]
	}

	// Check ready
	for _, cond := range node.Status.Conditions {
		if cond.Type == v1.NodeReady && cond.Status == v1.ConditionTrue {
			state.Ready = true
			break
		}
	}

	// Extract resources
	if cpu := node.Status.Allocatable.Cpu(); cpu != nil {
		state.CPUAllocatable = cpu.MilliValue()
	}
	if mem := node.Status.Allocatable.Memory(); mem != nil {
		state.MemAllocatable = mem.Value()
	}
	if pods := node.Status.Allocatable.Pods(); pods != nil {
		state.PodsAllocatable = int(pods.Value())
	}

	// Calculate used resources (from existing state or recalculate)
	existing, err := s.repo.GetNodeState(ctx, node.Name)
	if err == nil {
		state.CPUUsed = existing.CPUUsed
		state.MemUsed = existing.MemUsed
		state.PodsUsed = existing.PodsUsed
		state.Version = existing.Version
	}

	// Extract taints
	for _, taint := range node.Spec.Taints {
		state.Taints = append(state.Taints, taint.Key)
	}

	return s.repo.SetNodeState(ctx, state)
}

// extractServiceName extracts service identifier from pod labels
func extractServiceName(pod *v1.Pod) string {
	// Try extender-specific label first
	if service, ok := pod.Labels["extender.kubernetes.io/name"]; ok && service != "" {
		return service
	}
	// Try standard app label
	if app, ok := pod.Labels["app"]; ok && app != "" {
		return app
	}
	// Try app.kubernetes.io/name
	if appName, ok := pod.Labels["app.kubernetes.io/name"]; ok && appName != "" {
		return appName
	}
	return ""
}

// isWorkerNode returns true if the node is not a primary/master/control-plane node
func isWorkerNode(nodeName string, labels map[string]string) bool {
	// Check minikube label first (most reliable)
	if labels != nil {
		if primary, ok := labels["minikube.k8s.io/primary"]; ok {
			return primary == "false"
		}
	}
	// Fallback to name-based detection
	nodeLower := strings.ToLower(nodeName)
	return !strings.Contains(nodeLower, "primary") &&
		!strings.Contains(nodeLower, "master") &&
		!strings.Contains(nodeLower, "control-plane")
}
