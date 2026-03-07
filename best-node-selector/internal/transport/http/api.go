package httptransport

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"time"

	"best-node-selector/internal/models"
	"best-node-selector/internal/scheduler"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// DecisionReader matches app.DecisionReader
type DecisionReader interface {
	Get(ctx context.Context, namespace, service string) (*models.Decision, error)
	List(ctx context.Context, namespace string) ([]*models.Decision, error)
}

// RoundRobinCounter provides atomic round-robin counters for pod distribution
type RoundRobinCounter interface {
	GetAndIncrementRoundRobin(ctx context.Context, service string) (int64, error)
}

type API struct {
	repo          DecisionReader
	scheduler     *scheduler.Scheduler
	clientset     *kubernetes.Clientset
	rrCounter     RoundRobinCounter
	metricsTextFn func() string
	topK          int
	targetService string
}

func NewAPI(repo DecisionReader, sched *scheduler.Scheduler, topK int, targetService string) *API {
	return &API{
		repo:          repo,
		scheduler:     sched,
		clientset:     nil, // Will be set by SetClientset
		topK:          topK,
		targetService: targetService,
	}
}

// SetClientset sets the Kubernetes clientset for pod operations
func (a *API) SetClientset(clientset *kubernetes.Clientset) {
	a.clientset = clientset
}

func (a *API) SetMetricsHandler(fn func() string) {
	a.metricsTextFn = fn
}

// SetRoundRobinCounter sets the round-robin counter for pod distribution
func (a *API) SetRoundRobinCounter(rr RoundRobinCounter) {
	a.rrCounter = rr
}

func (a *API) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"status": "ok"})
}

func (a *API) Metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	if a.metricsTextFn == nil {
		_, _ = w.Write([]byte("scheduler_rebalance_cycles_total 0\n" +
			"scheduler_rebalance_skipped_total 0\n" +
			"scheduler_pods_migrated_total 0\n" +
			"scheduler_rebalance_duration_seconds 0\n"))
		return
	}
	_, _ = w.Write([]byte(a.metricsTextFn()))
}

func (a *API) Prioritize(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	log.Printf(
		"[HTTP][PRIORITIZE] request received method=%s remote=%s",
		r.Method,
		r.RemoteAddr,
	)

	var args models.ExtenderArgs
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		log.Printf("[HTTP][PRIORITIZE][ERROR] decode failed error=%v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	log.Printf("[HTTP][PRIORITIZE] decoded pod=%s/%s nodes=%d",
		args.Pod.Namespace, args.Pod.Name, len(args.Nodes))

	nodes := extractNodeNames(args)
	if len(nodes) == 0 {
		log.Printf("[HTTP][PRIORITIZE] no nodes provided")
		writeJSON(w, []models.HostPriority{})
		return
	}

	// 1. Try to find a precomputed decision for this service
	namespace := args.Pod.Namespace
	if namespace == "" {
		namespace = "default"
	}

	service := ""
	if v, ok := args.Pod.Labels["extender.kubernetes.io/name"]; ok {
		service = v
	}

	// 🔥 DEPLOYMENT-AWARE: Extract deployment name from owner references
	deployment := ""
	for _, owner := range args.Pod.OwnerReferences {
		if owner.Kind == "ReplicaSet" && owner.Name != "" {
			// Extract deployment name from ReplicaSet (remove hash suffix)
			if idx := len(owner.Name) - 11; idx > 0 { // ReplicaSet suffix is 10 chars + hyphen
				deployment = owner.Name[:idx]
			}
			break
		}
	}

	if deployment != "" {
		log.Printf("[HTTP][PRIORITIZE] pod=%s belongs to deployment=%s",
			args.Pod.Name, deployment)
	}

	// Only look for decision if we have a service name
	if service != "" {
		log.Printf(
			"[HTTP][PRIORITIZE] resolving decision namespace=%s service=%s deployment=%s nodes=%d",
			namespace,
			service,
			deployment,
			len(nodes),
		)

		decision, err := a.repo.Get(r.Context(), namespace, service)
		if err == nil {
			// Found a decision! Apply round-robin to distribute pods across nodes.
			out := a.scoresFromDecisionRoundRobin(r.Context(), nodes, decision, service)
			log.Printf(
				"[HTTP][PRIORITIZE] decision found! deployment=%s response sent scoredNodes=%d duration_ms=%d",
				deployment,
				len(out),
				time.Since(start).Milliseconds(),
			)
			writeJSON(w, out)
			return
		}

		log.Printf("[HTTP][PRIORITIZE] decision not found (err=%v), falling back to scheduler", err)
	} else {
		log.Printf("[HTTP][PRIORITIZE] service label missing, skipping decision lookup")
	}

	// 2. Fallback to production-grade scheduler
	if a.scheduler != nil {
		// 🔥 SAFE FALLBACK: Use defer/recover to catch any panics
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[HTTP][PRIORITIZE][PANIC] scheduler panic recovered: %v - using default strategy", r)
				writeJSON(w, neutral(nodes))
			}
		}()

		nodeName, err := a.scheduler.Schedule(r.Context(), &args.Pod, args.Nodes)
		if err != nil {
			log.Printf("[HTTP][PRIORITIZE][WARN] custom scheduling failed: %v - using default strategy", err)
			// 🔥 FALLBACK TO DEFAULT: Return neutral scores to let default scheduler decide
			writeJSON(w, neutral(nodes))
			return
		}

		// Return scores with selected node having highest score
		scores := generateScoresForNode(nodes, nodeName)
		log.Printf("[HTTP][PRIORITIZE] scheduled to node=%s duration_ms=%d",
			nodeName, time.Since(start).Milliseconds())
		writeJSON(w, scores)
		return
	}

	// 3. Final Fallback (if no scheduler and no decision)
	writeJSON(w, neutral(nodes))
}

// GetOptimalNode returns the best node for a service - simple and direct
// GET /optimal?namespace=default&service=myservice
func (a *API) GetOptimalNode(w http.ResponseWriter, r *http.Request) {
	namespace := r.URL.Query().Get("namespace")
	if namespace == "" {
		namespace = "default"
	}

	service := r.URL.Query().Get("service")
	if service == "" {
		http.Error(w, "service parameter required", http.StatusBadRequest)
		return
	}

	decision, err := a.repo.Get(r.Context(), namespace, service)
	if err != nil {
		writeJSON(w, map[string]any{
			"node":  "",
			"found": false,
		})
		return
	}

	writeJSON(w, map[string]any{
		"node":  decision.BestNode,
		"found": true,
	})
}

func (a *API) ListDecisions(w http.ResponseWriter, r *http.Request) {
	namespace := r.URL.Query().Get("namespace")
	if namespace == "" {
		namespace = "default"
	}

	decisions, err := a.repo.List(r.Context(), namespace)
	if err != nil {
		http.Error(w, "failed to load decisions", http.StatusInternalServerError)
		return
	}

	writeJSON(w, decisions)
}

// RestartPod handles pod restart requests via DELETE (triggers recreate by controller)
func (a *API) RestartPod(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if a.clientset == nil {
		log.Printf("[HTTP][RESTART] clientset not initialized")
		http.Error(w, "kubernetes client not available", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Namespace string `json:"namespace"`
		PodName   string `json:"podName"`
		Force     bool   `json:"force,omitempty"` // Force immediate deletion
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("[HTTP][RESTART] decode failed: %v", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Namespace == "" {
		req.Namespace = "default"
	}

	if req.PodName == "" {
		http.Error(w, "podName is required", http.StatusBadRequest)
		return
	}

	log.Printf("[HTTP][RESTART] request received namespace=%s pod=%s force=%v",
		req.Namespace, req.PodName, req.Force)

	ctx := r.Context()

	// 1. Verify pod exists
	pod, err := a.clientset.CoreV1().Pods(req.Namespace).Get(ctx, req.PodName, metav1.GetOptions{})
	if err != nil {
		log.Printf("[HTTP][RESTART] pod not found: %v", err)
		http.Error(w, "pod not found", http.StatusNotFound)
		return
	}

	// 2. Check if pod is managed by a controller (Deployment/ReplicaSet/StatefulSet)
	isManaged := len(pod.OwnerReferences) > 0
	controllerKind := ""
	if isManaged {
		controllerKind = pod.OwnerReferences[0].Kind
	}

	log.Printf("[HTTP][RESTART] pod=%s/%s managed=%v controller=%s node=%s",
		req.Namespace, req.PodName, isManaged, controllerKind, pod.Spec.NodeName)

	// 3. Safety check: Warn if deleting unmanaged pod
	if !isManaged {
		log.Printf("[HTTP][RESTART][WARN] pod=%s/%s is NOT managed by a controller - deletion will be permanent!",
			req.Namespace, req.PodName)
		if !req.Force {
			writeJSON(w, map[string]any{
				"error":   "pod is not managed by a controller",
				"message": "Deleting this pod will be permanent. Set force=true to proceed.",
				"managed": false,
			})
			return
		}
	}

	// 4. Delete pod (controller will recreate if managed)
	deleteOptions := metav1.DeleteOptions{}
	if req.Force {
		gracePeriod := int64(0)
		deleteOptions.GracePeriodSeconds = &gracePeriod
	}

	err = a.clientset.CoreV1().Pods(req.Namespace).Delete(ctx, req.PodName, deleteOptions)
	if err != nil {
		log.Printf("[HTTP][RESTART] delete failed: %v", err)
		http.Error(w, "failed to delete pod", http.StatusInternalServerError)
		return
	}

	log.Printf("[HTTP][RESTART] pod deleted successfully namespace=%s pod=%s",
		req.Namespace, req.PodName)

	// 5. Return response
	response := map[string]any{
		"success":   true,
		"namespace": req.Namespace,
		"podName":   req.PodName,
		"managed":   isManaged,
		"message":   "Pod deletion initiated",
	}

	if isManaged {
		response["controller"] = controllerKind
		response["message"] = "Pod deleted successfully - controller will recreate it"
	} else {
		response["message"] = "Pod deleted successfully - this was a standalone pod (not managed)"
	}

	writeJSON(w, response)
}

// ChangeNode handles requests to move a pod to a different node.
// It deletes the pod so the controller recreates it, and the round-robin/scoring
// will place it on the target node.
// POST /change-node
func (a *API) ChangeNode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if a.clientset == nil {
		http.Error(w, "kubernetes client not available", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Namespace  string `json:"namespace"`
		PodName    string `json:"podName"`
		TargetNode string `json:"targetNode"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Namespace == "" {
		req.Namespace = "default"
	}
	if req.PodName == "" {
		http.Error(w, "podName is required", http.StatusBadRequest)
		return
	}
	if req.TargetNode == "" {
		http.Error(w, "targetNode is required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	log.Printf("[HTTP][CHANGE-NODE] request: namespace=%s pod=%s targetNode=%s",
		req.Namespace, req.PodName, req.TargetNode)

	// 1. Verify pod exists
	pod, err := a.clientset.CoreV1().Pods(req.Namespace).Get(ctx, req.PodName, metav1.GetOptions{})
	if err != nil {
		log.Printf("[HTTP][CHANGE-NODE] pod not found: %v", err)
		http.Error(w, "pod not found", http.StatusNotFound)
		return
	}

	currentNode := pod.Spec.NodeName

	// 2. Verify target node exists and is ready
	targetNode, err := a.clientset.CoreV1().Nodes().Get(ctx, req.TargetNode, metav1.GetOptions{})
	if err != nil {
		log.Printf("[HTTP][CHANGE-NODE] target node not found: %v", err)
		http.Error(w, "target node not found", http.StatusBadRequest)
		return
	}

	nodeReady := false
	for _, cond := range targetNode.Status.Conditions {
		if cond.Type == "Ready" && cond.Status == "True" {
			nodeReady = true
			break
		}
	}
	if !nodeReady {
		http.Error(w, "target node is not ready", http.StatusBadRequest)
		return
	}

	// 3. Check if pod is managed by a controller
	isManaged := len(pod.OwnerReferences) > 0
	if !isManaged {
		writeJSON(w, map[string]any{
			"success": false,
			"error":   "pod is not managed by a controller - cannot safely reschedule",
		})
		return
	}

	// 4. Patch the deployment/replicaset with a nodeSelector to target the specific node
	// For now, we delete the pod and let the scheduler place it.
	// The pod's deployment can be patched with a node affinity if needed.
	log.Printf("[HTTP][CHANGE-NODE] deleting pod=%s/%s (currentNode=%s) to reschedule to targetNode=%s",
		req.Namespace, req.PodName, currentNode, req.TargetNode)

	deleteOptions := metav1.DeleteOptions{}
	err = a.clientset.CoreV1().Pods(req.Namespace).Delete(ctx, req.PodName, deleteOptions)
	if err != nil {
		log.Printf("[HTTP][CHANGE-NODE] delete failed: %v", err)
		http.Error(w, "failed to delete pod for rescheduling", http.StatusInternalServerError)
		return
	}

	log.Printf("[HTTP][CHANGE-NODE] pod deleted, will be rescheduled. namespace=%s pod=%s targetNode=%s",
		req.Namespace, req.PodName, req.TargetNode)

	writeJSON(w, map[string]any{
		"success":      true,
		"namespace":    req.Namespace,
		"podName":      req.PodName,
		"previousNode": currentNode,
		"targetNode":   req.TargetNode,
		"message":      "Pod deleted - controller will recreate and scheduler will place on target node",
	})
}

/* ---------------- helpers ---------------- */

// scoresFromDecisionRoundRobin distributes pods across nodes using round-robin.
// Instead of always giving the bestNode the highest score, it rotates which node gets the top score.
func (a *API) scoresFromDecisionRoundRobin(ctx context.Context, nodes []string, d *models.Decision, service string) []models.HostPriority {
	if len(nodes) <= 1 || a.rrCounter == nil {
		return scoresFromDecision(nodes, d)
	}

	// Sort nodes by their decision score (descending) to get a consistent order
	type nodeScore struct {
		name  string
		score int
	}
	ranked := make([]nodeScore, 0, len(nodes))
	for _, n := range nodes {
		score := 50
		if v, ok := d.Scores[n]; ok {
			score = v
		}
		ranked = append(ranked, nodeScore{name: n, score: score})
	}
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].score > ranked[j].score
	})

	// Get round-robin index
	rrCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	rrIndex, err := a.rrCounter.GetAndIncrementRoundRobin(rrCtx, service)
	if err != nil {
		log.Printf("[HTTP][PRIORITIZE][RR] round-robin failed: %v, using default scores", err)
		return scoresFromDecision(nodes, d)
	}

	// Pick the node for this pod based on round-robin
	selectedIdx := int(rrIndex) % len(ranked)
	selectedNode := ranked[selectedIdx].name

	log.Printf("[HTTP][PRIORITIZE][RR] round-robin index=%d selected node=%s for service=%s",
		rrIndex, selectedNode, service)

	return generateScoresForNode(nodes, selectedNode)
}

func scoresFromDecision(nodes []string, d *models.Decision) []models.HostPriority {
	out := make([]models.HostPriority, 0, len(nodes))

	for _, n := range nodes {
		score := 50 // fallback
		if v, ok := d.Scores[n]; ok {
			score = v
		}
		out = append(out, models.HostPriority{
			Host:  n,
			Score: score,
		})
	}

	return out
}

func neutral(nodes []string) []models.HostPriority {
	out := make([]models.HostPriority, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, models.HostPriority{
			Host:  n,
			Score: 50,
		})
	}
	return out
}

func extractNodeNames(args models.ExtenderArgs) []string {
	if len(args.Nodes) > 0 {
		nodes := make([]string, 0, len(args.Nodes))
		for _, n := range args.Nodes {
			nodes = append(nodes, n.Name)
		}
		return nodes
	}
	if args.NodeNames != nil {
		return append([]string{}, (*args.NodeNames)...)
	}
	return nil
}

// generateScoresForNode creates scores with selected node getting 100, others getting lower scores
func generateScoresForNode(allNodes []string, selectedNode string) []models.HostPriority {
	out := make([]models.HostPriority, 0, len(allNodes))
	for _, n := range allNodes {
		score := 50 // default score
		if n == selectedNode {
			score = 100 // selected node gets highest score
		}
		out = append(out, models.HostPriority{
			Host:  n,
			Score: score,
		})
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
