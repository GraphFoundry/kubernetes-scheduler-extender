package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

const (
	schedulerName = "my-scheduler"
)

type OptimalResponse struct {
	Node  string `json:"node"`
	Found bool   `json:"found"`
}

func main() {
	fmt.Println("Starting custom scheduler...")

	config, err := rest.InClusterConfig()
	if err != nil {
		fmt.Printf("Failed to get in-cluster config, trying kubeconfig: %v\n", err)
		kubeconfig := filepath.Join(homedir.HomeDir(), ".kube", "config")
		if envVar := os.Getenv("KUBECONFIG"); envVar != "" {
			kubeconfig = envVar
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			panic(err.Error())
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	// Watch loop
	for {
		pods, err := clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{
			FieldSelector: fmt.Sprintf("spec.schedulerName=%s,spec.nodeName=", schedulerName),
		})
		if err != nil {
			fmt.Printf("Error listing pods: %v\n", err)
			time.Sleep(5 * time.Second)
			continue
		}

		for _, pod := range pods.Items {
			// Skip pods that are already bound to a node
			if pod.Spec.NodeName != "" {
				log.Printf("Pod %s/%s already bound to node %s, skipping", pod.Namespace, pod.Name, pod.Spec.NodeName)
				continue
			}

			fmt.Printf("Attempting to schedule pod: %s/%s\n", pod.Namespace, pod.Name)
			err := schedulePod(clientset, &pod)
			if err != nil {
				fmt.Printf("Error scheduling pod %s/%s: %v\n", pod.Namespace, pod.Name, err)
			} else {
				fmt.Printf("Successfully scheduled pod %s/%s\n", pod.Namespace, pod.Name)
			}
		}

		time.Sleep(2 * time.Second)
	}
}

func schedulePod(clientset *kubernetes.Clientset, pod *v1.Pod) error {
	ctx := context.TODO()

	// Re-fetch the pod to get the latest state and ResourceVersion
	freshPod, err := clientset.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to re-fetch pod: %w", err)
	}

	// Check if pod is already bound
	if freshPod.Spec.NodeName != "" {
		return nil // Already scheduled
	}

	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	if len(nodes.Items) == 0 {
		return fmt.Errorf("no nodes available for scheduling pod %s/%s", freshPod.Namespace, freshPod.Name)
	}

	// Build a map of eligible nodes (ready, schedulable, not control-plane)
	eligibleNodes := buildEligibleNodeMap(nodes.Items, freshPod)

	// Get service name from pod labels
	serviceName := getServiceName(freshPod)

	// Try to get optimal node from best-node-selector
	selectedNode := getOptimalNode(freshPod.Namespace, serviceName)

	needsFallback := false
	if selectedNode == "" {
		log.Printf("No optimal node for pod %s/%s, falling back to default-scheduler scoring",
			freshPod.Namespace, freshPod.Name)
		needsFallback = true
	} else if !eligibleNodes[selectedNode] {
		log.Printf("Optimal node %s is NOT eligible for pod %s/%s, falling back to default-scheduler scoring",
			selectedNode, freshPod.Namespace, freshPod.Name)
		needsFallback = true
	}

	if needsFallback {
		selectedNode, err = scoreAndSelectNode(ctx, clientset, nodes.Items, eligibleNodes, freshPod)
		if err != nil {
			return fmt.Errorf("fallback scheduling failed for pod %s/%s: %w", freshPod.Namespace, freshPod.Name, err)
		}
		log.Printf("Default-scheduler-style scoring selected node %s for pod %s/%s",
			selectedNode, freshPod.Namespace, freshPod.Name)
	} else {
		log.Printf("Using optimal node from best-node-selector: %s", selectedNode)
	}
	log.Printf("Selected node %s for pod %s/%s", selectedNode, freshPod.Namespace, freshPod.Name)

	binding := &v1.Binding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      freshPod.Name,
			Namespace: freshPod.Namespace,
			UID:       freshPod.UID,
		},
		Target: v1.ObjectReference{
			APIVersion: "v1",
			Kind:       "Node",
			Name:       selectedNode,
		},
	}

	err = clientset.CoreV1().Pods(freshPod.Namespace).Bind(ctx, binding, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to bind pod %s/%s to node %s: %w",
			freshPod.Namespace, freshPod.Name, selectedNode, err)
	}

	return nil
}

// buildEligibleNodeMap returns a set of node names that are ready, schedulable,
// and not control-plane nodes. This acts as a basic eligibility filter similar
// to what the default kube-scheduler would apply.
func buildEligibleNodeMap(nodes []v1.Node, pod *v1.Pod) map[string]bool {
	eligible := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		// Must be schedulable
		if node.Spec.Unschedulable {
			log.Printf("Node %s is unschedulable, skipping", node.Name)
			continue
		}

		// Must be Ready
		ready := false
		for _, cond := range node.Status.Conditions {
			if cond.Type == v1.NodeReady && cond.Status == v1.ConditionTrue {
				ready = true
				break
			}
		}
		if !ready {
			log.Printf("Node %s is not ready, skipping", node.Name)
			continue
		}

		// Check if the node is a control-plane node
		if isControlPlaneNode(&node) {
			// Only allow scheduling on control-plane if the pod tolerates the taint
			if !podToleratesControlPlaneTaints(pod, &node) {
				log.Printf("Node %s is control-plane and pod lacks toleration, skipping", node.Name)
				continue
			}
		}

		// Check all node taints are tolerated by the pod
		if !podToleratesAllTaints(pod, &node) {
			log.Printf("Node %s has untolerated taints, skipping", node.Name)
			continue
		}

		eligible[node.Name] = true
	}
	return eligible
}

// isControlPlaneNode checks if a node is a control-plane/master node
func isControlPlaneNode(node *v1.Node) bool {
	labels := node.Labels
	if _, ok := labels["node-role.kubernetes.io/control-plane"]; ok {
		return true
	}
	if _, ok := labels["node-role.kubernetes.io/master"]; ok {
		return true
	}
	// Name-based fallback
	nameLower := strings.ToLower(node.Name)
	if strings.Contains(nameLower, "control-plane") || strings.Contains(nameLower, "master") {
		return true
	}
	// Detect patterns like k8s-cp1
	parts := strings.Split(nameLower, "-")
	for _, part := range parts {
		if len(part) >= 2 && strings.HasPrefix(part, "cp") {
			allDigits := true
			for _, ch := range part[2:] {
				if ch < '0' || ch > '9' {
					allDigits = false
					break
				}
			}
			if allDigits {
				return true
			}
		}
	}
	return false
}

// podToleratesControlPlaneTaints checks if the pod has tolerations for control-plane taints
func podToleratesControlPlaneTaints(pod *v1.Pod, node *v1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key == "node-role.kubernetes.io/control-plane" ||
			taint.Key == "node-role.kubernetes.io/master" {
			if !hasToleration(pod, taint) {
				return false
			}
		}
	}
	return true
}

// podToleratesAllTaints checks if the pod tolerates all NoSchedule/NoExecute taints on the node
func podToleratesAllTaints(pod *v1.Pod, node *v1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Effect == v1.TaintEffectNoSchedule || taint.Effect == v1.TaintEffectNoExecute {
			if !hasToleration(pod, taint) {
				return false
			}
		}
	}
	return true
}

// hasToleration checks if a pod has a matching toleration for a taint
func hasToleration(pod *v1.Pod, taint v1.Taint) bool {
	for _, toleration := range pod.Spec.Tolerations {
		if toleration.Operator == v1.TolerationOpExists && toleration.Key == "" {
			return true // Tolerates everything
		}
		if toleration.Key == taint.Key {
			if toleration.Operator == v1.TolerationOpExists {
				return true
			}
			if toleration.Operator == v1.TolerationOpEqual && toleration.Value == taint.Value {
				if toleration.Effect == "" || toleration.Effect == taint.Effect {
					return true
				}
			}
			// Default operator is Equal
			if toleration.Operator == "" && toleration.Value == taint.Value {
				if toleration.Effect == "" || toleration.Effect == taint.Effect {
					return true
				}
			}
		}
	}
	return false
}

// scoreAndSelectNode replicates the default kube-scheduler's core scoring:
// LeastRequestedPriority + BalancedResourceAllocation.
// It sums resource requests of all non-terminal pods on each eligible node,
// then picks the node with the highest combined score.
func scoreAndSelectNode(ctx context.Context, clientset *kubernetes.Clientset, nodes []v1.Node, eligible map[string]bool, pod *v1.Pod) (string, error) {
	// Index allocatable resources by node name for eligible nodes
	type nodeResources struct {
		node       string
		allocCPU   int64 // milliCPU
		allocMem   int64 // bytes
		reqCPU     int64
		reqMem     int64
	}

	nodeMap := make(map[string]*nodeResources, len(eligible))
	for _, n := range nodes {
		if !eligible[n.Name] {
			continue
		}
		nodeMap[n.Name] = &nodeResources{
			node:     n.Name,
			allocCPU: n.Status.Allocatable.Cpu().MilliValue(),
			allocMem: n.Status.Allocatable.Memory().Value(),
		}
	}

	if len(nodeMap) == 0 {
		return "", fmt.Errorf("no eligible nodes")
	}

	// Sum existing pod resource requests per node
	allPods, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to list pods for resource accounting: %w", err)
	}

	for i := range allPods.Items {
		p := &allPods.Items[i]
		if p.Spec.NodeName == "" {
			continue
		}
		if p.Status.Phase == v1.PodSucceeded || p.Status.Phase == v1.PodFailed {
			continue
		}
		nr, ok := nodeMap[p.Spec.NodeName]
		if !ok {
			continue
		}
		for _, c := range p.Spec.Containers {
			nr.reqCPU += c.Resources.Requests.Cpu().MilliValue()
			nr.reqMem += c.Resources.Requests.Memory().Value()
		}
		for _, c := range p.Spec.InitContainers {
			// Init containers run sequentially; use max, not sum
			cpuReq := c.Resources.Requests.Cpu().MilliValue()
			memReq := c.Resources.Requests.Memory().Value()
			if cpuReq > nr.reqCPU {
				nr.reqCPU = cpuReq
			}
			if memReq > nr.reqMem {
				nr.reqMem = memReq
			}
		}
	}

	// Add the incoming pod's own requests
	var podCPU, podMem int64
	for _, c := range pod.Spec.Containers {
		podCPU += c.Resources.Requests.Cpu().MilliValue()
		podMem += c.Resources.Requests.Memory().Value()
	}

	// Score each node — higher is better
	bestNode := ""
	bestScore := -1.0

	for name, nr := range nodeMap {
		usedCPU := nr.reqCPU + podCPU
		usedMem := nr.reqMem + podMem

		// Check if the pod actually fits
		if usedCPU > nr.allocCPU || usedMem > nr.allocMem {
			log.Printf("[FALLBACK] node %s cannot fit pod (cpu=%dm/%dm mem=%d/%d), skipping",
				name, usedCPU, nr.allocCPU, usedMem, nr.allocMem)
			continue
		}

		// LeastRequestedPriority: prefer nodes with more free resources
		cpuScore := float64(nr.allocCPU-usedCPU) / float64(nr.allocCPU) * 100
		memScore := float64(nr.allocMem-usedMem) / float64(nr.allocMem) * 100
		leastRequested := (cpuScore + memScore) / 2

		// BalancedResourceAllocation: prefer nodes where CPU/mem usage is balanced
		cpuFraction := float64(usedCPU) / float64(nr.allocCPU)
		memFraction := float64(usedMem) / float64(nr.allocMem)
		balanced := (1 - math.Abs(cpuFraction-memFraction)) * 100

		// Combined score (default scheduler weights these equally)
		score := leastRequested + balanced

		log.Printf("[FALLBACK] node %s score=%.1f (leastReq=%.1f balanced=%.1f) cpu=%dm/%dm mem=%s/%s",
			name, score, leastRequested, balanced,
			usedCPU, nr.allocCPU,
			resource.NewQuantity(usedMem, resource.BinarySI).String(),
			resource.NewQuantity(nr.allocMem, resource.BinarySI).String())

		if score > bestScore {
			bestScore = score
			bestNode = name
		}
	}

	if bestNode == "" {
		return "", fmt.Errorf("no nodes with sufficient resources")
	}

	return bestNode, nil
}

// getServiceName extracts service name from pod labels
func getServiceName(pod *v1.Pod) string {
	if name, ok := pod.Labels["app"]; ok {
		return name
	}
	if name, ok := pod.Labels["app.kubernetes.io/name"]; ok {
		return name
	}
	if name, ok := pod.Labels["service"]; ok {
		return name
	}
	return pod.Name
}

// getOptimalNode calls best-node-selector to get the optimal node
func getOptimalNode(namespace, service string) string {
	baseURL := os.Getenv("BEST_NODE_SELECTOR_URL")
	if baseURL == "" {
		baseURL = "http://host.minikube.internal:9000"
	}

	url := fmt.Sprintf("%s/optimal?namespace=%s&service=%s", baseURL, namespace, service)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		log.Printf("Failed to call best-node-selector: %v", err)
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("best-node-selector returned status %d", resp.StatusCode)
		return ""
	}

	var result OptimalResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("Failed to decode response: %v", err)
		return ""
	}

	if result.Found && result.Node != "" {
		return result.Node
	}

	return ""
}
