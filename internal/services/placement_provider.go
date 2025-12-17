package services

import "context"

// PlacementProvider tells "which services are running on which nodes" right now.
type PlacementProvider interface {
	// Returns: nodeName -> set(serviceName)
	BuildNodeServiceIndex(ctx context.Context, namespaces []string) (map[string]map[string]struct{}, error)
}
