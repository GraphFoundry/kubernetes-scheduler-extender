package rebalancer

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"best-node-selector/internal/config"
	"best-node-selector/internal/models"
	"best-node-selector/internal/redis"
	"best-node-selector/internal/services"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

const (
	rebalanceLastRunKeyPrefix   = "rebalance:last_run"
	rebalanceLastMovedPodPrefix = "rebalance:pod_moved:"
)

type Stats struct {
	cyclesTotal   atomic.Int64
	skippedTotal  atomic.Int64
	podsMigrated  atomic.Int64
	durationNanos atomic.Int64
}

type Controller struct {
	cfg       config.Config
	repo      *redis.SchedulerRepository
	metrics   services.MetricsProvider
	clientset *kubernetes.Clientset
	stats     *Stats
}

func New(cfg config.Config, repo *redis.SchedulerRepository, metrics services.MetricsProvider, clientset *kubernetes.Clientset) *Controller {
	return &Controller{cfg: cfg, repo: repo, metrics: metrics, clientset: clientset, stats: &Stats{}}
}

func (c *Controller) Run(ctx context.Context) {
	if c.cfg.RebalanceInterval <= 0 {
		c.cfg.RebalanceInterval = 5 * time.Minute
	}
	ticker := time.NewTicker(c.cfg.RebalanceInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.RunOnce(ctx)
		}
	}
}

func (c *Controller) RunOnce(ctx context.Context) {
	start := time.Now()
	c.stats.cyclesTotal.Add(1)
	defer c.stats.durationNanos.Store(time.Since(start).Nanoseconds())

	if !c.cfg.RebalancingEnabled {
		c.stats.skippedTotal.Add(1)
		return
	}

	if ok := c.cooldownElapsed(ctx, start); !ok {
		c.stats.skippedTotal.Add(1)
		return
	}

	nodeStates, err := c.repo.ListNodeStates(ctx)
	if err != nil || len(nodeStates) == 0 {
		c.stats.skippedTotal.Add(1)
		return
	}

	latencyHotspot := c.hasLatencyHotspot(ctx)
	triggered, overloaded, freeNodes := c.evaluateTriggers(nodeStates, latencyHotspot)
	if !triggered {
		c.stats.skippedTotal.Add(1)
		return
	}

	maxMoves := c.cfg.MaxPodsMovedPerCycle
	if maxMoves <= 0 {
		maxMoves = 3
	}
	migrated := c.executeLimitedMigrations(ctx, overloaded, freeNodes, latencyHotspot, maxMoves)
	c.stats.podsMigrated.Add(int64(migrated))

	_ = c.repo.SetUnixTime(ctx, rebalanceLastRunKeyPrefix, start.UTC(), 24*time.Hour)
	log.Printf("[REBALANCER] cycle completed moved=%d triggered=%v", migrated, triggered)
}

func (c *Controller) cooldownElapsed(ctx context.Context, now time.Time) bool {
	last, err := c.repo.GetUnixTime(ctx, rebalanceLastRunKeyPrefix)
	if err != nil {
		return true
	}
	cooldown := c.cfg.RebalanceCooldown
	if cooldown <= 0 {
		cooldown = 30 * time.Minute
	}
	return now.Sub(last) >= cooldown
}

func (c *Controller) hasLatencyHotspot(ctx context.Context) bool {
	threshold := c.cfg.RebalanceLatencyThreshold
	if threshold <= 0 {
		threshold = 40
	}

	svcs, err := c.metrics.GetServices(ctx)
	if err != nil {
		return false
	}
	limit := len(svcs.Services)
	if limit > 10 {
		limit = 10
	}
	for i := 0; i < limit; i++ {
		peers, err := c.metrics.GetPeers(ctx, svcs.Services[i].Name, "out", 5)
		if err != nil {
			continue
		}
		for _, p := range peers.Peers {
			if p.Metrics.P95 > threshold {
				return true
			}
		}
	}
	return false
}

func (c *Controller) evaluateTriggers(states []*models.NodeState, latencyHotspot bool) (bool, []*models.NodeState, []*models.NodeState) {
	maxCPU := 0.0
	minCPU := 1.0
	maxMem := 0.0
	minMem := 1.0
	overloaded := []*models.NodeState{}
	freeNodes := []*models.NodeState{}

	for _, s := range states {
		if !s.Ready || s.CPUAllocatable <= 0 || s.MemAllocatable <= 0 {
			continue
		}
		cpu := float64(s.CPUUsed) / float64(s.CPUAllocatable)
		mem := float64(s.MemUsed) / float64(s.MemAllocatable)
		if cpu > maxCPU {
			maxCPU = cpu
		}
		if cpu < minCPU {
			minCPU = cpu
		}
		if mem > maxMem {
			maxMem = mem
		}
		if mem < minMem {
			minMem = mem
		}
		if cpu > 0.75 || mem > 0.75 {
			overloaded = append(overloaded, s)
		}
		if cpu < 0.25 && mem < 0.25 {
			freeNodes = append(freeNodes, s)
		}
	}

	imbalance := (maxCPU-minCPU) > 0.35 || (maxMem-minMem) > 0.35
	freeDetected := len(freeNodes) > 0
	triggered := imbalance || freeDetected || latencyHotspot

	sort.Slice(overloaded, func(i, j int) bool {
		left := float64(overloaded[i].CPUUsed)/float64(overloaded[i].CPUAllocatable) + float64(overloaded[i].MemUsed)/float64(overloaded[i].MemAllocatable)
		right := float64(overloaded[j].CPUUsed)/float64(overloaded[j].CPUAllocatable) + float64(overloaded[j].MemUsed)/float64(overloaded[j].MemAllocatable)
		return left > right
	})
	return triggered, overloaded, freeNodes
}

func (c *Controller) executeLimitedMigrations(ctx context.Context, overloaded []*models.NodeState, free []*models.NodeState, latencyHotspot bool, maxMoves int) int {
	if len(overloaded) == 0 || len(free) == 0 {
		return 0
	}
	deadline := time.Now().Add(c.cfg.MaxRebalanceDuration)
	if c.cfg.MaxRebalanceDuration <= 0 {
		deadline = time.Now().Add(30 * time.Second)
	}

	moved := 0
	for _, src := range overloaded {
		if moved >= maxMoves || time.Now().After(deadline) {
			break
		}

		pods, err := c.clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + src.Name})
		if err != nil {
			continue
		}

		for _, pod := range pods.Items {
			if moved >= maxMoves || time.Now().After(deadline) {
				break
			}
			if !c.isPodEligible(&pod) {
				continue
			}
			if !c.pdbAllowsDisruption(ctx, &pod) {
				continue
			}
			if c.wasMovedRecently(ctx, string(pod.UID)) {
				continue
			}
			if !c.benefitExceedsCost(src, latencyHotspot) {
				continue
			}

			if err := c.clientset.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
				continue
			}
			_ = c.repo.SetUnixTime(ctx, rebalanceLastMovedPodPrefix+string(pod.UID), time.Now().UTC(), 24*time.Hour)
			moved++
		}
	}
	return moved
}

func (c *Controller) isPodEligible(pod *v1.Pod) bool {
	if pod == nil {
		return false
	}
	if pod.Namespace == "kube-system" {
		return false
	}
	minAge := c.cfg.MinPodAgeForRebalance
	if minAge <= 0 {
		minAge = 10 * time.Minute
	}
	if time.Since(pod.CreationTimestamp.Time) < minAge {
		return false
	}
	for _, owner := range pod.OwnerReferences {
		kind := strings.ToLower(owner.Kind)
		if kind == "daemonset" || kind == "statefulset" {
			return false
		}
	}
	// Only skip pods on control-plane nodes if the node has NoSchedule/NoExecute taints
	if c.isNodeTainted(pod.Spec.NodeName) {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.RestartCount > 0 {
			return false
		}
	}
	return len(pod.OwnerReferences) > 0
}

// isNodeTainted checks if a node has NoSchedule or NoExecute taints
func (c *Controller) isNodeTainted(nodeName string) bool {
	node, err := c.clientset.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if err != nil {
		return false
	}
	for _, taint := range node.Spec.Taints {
		if taint.Effect == v1.TaintEffectNoSchedule || taint.Effect == v1.TaintEffectNoExecute {
			return true
		}
	}
	return false
}

func (c *Controller) pdbAllowsDisruption(ctx context.Context, pod *v1.Pod) bool {
	pdbs, err := c.clientset.PolicyV1().PodDisruptionBudgets(pod.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false
	}
	for _, pdb := range pdbs.Items {
		selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil {
			continue
		}
		if selector.Empty() {
			continue
		}
		if selector.Matches(labels.Set(pod.Labels)) {
			return pdb.Status.DisruptionsAllowed > 0 && hasMinAvailable(&pdb)
		}
	}
	return true
}

func hasMinAvailable(pdb *policyv1.PodDisruptionBudget) bool {
	if pdb == nil {
		return false
	}
	if pdb.Spec.MinAvailable == nil {
		return true
	}
	return pdb.Status.CurrentHealthy > pdb.Status.DesiredHealthy
}

func (c *Controller) wasMovedRecently(ctx context.Context, podUID string) bool {
	last, err := c.repo.GetUnixTime(ctx, rebalanceLastMovedPodPrefix+podUID)
	if err != nil {
		return false
	}
	window := c.cfg.PodMoveAntiThrashWindow
	if window <= 0 {
		window = time.Hour
	}
	return time.Since(last) < window
}

func (c *Controller) benefitExceedsCost(src *models.NodeState, latencyHotspot bool) bool {
	cpu := float64(src.CPUUsed) / float64(src.CPUAllocatable)
	mem := float64(src.MemUsed) / float64(src.MemAllocatable)
	expectedBenefit := max(0, cpu-0.65) + max(0, mem-0.65)
	if latencyHotspot {
		expectedBenefit += 0.3
	}
	migrationCost := 0.2 + 0.2 + 0.2 // restart + warmup + cache loss
	return expectedBenefit > migrationCost
}

func (c *Controller) MetricsText() string {
	durationSec := float64(c.stats.durationNanos.Load()) / float64(time.Second)
	return fmt.Sprintf(
		"scheduler_rebalance_cycles_total %d\n"+
			"scheduler_rebalance_skipped_total %d\n"+
			"scheduler_pods_migrated_total %d\n"+
			"scheduler_rebalance_duration_seconds %f\n",
		c.stats.cyclesTotal.Load(),
		c.stats.skippedTotal.Load(),
		c.stats.podsMigrated.Load(),
		durationSec,
	)
}

func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
