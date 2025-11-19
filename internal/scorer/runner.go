package scorer

import (
	"context"
	"log"
	"time"

	"scheduler-extender/internal/services"
)

const defaultInterval = 30 * time.Second

func Start(
	ctx context.Context,
	scorer *Scorer,
	placement services.PlacementProvider,
	windowSeconds int,
) {
	ticker := time.NewTicker(defaultInterval)
	defer ticker.Stop()

	log.Printf(
		"[SCORER] background scorer started interval=%s windowSeconds=%d",
		defaultInterval,
		windowSeconds,
	)

	for {
		select {
		case <-ticker.C:
			runOnce(ctx, scorer, placement, windowSeconds)

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
) {
	log.Printf("[SCORER] discovery cycle started")

	// 1️⃣ Build node → services index from Kubernetes
	nodeServiceIndex, err := placement.BuildNodeServiceIndex(ctx)
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
		"[SCORER] discovered services=%d nodes=%d",
		len(servicesSet),
		len(nodes),
	)

	// 4️⃣ Compute score for each service
	for service := range servicesSet {
		select {
		case <-ctx.Done():
			return
		default:
			scorer.ComputeForService(
				ctx,
				"default", // namespace resolution can be added later
				service,
				nodes,
				windowSeconds,
			)
		}
	}

	log.Printf("[SCORER] discovery cycle completed")
}
