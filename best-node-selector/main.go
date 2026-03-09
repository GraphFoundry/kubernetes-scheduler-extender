package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"best-node-selector/internal/config"
	"best-node-selector/internal/rebalancer"
	"best-node-selector/internal/redis"
	"best-node-selector/internal/scheduler"
	"best-node-selector/internal/scorer"
	"best-node-selector/internal/services"
	httptransport "best-node-selector/internal/transport/http"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	log.SetPrefix("[SCHEDULER] ")
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	config.Init(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ✅ Kubernetes client
	clientset, err := newKubernetesClient()
	if err != nil {
		log.Fatalf("failed to create kubernetes client: %v", err)
	}

	// ✅ Redis repository (coordinator)
	repo := redis.NewSchedulerRepository(cfg.RedisAddr)
	defer repo.Close()

	// ✅ Decision repository (for decisions)
	decisionRepo := redis.NewDecisionRepository(cfg.RedisAddr, 1*time.Hour)

	// ✅ Production-grade scheduler (with Redis-first decision support)
	sched := scheduler.New(repo, decisionRepo, clientset)

	// ✅ Metrics and placement providers
	metricsProvider := services.NewHTTPMetricsProvider(cfg.MetricsBaseURL, cfg.MetricsTimeout)
	placementProvider, err := services.NewK8sPlacementProvider()
	if err != nil {
		log.Fatalf("failed to create placement provider: %v", err)
	}

	// ✅ Initialize scorer with decision repository
	scorerInstance := scorer.New(metricsProvider, placementProvider, decisionRepo, cfg.TopKPeers)
	scorerInstance.SetPreferenceReader(repo) // ✅ Enable user preference lookups during scoring
	rebalancerController := rebalancer.New(cfg, repo, metricsProvider, clientset)

	log.Println("🚀 Production-grade scheduler initialized")
	log.Printf("   Redis: %s", cfg.RedisAddr)
	log.Printf("   Metrics: %s", cfg.MetricsBaseURL)
	log.Printf("   Score Updates: %v", cfg.EnableScoreUpdate)
	log.Println("   Features: optimistic locking, architecture-aware, API validation")

	// 🔁 Background: Sync node states from API to Redis
	go syncNodeStates(ctx, sched, clientset, 30*time.Second)

	// 🔁 Background: Score services and save decisions to Redis (optional)
	if cfg.EnableScoreUpdate {
		log.Println("✅ Starting background score update loop")
		go scorer.Start(ctx, scorerInstance, placementProvider, cfg.TimeWindowSeconds, cfg.WatchNamespaces, cfg.ScorerInterval)
	} else {
		log.Println("⏸️  Score updates disabled (ENABLE_SCORE_UPDATE=false)")
	}

	if cfg.RebalancingEnabled {
		log.Println("✅ Starting controlled rebalancing loop")
		go rebalancerController.Run(ctx)
	} else {
		log.Println("⏸️  Rebalancing disabled (REBALANCING_ENABLED=false)")
	}

	// 🌐 HTTP API
	api := httptransport.NewAPI(decisionRepo, sched, cfg.TopKPeers, cfg.TargetServiceID)
	api.SetClientset(clientset) // ✅ Enable pod restart functionality
	api.SetMetricsHandler(rebalancerController.MetricsText)
	api.SetRoundRobinCounter(repo) // ✅ Enable round-robin pod distribution
	api.SetPreferenceStore(repo)   // ✅ Enable user node preferences
	api.SetOverrideStore(repo)     // ✅ Enable change-node overrides
	api.SetChangeNodeLocker(repo)  // ✅ Enable distributed locking for change-node
	handlers := httptransport.Handlers{
		Health:         api.Health,
		Metrics:        api.Metrics,
		Prioritize:     api.Prioritize,
		List:           api.ListDecisions,
		RestartPod:     api.RestartPod,
		GetOptimalNode: api.GetOptimalNode,
		ChangeNode:     api.ChangeNode,
		SetPreference:  api.SetPreference,
		DelPreference:  api.DeletePreference,
		GetPreference:  api.GetPreference,
	}
	mux := httptransport.NewRouter(handlers)

	server := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: mux,
	}

	go func() {
		log.Printf("📡 Server listening on port %s", cfg.Port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("❌ Server failed: %v", err)
		}
	}()

	// 🛑 Graceful Shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("⚠️  Shutting down server...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("❌ Server forced to shutdown: %v", err)
	}

	log.Println("✅ Server exited")
}

// newKubernetesClient creates a Kubernetes clientset
func newKubernetesClient() (*kubernetes.Clientset, error) {
	// Try in-cluster config first
	config, err := rest.InClusterConfig()
	if err != nil {
		// Fall back to kubeconfig
		kubeconfig := filepath.Join(os.Getenv("HOME"), ".kube", "config")
		if envKube := os.Getenv("KUBECONFIG"); envKube != "" {
			kubeconfig = envKube
		}

		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, err
		}
	}

	return kubernetes.NewForConfig(config)
}

// syncNodeStates periodically syncs node states from API to Redis
func syncNodeStates(
	ctx context.Context,
	sched *scheduler.Scheduler,
	clientset *kubernetes.Clientset,
	interval time.Duration,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("📊 Node state sync started interval=%s", interval)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := syncNodes(ctx, sched, clientset); err != nil {
				log.Printf("❌ Node sync failed: %v", err)
			}
		}
	}
}

func syncNodes(ctx context.Context, sched *scheduler.Scheduler, clientset *kubernetes.Clientset) error {
	nodes, err := clientset.CoreV1().Nodes().List(ctx, v1.ListOptions{})
	if err != nil {
		return err
	}

	synced := 0
	for _, node := range nodes.Items {
		if err := sched.UpdateNodeStateFromAPI(ctx, &node); err != nil {
			log.Printf("⚠️  Failed to sync node %s: %v", node.Name, err)
			continue
		}
		synced++
	}

	return nil
}
