package services

import "context"

type PodResourceRequest struct {
	CPUMilli int64
	MemBytes int64
}

type NodeRuntime struct {
	Name             string
	Ready            bool
	Schedulable      bool
	AllocCPUMilli    int64
	AllocMemBytes    int64
	UsedCPUMilli     int64
	UsedMemBytes     int64
	Services         map[string]struct{}
	PodsByService    map[string]int
	ServiceResources map[string][]PodResourceRequest
}

// PlacementProvider tells "which services are running on which nodes" right now.
type PlacementProvider interface {
	// Returns: nodeName -> set(serviceName)
	BuildNodeServiceIndex(ctx context.Context, namespaces []string) (map[string]map[string]struct{}, error)
	// Returns: nodeName -> labels
	GetNodeLabels(ctx context.Context) (map[string]map[string]string, error)
	// Returns complete node placement/runtime data used by scorer.
	GetNodeRuntime(ctx context.Context, namespaces []string) (map[string]NodeRuntime, error)
}
