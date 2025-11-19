package app

import (
	"context"
	"log"
	"net/http"
	"scheduler-extender/internal/config"
	"scheduler-extender/internal/models"
	httptransport "scheduler-extender/internal/transport/http"
	"time"
)

type App struct {
	cfg  config.Config
	repo DecisionReader
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

func (a *App) Run() error {
	api := httptransport.NewAPI(
		a.repo,
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

	log.Printf("[APP] scheduler-extender listening on :%s", a.cfg.Port)
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
