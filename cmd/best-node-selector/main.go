package main

import (
	"context"
	"log"
	"time"

	"scheduler-extender/internal/app"
	"scheduler-extender/internal/config"
	"scheduler-extender/internal/redis"
	"scheduler-extender/internal/scorer"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()

	// ✅ Redis repository (single source of truth)
	repo := redis.NewDecisionRepository(
		cfg.RedisAddr,
		120*time.Second,
	)

	// ✅ Placement provider (Kubernetes state)
	placement := app.PlacementProvider()

	// ✅ Background scorer (slow path)
	sc := scorer.New(
		app.MetricsProvider(cfg),
		placement,
		repo,
	)

	// 🔁 Background scoring loop
	go scorer.Start(
		ctx,
		sc,
		placement,
		cfg.TimeWindowSeconds,
	)

	// 🚀 HTTP server (fast path)
	if err := app.New(cfg, repo).Run(); err != nil {
		log.Fatal(err)
	}
}
