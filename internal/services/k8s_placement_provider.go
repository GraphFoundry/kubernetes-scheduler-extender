package services

import (
	"context"
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

func (p *K8sPlacementProvider) BuildNodeServiceIndex(ctx context.Context) (map[string]map[string]struct{}, error) {
	// "" means all namespaces
	pods, err := p.clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	index := make(map[string]map[string]struct{})

	for _, pod := range pods.Items {
		node := pod.Spec.NodeName
		if node == "" {
			continue // not scheduled yet
		}

		labels := pod.Labels
		svc := serviceNameFromLabels(labels)
		if svc == "" {
			continue
		}

		if _, ok := index[node]; !ok {
			index[node] = make(map[string]struct{})
		}
		index[node][svc] = struct{}{}
	}

	return index, nil
}

func serviceNameFromLabels(labels map[string]string) string {
	// Try common Kubernetes conventions
	if v := labels["app.kubernetes.io/name"]; v != "" {
		return v
	}
	if v := labels["app"]; v != "" {
		return v
	}
	if v := labels["k8s-app"]; v != "" {
		return v
	}
	return ""
}
