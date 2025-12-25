package services

import (
	"context"
	"log"
	"os"
	"path/filepath"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type K8sPlacementProvider struct {
	clientset *kubernetes.Clientset
}

// K8sPlacementProvider queries the Kubernetes API to find
// which services are currently running on which nodes.
//
// It builds a mapping:
//   nodeName -> set of service names
//
// This allows the scheduler extender to understand service
// colocation when calculating node scores.

func NewK8sPlacementProvider() (*K8sPlacementProvider, error) {
	cfg, err := inClusterConfig()
	if err != nil {
		// fallback to kubeconfig for local run
		cfg, err = kubeConfig()
		if err != nil {
			return nil, err
		}
	}

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	return &K8sPlacementProvider{clientset: cs}, nil
}

func inClusterConfig() (*rest.Config, error) {
	return rest.InClusterConfig()
}

func kubeConfig() (*rest.Config, error) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = filepath.Join(home, ".kube", "config")
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

func (p *K8sPlacementProvider) BuildNodeServiceIndex(ctx context.Context, namespaces []string) (map[string]map[string]struct{}, error) {
	log.Printf("[PLACEMENT][K8S] building node-service index namespaces=%v", namespaces)

	index := make(map[string]map[string]struct{})
	scheduledPods := 0

	// If no namespaces specified, watch all
	if len(namespaces) == 0 {
		namespaces = []string{""}
	}

	for _, ns := range namespaces {
		pods, err := p.clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			log.Printf("[PLACEMENT][K8S][ERROR] failed to list pods in namespace=%s error=%v", ns, err)
			continue
		}

		for _, pod := range pods.Items {
			node := pod.Spec.NodeName
			if node == "" {
				continue
			}
			scheduledPods++

			svc := serviceNameFromLabels(pod.Labels)
			if svc == "" {
				continue
			}

			if _, ok := index[node]; !ok {
				index[node] = make(map[string]struct{})
			}
			index[node][svc] = struct{}{}
		}
	}

	log.Printf(
		"[PLACEMENT][K8S] index built nodes=%d scheduledPods=%d",
		len(index),
		scheduledPods,
	)

	return index, nil
}

func serviceNameFromLabels(labels map[string]string) string {
	// Try common Kubernetes conventions
	if v := labels["extender.kubernetes.io/name"]; v != "" {
		return v
	}
	if v := labels["extender"]; v != "" {
		return v
	}
	if v := labels["k8s-extender"]; v != "" {
		return v
	}
	// Try Istio canonical name
	if v := labels["service.istio.io/canonical-name"]; v != "" {
		return v
	}
	// Try standard app label
	if v := labels["app"]; v != "" {
		return v
	}
	return ""
}
