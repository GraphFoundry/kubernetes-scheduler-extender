package scheduler

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"

	"scheduler-extender/internal/models"
	"scheduler-extender/internal/redis"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Scoring weights (tunable)
const (
	weightCPUFree         = 1.0
	weightMemFree         = 1.0
	weightFragmentation   = 0.5
	weightTaint           = 2.0
)

// Scheduler implements production-grade scheduling with optimistic locking
type Scheduler struct {
	repo      *redis.SchedulerRepository
	clientset *kubernetes.Clientset
}

func New(repo *redis.SchedulerRepository, clientset *kubernetes.Clientset) *Scheduler {
	return &Scheduler{
		repo:      repo,
		clientset: clientset,
	}
}

// Schedule performs the complete scheduling workflow
func (s *Scheduler) Schedule(ctx context.Context, pod *v1.Pod, nodes []v1.Node) (string, error) {
	// STEP 1: Extract pod intent
	intent, err := s.extractPodIntent(pod)
	if err != nil {
		return "", fmt.Errorf("extract pod intent: %w", err)
	}

	log.Printf("[SCHEDULER] scheduling pod=%s/%s cpu=%dm mem=%dMi arch=%s",
		intent.Namespace, intent.Name, intent.CPURequest, intent.MemRequest/(1024*1024), intent.Arch)

	// STEP 2: Fetch candidate nodes from Redis
	nodeStates, err := s.repo.ListNodeStates(ctx)
	if err != nil {
		return "", fmt.Errorf("fetch node states: %w", err)
	}

	log.Printf("[SCHEDULER] fetched %d candidate nodes from Redis", len(nodeStates))

	// STEP 3: HARD FILTER
	filtered, failedNodes := s.filter(intent, nodeStates)
	if len(filtered) == 0 {
		return "", fmt.Errorf("no nodes passed filtering: %v", failedNodes)
	}

	log.Printf("[SCHEDULER] %d nodes passed filtering, %d failed", len(filtered), len(failedNodes))

	// STEP 4: SOFT SCORING
	scored := s.score(intent, filtered)
	
	log.Printf("[SCHEDULER] scored %d nodes, top: %s (%.2f)", 
		len(scored), scored[0].Name, scored[0].Score)

	// STEP 5-8: RESERVE, VALIDATE, BIND (with optimistic locking)
	nodeName, err := s.reserveAndBind(ctx, intent, scored, nodes)
	if err != nil {
		return "", fmt.Errorf("reserve and bind: %w", err)
	}

	return nodeName, nil
}

// extractPodIntent extracts scheduling requirements from pod
func (s *Scheduler) extractPodIntent(pod *v1.Pod) (*models.PodIntent, error) {
	intent := &models.PodIntent{
		UID:       string(pod.UID),
		Namespace: pod.Namespace,
		Name:      pod.Name,
		Scheduler: pod.Spec.SchedulerName,
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

	score := weightCPUFree*cpuFreeRatio +
		weightMemFree*memFreeRatio -
		weightFragmentation*fragmentation -
		weightTaint*taintPenalty

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

	// Try each node in score order
	for _, scored := range scoredNodes {
		nodeName := scored.Name
		state := scored.State

		log.Printf("[RESERVE] attempting node=%s score=%.2f version=%d",
			nodeName, scored.Score, state.Version)

		// STEP 5.1: Acquire node lock
		lockUUID, err := s.repo.LockNode(ctx, nodeName)
		if err != nil {
			log.Printf("[RESERVE] node=%s lock failed: %v", nodeName, err)
			continue
		}

		// Ensure unlock
		defer s.repo.UnlockNode(ctx, nodeName, lockUUID)

		// STEP 5.2: Read node state again
		freshState, err := s.repo.GetNodeState(ctx, nodeName)
		if err != nil {
			log.Printf("[RESERVE] node=%s fetch failed: %v", nodeName, err)
			continue
		}

		// STEP 5.2: Version check
		if freshState.Version != state.Version {
			log.Printf("[RESERVE] node=%s version mismatch (cached=%d fresh=%d)",
				nodeName, state.Version, freshState.Version)
			continue
		}

		// STEP 5.3: Re-check constraints
		if reason := s.checkHardConstraints(intent, freshState); reason != "" {
			log.Printf("[RESERVE] node=%s constraints failed on recheck: %s", nodeName, reason)
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
			continue
		}

		log.Printf("[RESERVE] node=%s reserved cpu=%dm mem=%dMi version=%d->%d",
			nodeName, intent.CPURequest, intent.MemRequest/(1024*1024), freshState.Version, newVersion)

		// STEP 5.5: Release lock
		s.repo.UnlockNode(ctx, nodeName, lockUUID)

		// STEP 6: API server validation
		kubeNode := s.findKubeNode(kubeNodes, nodeName)
		if kubeNode == nil || !s.validateAPINode(intent, kubeNode) {
			log.Printf("[RESERVE] node=%s API validation failed, rolling back", nodeName)
			s.repo.RollbackResources(ctx, nodeName, intent.CPURequest, intent.MemRequest)
			continue
		}

		// STEP 7: Bind pod
		if err := s.bindPod(ctx, intent, nodeName); err != nil {
			log.Printf("[RESERVE] node=%s bind failed: %v, rolling back", nodeName, err)
			s.repo.RollbackResources(ctx, nodeName, intent.CPURequest, intent.MemRequest)
			continue
		}

		log.Printf("[SUCCESS] pod=%s/%s scheduled to node=%s", intent.Namespace, intent.Name, nodeName)
		return nodeName, nil
	}

	return "", fmt.Errorf("no node could accommodate pod")
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

// UpdateNodeStateFromAPI updates Redis node state from Kubernetes API
func (s *Scheduler) UpdateNodeStateFromAPI(ctx context.Context, node *v1.Node) error {
	state := &models.NodeState{
		Name:  node.Name,
		Ready: false,
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
