package scorer

import (
	"context"
	"log"
	"time"

	"best-node-selector/internal/services"
)

func Start(
	ctx context.Context,
	scorer *Scorer,
	placement services.PlacementProvider,
	windowSeconds int,
	namespaces []string,
	interval time.Duration,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf(
		"[SCORER] background scorer started interval=%s windowSeconds=%d namespaces=%v",
		interval,
		windowSeconds,
		namespaces,
	)

	for {
		select {
		case <-ticker.C:
			runOnce(ctx, scorer, placement, windowSeconds, namespaces)

		case <-ctx.Done():
			log.Printf("[SCORER] background scorer stopped")
			return
		}
	}
}

func runOnce(
	ctx context.Context,
	scorer *Scorer,
	placement services.PlacementProvider,
	windowSeconds int,
	namespaces []string,
) {
	log.Printf("[SCORER] discovery cycle started")

	// Default to "default" namespace if none specified
	if len(namespaces) == 0 {
		namespaces = []string{"default"}
	}

	// 1️⃣ Build node → services index from Kubernetes
	nodeServiceIndex, err := placement.BuildNodeServiceIndex(ctx, namespaces)
	if err != nil {
		log.Printf("[SCORER][ERROR] failed to build node-service index: %v", err)
		return
	}

	if len(nodeServiceIndex) == 0 {
		log.Printf("[SCORER][WARN] no scheduled pods with service labels found")
		return
	}

	// 2️⃣ Collect unique nodes
	nodes := make([]string, 0, len(nodeServiceIndex))
	for node := range nodeServiceIndex {
		nodes = append(nodes, node)
	}

	// 3️⃣ Collect unique services
	servicesSet := make(map[string]struct{})
	for _, svcSet := range nodeServiceIndex {
		for svc := range svcSet {
			servicesSet[svc] = struct{}{}
		}
	}

	if len(servicesSet) == 0 {
		log.Printf("[SCORER][WARN] no services discovered")
		return
	}

	log.Printf(
		"[SCORER] discovered services=%d nodes=%d namespaces=%v",
		len(servicesSet),
		len(nodes),
		namespaces,
	)

	// 4️⃣ Compute score for each service
	// Use first namespace as default for now (could be enhanced to track per-namespace)
	namespace := namespaces[0]
	for service := range servicesSet {
		select {
		case <-ctx.Done():
			return
		default:
			scorer.ComputeForService(
				ctx,
				namespace,
				service,
				nodes,
				windowSeconds,
			)
		}
	}

	log.Printf("[SCORER] discovery cycle completed")
}
