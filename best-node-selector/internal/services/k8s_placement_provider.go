package services

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"

	v1 "k8s.io/api/core/v1"
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
			svc = strings.ToLower(svc)

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

// GetNodeLabels returns labels for all nodes in the cluster
func (p *K8sPlacementProvider) GetNodeLabels(ctx context.Context) (map[string]map[string]string, error) {
	nodes, err := p.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	result := make(map[string]map[string]string)
	for _, node := range nodes.Items {
		result[node.Name] = node.Labels
	}
	return result, nil
}

func (p *K8sPlacementProvider) GetNodeRuntime(ctx context.Context, namespaces []string) (map[string]NodeRuntime, error) {
	nodes, err := p.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	runtime := make(map[string]NodeRuntime, len(nodes.Items))
	for _, node := range nodes.Items {
		allocCPU := node.Status.Allocatable.Cpu().MilliValue()
		allocMem := node.Status.Allocatable.Memory().Value()

		ready := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == "Ready" {
				ready = string(condition.Status) == "True"
				break
			}
		}

		var taints []NodeTaint
		for _, t := range node.Spec.Taints {
			taints = append(taints, NodeTaint{
				Key:    t.Key,
				Effect: string(t.Effect),
			})
		}

		runtime[node.Name] = NodeRuntime{
			Name:             node.Name,
			Ready:            ready,
			Schedulable:      !node.Spec.Unschedulable,
			AllocCPUMilli:    allocCPU,
			AllocMemBytes:    allocMem,
			Services:         map[string]struct{}{},
			PodsByService:    map[string]int{},
			ServiceResources: map[string][]PodResourceRequest{},
			Taints:           taints,
		}
	}

	if len(namespaces) == 0 {
		namespaces = []string{""}
	}

	for _, ns := range namespaces {
		pods, err := p.clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			log.Printf("[PLACEMENT][K8S][WARN] GetNodeRuntime pods list failed namespace=%s error=%v", ns, err)
			continue
		}

		for _, pod := range pods.Items {
			nodeName := pod.Spec.NodeName
			if nodeName == "" {
				continue
			}

			nr, ok := runtime[nodeName]
			if !ok {
				continue
			}

			if strings.EqualFold(string(pod.Status.Phase), "Succeeded") || strings.EqualFold(string(pod.Status.Phase), "Failed") {
				continue
			}

			podCPU, podMem := podResourceRequest(&pod)
			nr.UsedCPUMilli += podCPU
			nr.UsedMemBytes += podMem

			svc := serviceNameFromLabels(pod.Labels)
			if svc != "" {
				svc = strings.ToLower(svc)
				nr.Services[svc] = struct{}{}
				nr.PodsByService[svc] = nr.PodsByService[svc] + 1
				nr.ServiceResources[svc] = append(nr.ServiceResources[svc], PodResourceRequest{CPUMilli: podCPU, MemBytes: podMem})
			}

			runtime[nodeName] = nr
		}
	}

	return runtime, nil
}

func podResourceRequest(pod *v1.Pod) (int64, int64) {
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

	return cpuMillis, memBytes
}

func serviceNameFromLabels(labels map[string]string) string {
	// Try standard app label
	if v := labels["app"]; v != "" {
		return v
	}
	// Try app.kubernetes.io/name
	if v := labels["app.kubernetes.io/name"]; v != "" {
		return v
	}
	// Try Istio canonical name
	if v := labels["service.istio.io/canonical-name"]; v != "" {
		return v
	}
	return ""
}
