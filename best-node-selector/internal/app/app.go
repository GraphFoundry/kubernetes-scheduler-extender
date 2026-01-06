package app

import (
	"context"
	"log"
	"net/http"
	"best-node-selector/internal/config"
	"best-node-selector/internal/models"
	"best-node-selector/internal/scheduler"
	httptransport "best-node-selector/internal/transport/http"
	"time"
)

type App struct {
	cfg       config.Config
	repo      DecisionReader
	scheduler *scheduler.Scheduler
}

type DecisionReader interface {
	Get(ctx context.Context, namespace, service string) (*models.Decision, error)
	List(ctx context.Context, namespace string) ([]*models.Decision, error)
}

func New(cfg config.Config, repo DecisionReader) *App {
	return &App{
		cfg:  cfg,
		repo: repo,
	}
}

func NewWithScheduler(cfg config.Config, repo DecisionReader, sched *scheduler.Scheduler) *App {
	return &App{
		cfg:       cfg,
		repo:      repo,
		scheduler: sched,
	}
}

func (a *App) Run() error {
	api := httptransport.NewAPI(
		a.repo,
		a.scheduler, // May be nil for legacy mode
		a.cfg.TopKPeers,
		a.cfg.TargetServiceID,
	)

	router := httptransport.NewRouter(httptransport.Handlers{
		Health:     api.Health,
		Prioritize: api.Prioritize,
		List:       api.ListDecisions,
	})

	server := &http.Server{
		Addr:              ":" + a.cfg.Port,
		Handler:           router,
		ReadHeaderTimeout: 3 * time.Second,
	}

	if a.scheduler != nil {
		log.Printf("[APP] scheduler-extender listening on :%s (mode=production-grade)", a.cfg.Port)
	} else {
		log.Printf("[APP] scheduler-extender listening on :%s (mode=legacy)", a.cfg.Port)
	}
	
	return server.ListenAndServe()
}

//func (a *App) Run() error {
//	var provider services.MetricsProvider
//
//	switch a.cfg.MetricsProvider {
//	case config.ProviderSample:
//		provider = services.NewSampleMetricsProvider()
//	case config.ProviderHTTP:
//		provider = services.NewHTTPMetricsProvider(a.cfg.MetricsBaseURL, a.cfg.MetricsTimeout)
//	default:
//		provider = services.NewSampleMetricsProvider()
//	}
//
//	placement, err := services.NewK8sPlacementProvider()
//	if err != nil {
//		log.Fatal(err)
//	}
//
//	svc := services.NewSchedulerService(provider, placement)
//
//	api := httptransport.NewAPI(svc, a.cfg.TopKPeers, a.cfg.TargetServiceID)
//
//	router := httptransport.NewRouter(httptransport.Handlers{
//		Health:     api.Health,
//		Prioritize: api.Prioritize,
//	})
//
//	server := &http.Server{
//		Addr:              ":" + a.cfg.Port,
//		Handler:           router,
//		ReadHeaderTimeout: 3 * time.Second,
//	}
//
//	log.Printf("scheduler-extender listening on :%s (provider=%s)", a.cfg.Port, a.cfg.MetricsProvider)
//	return server.ListenAndServe()
//}
