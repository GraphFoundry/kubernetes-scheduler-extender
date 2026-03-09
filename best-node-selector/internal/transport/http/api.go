package httptransport

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
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

// PreferenceStore provides read/write access to user node preferences
type PreferenceStore interface {
	SetNodePreference(ctx context.Context, namespace, service, nodeName string) error
	GetNodePreference(ctx context.Context, namespace, service string) (string, error)
	DeleteNodePreference(ctx context.Context, namespace, service string) error
}

// OverrideStore provides one-time node override for change-node operations
type OverrideStore interface {
	SetNodeOverride(ctx context.Context, namespace, service, nodeName string) error
	ConsumeNodeOverride(ctx context.Context, namespace, service string) (string, bool)
}

// ChangeNodeLocker provides distributed locking for change-node operations
type ChangeNodeLocker interface {
	AcquireChangeNodeLock(ctx context.Context, namespace, service string) (string, error)
	ReleaseChangeNodeLock(ctx context.Context, namespace, service, lockUUID string) error
}

type API struct {
	repo          DecisionReader
	scheduler     *scheduler.Scheduler
	clientset     *kubernetes.Clientset
	rrCounter     RoundRobinCounter
	prefStore     PreferenceStore
	overrideStore OverrideStore
	changeLocker  ChangeNodeLocker
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

// SetPreferenceStore sets the preference store for user node preferences
func (a *API) SetPreferenceStore(ps PreferenceStore) {
	a.prefStore = ps
}

// SetOverrideStore sets the override store for change-node operations
func (a *API) SetOverrideStore(os OverrideStore) {
	a.overrideStore = os
}

// SetChangeNodeLocker sets the distributed locker for change-node operations
func (a *API) SetChangeNodeLocker(locker ChangeNodeLocker) {
	a.changeLocker = locker
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
	if v, ok := args.Pod.Labels["app"]; ok {
		service = v
	} else if v, ok := args.Pod.Labels["app.kubernetes.io/name"]; ok {
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

		// Check for a pending change-node override first (highest priority)
		if a.overrideStore != nil {
			if overrideNode, ok := a.overrideStore.ConsumeNodeOverride(r.Context(), namespace, service); ok {
				log.Printf("[HTTP][PRIORITIZE] override consumed service=%s node=%s", service, overrideNode)
				out := generateScoresForNode(nodes, overrideNode)
				writeJSON(w, out)
				return
			}
		}

		// Check for a persistent user preference (always honored over scorer decisions)
		if a.prefStore != nil {
			prefNode, prefErr := a.prefStore.GetNodePreference(r.Context(), namespace, service)
			if prefErr == nil && prefNode != "" {
				log.Printf("[HTTP][PRIORITIZE] user preference found service=%s node=%s", service, prefNode)
				out := generateScoresForNode(nodes, prefNode)
				writeJSON(w, out)
				return
			}
		}

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

	// 1. Check for a pending change-node override (one-time, consumed on read)
	if a.overrideStore != nil {
		if overrideNode, ok := a.overrideStore.ConsumeNodeOverride(r.Context(), namespace, service); ok {
			log.Printf("[HTTP][OPTIMAL] override consumed namespace=%s service=%s node=%s", namespace, service, overrideNode)
			writeJSON(w, map[string]any{
				"node":     overrideNode,
				"found":    true,
				"override": true,
			})
			return
		}
	}

	// 2. Check for a persistent user preference (always honored)
	if a.prefStore != nil {
		prefNode, err := a.prefStore.GetNodePreference(r.Context(), namespace, service)
		if err == nil && prefNode != "" {
			log.Printf("[HTTP][OPTIMAL] user preference found namespace=%s service=%s node=%s", namespace, service, prefNode)
			writeJSON(w, map[string]any{
				"node":       prefNode,
				"found":      true,
				"preference": true,
			})
			return
		}
	}

	// 3. Return precomputed decision from scorer
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

// ChangeNode handles requests to move a pod to a different node gracefully.
// It scales up the deployment, waits for a new pod on the target node,
// then removes the old pod — avoiding downtime.
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
	targetNodeObj, err := a.clientset.CoreV1().Nodes().Get(ctx, req.TargetNode, metav1.GetOptions{})
	if err != nil {
		log.Printf("[HTTP][CHANGE-NODE] target node not found: %v", err)
		http.Error(w, "target node not found", http.StatusBadRequest)
		return
	}

	nodeReady := false
	for _, cond := range targetNodeObj.Status.Conditions {
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

	// 4. Extract service name from pod labels
	serviceName := serviceNameFromPodLabels(pod.Labels)
	if serviceName == "" {
		log.Printf("[HTTP][CHANGE-NODE] WARNING: could not extract service name from pod labels, override may not work")
	}

	// 5. Find the deployment that owns this pod (via ReplicaSet → Deployment)
	deploymentName := ""
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "ReplicaSet" && owner.Name != "" {
			// Look up the ReplicaSet to find its owning Deployment
			rs, rsErr := a.clientset.AppsV1().ReplicaSets(req.Namespace).Get(ctx, owner.Name, metav1.GetOptions{})
			if rsErr == nil {
				for _, rsOwner := range rs.OwnerReferences {
					if rsOwner.Kind == "Deployment" {
						deploymentName = rsOwner.Name
						break
					}
				}
			}
			if deploymentName == "" {
				// Fallback: strip hash suffix from ReplicaSet name
				parts := strings.Split(owner.Name, "-")
				if len(parts) > 1 {
					deploymentName = strings.Join(parts[:len(parts)-1], "-")
				}
			}
			break
		}
	}

	if deploymentName == "" {
		log.Printf("[HTTP][CHANGE-NODE] could not determine deployment for pod=%s, falling back to destructive change", req.PodName)
		a.changeNodeDestructive(w, r, req.Namespace, req.PodName, req.TargetNode, currentNode, serviceName)
		return
	}

	// 6. Acquire Redis lock to prevent concurrent change-node operations
	var lockUUID string
	if a.changeLocker != nil && serviceName != "" {
		var lockErr error
		lockUUID, lockErr = a.changeLocker.AcquireChangeNodeLock(ctx, req.Namespace, serviceName)
		if lockErr != nil {
			log.Printf("[HTTP][CHANGE-NODE] failed to acquire lock: %v", lockErr)
			http.Error(w, fmt.Sprintf("change-node operation already in progress: %v", lockErr), http.StatusConflict)
			return
		}
		log.Printf("[HTTP][CHANGE-NODE] acquired lock for %s/%s (uuid=%s)", req.Namespace, serviceName, lockUUID)
		defer func() {
			if releaseErr := a.changeLocker.ReleaseChangeNodeLock(ctx, req.Namespace, serviceName, lockUUID); releaseErr != nil {
				log.Printf("[HTTP][CHANGE-NODE] WARNING: failed to release lock: %v", releaseErr)
			} else {
				log.Printf("[HTTP][CHANGE-NODE] released lock for %s/%s", req.Namespace, serviceName)
			}
		}()
	}

	// 7. Store the node override in Redis BEFORE scaling up
	if a.overrideStore != nil && serviceName != "" {
		if err := a.overrideStore.SetNodeOverride(ctx, req.Namespace, serviceName, req.TargetNode); err != nil {
			log.Printf("[HTTP][CHANGE-NODE] WARNING: failed to store override: %v", err)
		} else {
			log.Printf("[HTTP][CHANGE-NODE] stored node override namespace=%s service=%s targetNode=%s",
				req.Namespace, serviceName, req.TargetNode)
		}
	}

	// 8. Scale up the deployment by 1 replica
	deployment, err := a.clientset.AppsV1().Deployments(req.Namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		log.Printf("[HTTP][CHANGE-NODE] failed to get deployment %s: %v", deploymentName, err)
		http.Error(w, "failed to get deployment", http.StatusInternalServerError)
		return
	}

	originalReplicas := int32(1)
	if deployment.Spec.Replicas != nil {
		originalReplicas = *deployment.Spec.Replicas
	}
	scaledReplicas := originalReplicas + 1

	log.Printf("[HTTP][CHANGE-NODE] scaling deployment=%s from %d to %d replicas",
		deploymentName, originalReplicas, scaledReplicas)

	scale, err := a.clientset.AppsV1().Deployments(req.Namespace).GetScale(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		log.Printf("[HTTP][CHANGE-NODE] failed to get scale: %v", err)
		http.Error(w, "failed to scale deployment", http.StatusInternalServerError)
		return
	}
	scale.Spec.Replicas = scaledReplicas
	_, err = a.clientset.AppsV1().Deployments(req.Namespace).UpdateScale(ctx, deploymentName, scale, metav1.UpdateOptions{})
	if err != nil {
		log.Printf("[HTTP][CHANGE-NODE] failed to scale up: %v", err)
		http.Error(w, "failed to scale up deployment", http.StatusInternalServerError)
		return
	}

	log.Printf("[HTTP][CHANGE-NODE] deployment scaled up, waiting for new pod on targetNode=%s", req.TargetNode)

	// 9. Wait for a new pod on the target node to be Running+Ready (poll with timeout)
	const pollInterval = 2 * time.Second
	const pollTimeout = 90 * time.Second
	deadline := time.Now().Add(pollTimeout)
	newPodReady := false

	for time.Now().Before(deadline) {
		time.Sleep(pollInterval)

		pods, listErr := a.clientset.CoreV1().Pods(req.Namespace).List(ctx, metav1.ListOptions{})
		if listErr != nil {
			log.Printf("[HTTP][CHANGE-NODE] failed to list pods: %v", listErr)
			continue
		}

		for _, p := range pods.Items {
			// Skip the old pod
			if p.Name == req.PodName {
				continue
			}
			// Must be on the target node
			if p.Spec.NodeName != req.TargetNode {
				continue
			}
			// Must belong to the same deployment (check labels match)
			if !labelsMatchService(p.Labels, serviceName) {
				continue
			}
			// Must be Running and Ready
			if p.Status.Phase != "Running" {
				continue
			}
			ready := false
			for _, cond := range p.Status.Conditions {
				if cond.Type == "Ready" && cond.Status == "True" {
					ready = true
					break
				}
			}
			if ready {
				log.Printf("[HTTP][CHANGE-NODE] new pod %s is Running+Ready on targetNode=%s",
					p.Name, req.TargetNode)
				newPodReady = true
				break
			}
		}

		if newPodReady {
			break
		}
		log.Printf("[HTTP][CHANGE-NODE] still waiting for new pod on targetNode=%s ...", req.TargetNode)
	}

	if !newPodReady {
		// Timeout: scale back down and report failure
		log.Printf("[HTTP][CHANGE-NODE] TIMEOUT waiting for new pod, scaling back down")
		a.scaleDeployment(ctx, req.Namespace, deploymentName, originalReplicas)
		http.Error(w, "timeout waiting for new pod to become ready on target node", http.StatusGatewayTimeout)
		return
	}

	// 10. Delete the old pod now that the new one is healthy
	log.Printf("[HTTP][CHANGE-NODE] deleting old pod=%s/%s (currentNode=%s)",
		req.Namespace, req.PodName, currentNode)

	deleteOptions := metav1.DeleteOptions{}
	err = a.clientset.CoreV1().Pods(req.Namespace).Delete(ctx, req.PodName, deleteOptions)
	if err != nil {
		log.Printf("[HTTP][CHANGE-NODE] failed to delete old pod: %v", err)
		// Continue anyway — the new pod is healthy, scale back down
	}

	// 11. Scale deployment back to original replicas
	a.scaleDeployment(ctx, req.Namespace, deploymentName, originalReplicas)

	log.Printf("[HTTP][CHANGE-NODE] graceful migration complete. namespace=%s pod=%s %s→%s",
		req.Namespace, req.PodName, currentNode, req.TargetNode)

	writeJSON(w, map[string]any{
		"success":      true,
		"namespace":    req.Namespace,
		"podName":      req.PodName,
		"previousNode": currentNode,
		"targetNode":   req.TargetNode,
		"message":      fmt.Sprintf("Pod gracefully migrated from %s to %s (zero-downtime)", currentNode, req.TargetNode),
	})
}

// scaleDeployment sets the replica count for a deployment
func (a *API) scaleDeployment(ctx context.Context, namespace, deploymentName string, replicas int32) {
	scale, err := a.clientset.AppsV1().Deployments(namespace).GetScale(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		log.Printf("[HTTP][CHANGE-NODE] failed to get scale for rollback: %v", err)
		return
	}
	scale.Spec.Replicas = replicas
	_, err = a.clientset.AppsV1().Deployments(namespace).UpdateScale(ctx, deploymentName, scale, metav1.UpdateOptions{})
	if err != nil {
		log.Printf("[HTTP][CHANGE-NODE] failed to scale to %d: %v", replicas, err)
	} else {
		log.Printf("[HTTP][CHANGE-NODE] scaled deployment=%s to %d replicas", deploymentName, replicas)
	}
}

// labelsMatchService checks if a pod's labels indicate it belongs to the given service
func labelsMatchService(labels map[string]string, service string) bool {
	if service == "" {
		return false
	}
	svcFromLabels := serviceNameFromPodLabels(labels)
	return svcFromLabels == service
}

// changeNodeDestructive is the fallback when the deployment cannot be determined.
// It uses the old destructive approach: store override + delete pod.
func (a *API) changeNodeDestructive(w http.ResponseWriter, r *http.Request, namespace, podName, targetNode, currentNode, serviceName string) {
	ctx := r.Context()

	if a.overrideStore != nil && serviceName != "" {
		if err := a.overrideStore.SetNodeOverride(ctx, namespace, serviceName, targetNode); err != nil {
			log.Printf("[HTTP][CHANGE-NODE] WARNING: failed to store override: %v", err)
		}
	}

	deleteOptions := metav1.DeleteOptions{}
	err := a.clientset.CoreV1().Pods(namespace).Delete(ctx, podName, deleteOptions)
	if err != nil {
		log.Printf("[HTTP][CHANGE-NODE] delete failed: %v", err)
		http.Error(w, "failed to delete pod for rescheduling", http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{
		"success":      true,
		"namespace":    namespace,
		"podName":      podName,
		"previousNode": currentNode,
		"targetNode":   targetNode,
		"message":      "Pod deleted - controller will recreate and scheduler will place on target node",
	})
}

// SetPreference saves the user's preferred node for a service.
// POST /preference  {"namespace":"default","service":"myapp","node":"worker-1"}
func (a *API) SetPreference(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if a.prefStore == nil {
		http.Error(w, "preference store not available", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Namespace string `json:"namespace"`
		Service   string `json:"service"`
		Node      string `json:"node"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Namespace == "" {
		req.Namespace = "default"
	}
	if req.Service == "" || req.Node == "" {
		http.Error(w, "service and node are required", http.StatusBadRequest)
		return
	}

	if err := a.prefStore.SetNodePreference(r.Context(), req.Namespace, req.Service, req.Node); err != nil {
		log.Printf("[HTTP][PREFERENCE] set failed: %v", err)
		http.Error(w, "failed to set preference", http.StatusInternalServerError)
		return
	}

	log.Printf("[HTTP][PREFERENCE] set namespace=%s service=%s node=%s", req.Namespace, req.Service, req.Node)
	writeJSON(w, map[string]any{
		"success":   true,
		"namespace": req.Namespace,
		"service":   req.Service,
		"node":      req.Node,
		"message":   "Preference saved",
	})
}

// DeletePreference removes the user's preferred node for a service.
// DELETE /preference?namespace=default&service=myapp
func (a *API) DeletePreference(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if a.prefStore == nil {
		http.Error(w, "preference store not available", http.StatusServiceUnavailable)
		return
	}

	namespace := r.URL.Query().Get("namespace")
	if namespace == "" {
		namespace = "default"
	}
	service := r.URL.Query().Get("service")
	if service == "" {
		http.Error(w, "service parameter required", http.StatusBadRequest)
		return
	}

	if err := a.prefStore.DeleteNodePreference(r.Context(), namespace, service); err != nil {
		log.Printf("[HTTP][PREFERENCE] delete failed: %v", err)
		http.Error(w, "failed to delete preference", http.StatusInternalServerError)
		return
	}

	log.Printf("[HTTP][PREFERENCE] deleted namespace=%s service=%s", namespace, service)
	writeJSON(w, map[string]any{
		"success":   true,
		"namespace": namespace,
		"service":   service,
		"message":   "Preference removed — scheduler logic takes over",
	})
}

// GetPreference returns the user's preferred node for a service.
// GET /preference?namespace=default&service=myapp
func (a *API) GetPreference(w http.ResponseWriter, r *http.Request) {
	if a.prefStore == nil {
		http.Error(w, "preference store not available", http.StatusServiceUnavailable)
		return
	}

	namespace := r.URL.Query().Get("namespace")
	if namespace == "" {
		namespace = "default"
	}
	service := r.URL.Query().Get("service")
	if service == "" {
		http.Error(w, "service parameter required", http.StatusBadRequest)
		return
	}

	node, err := a.prefStore.GetNodePreference(r.Context(), namespace, service)
	if err != nil {
		log.Printf("[HTTP][PREFERENCE] get failed: %v", err)
		http.Error(w, "failed to get preference", http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{
		"namespace": namespace,
		"service":   service,
		"node":      node,
		"hasPreference": node != "",
	})
}

/* ---------------- helpers ---------------- */

// scoresFromDecisionRoundRobin distributes pods across nodes using round-robin.
// If a user preference exists, the preferred node always gets the highest score.
func (a *API) scoresFromDecisionRoundRobin(ctx context.Context, nodes []string, d *models.Decision, service string) []models.HostPriority {
	// If user has a preference, always prioritise that node
	if d.PreferredNode != "" {
		for _, n := range nodes {
			if n == d.PreferredNode {
				log.Printf("[HTTP][PRIORITIZE][PREF] using preferred node=%s for service=%s", d.PreferredNode, service)
				return generateScoresForNode(nodes, d.PreferredNode)
			}
		}
		log.Printf("[HTTP][PRIORITIZE][PREF] preferred node=%s not in candidate list, falling back", d.PreferredNode)
	}

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

// serviceNameFromPodLabels extracts the service name from pod labels
func serviceNameFromPodLabels(labels map[string]string) string {
	if v := labels["app"]; v != "" {
		return v
	}
	if v := labels["app.kubernetes.io/name"]; v != "" {
		return v
	}
	if v := labels["service.istio.io/canonical-name"]; v != "" {
		return v
	}
	return ""
}
