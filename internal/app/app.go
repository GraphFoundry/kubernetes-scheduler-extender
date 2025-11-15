package app

import (
	"log"
	"net/http"
	"time"

	"scheduler-extender/internal/config"
	"scheduler-extender/internal/services"
	httptransport "scheduler-extender/internal/transport/http"
)

type App struct {
	cfg config.Config
}

func New(cfg config.Config) *App {
	return &App{cfg: cfg}
}

func (a *App) Run() error {
	var provider services.MetricsProvider

	switch a.cfg.MetricsProvider {
	case config.ProviderSample:
		provider = services.NewSampleMetricsProvider()
	case config.ProviderHTTP:
		provider = services.NewHTTPMetricsProvider(a.cfg.MetricsBaseURL, a.cfg.MetricsTimeout)
	default:
		provider = services.NewSampleMetricsProvider()
	}

	placement, err := services.NewK8sPlacementProvider()
	if err != nil {
		log.Fatal(err)
	}

	svc := services.NewSchedulerService(provider, placement)

	api := httptransport.NewAPI(svc, a.cfg.TopKPeers, a.cfg.TargetServiceID)

	router := httptransport.NewRouter(httptransport.Handlers{
		Health:     api.Health,
		Filter:     api.Filter,
		Prioritize: api.Prioritize,
	})

	server := &http.Server{
		Addr:              ":" + a.cfg.Port,
		Handler:           router,
		ReadHeaderTimeout: 3 * time.Second,
	}

	log.Printf("scheduler-extender listening on :%s (provider=%s)", a.cfg.Port, a.cfg.MetricsProvider)
	return server.ListenAndServe()
}
