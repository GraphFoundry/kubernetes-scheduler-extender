package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

const (
	schedulerName        = "my-scheduler"
	defaultSchedulerName = "default-scheduler"
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

	// Get service name from pod labels
	serviceName := getServiceName(freshPod)

	// Try to get optimal node from best-node-selector
	selectedNode := getOptimalNode(freshPod.Namespace, serviceName)

	if selectedNode == "" {
		log.Printf("No optimal node for pod %s/%s, delegating scheduling to %s", freshPod.Namespace, freshPod.Name, defaultSchedulerName)
		return delegateToDefaultScheduler(ctx, clientset, freshPod)
	}

	log.Printf("Using optimal node from best-node-selector: %s", selectedNode)
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
		return fmt.Errorf("failed to bind pod: %w", err)
	}

	return nil
}

func delegateToDefaultScheduler(ctx context.Context, clientset *kubernetes.Clientset, pod *v1.Pod) error {
	if pod.Spec.NodeName != "" {
		return nil
	}

	if pod.Spec.SchedulerName == defaultSchedulerName {
		return nil
	}

	podCopy := pod.DeepCopy()
	podCopy.Spec.SchedulerName = defaultSchedulerName

	_, err := clientset.CoreV1().Pods(pod.Namespace).Update(ctx, podCopy, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to delegate pod to %s: %w", defaultSchedulerName, err)
	}

	log.Printf("Delegated pod %s/%s to %s", pod.Namespace, pod.Name, defaultSchedulerName)
	return nil
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
