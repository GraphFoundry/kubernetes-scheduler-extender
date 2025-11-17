package app

import (
	"log"
	"scheduler-extender/internal/config"

	//"scheduler-extender/testdata"

	"scheduler-extender/internal/services"
)

/*
MetricsProvider factory
------------------------
ONLY real providers are allowed in production.
*/
func MetricsProvider(cfg config.Config) services.MetricsProvider {
	if cfg.MetricsProvider != config.ProviderHTTP {
		log.Fatal("[APP][ERROR] sample metrics provider is not allowed in production")
	}

	return services.NewHTTPMetricsProvider(
		cfg.MetricsBaseURL,
		cfg.MetricsTimeout,
	)
}

/*
MetricsProvider factory
------------------------
Creates the correct MetricsProvider based on config.
This keeps main.go clean and prevents leaking service construction logic.

func MetricsProvider(cfg config.Config) services.MetricsProvider {
	switch cfg.MetricsProvider {
	case config.ProviderHTTP:
		return services.NewHTTPMetricsProvider(
			cfg.MetricsBaseURL,
			cfg.MetricsTimeout,
		)
	case config.ProviderSample:
		fallthrough
	default:
		return testdata.NewSampleMetricsProvider()
	}
}
*/

/*
PlacementProvider factory
-------------------------
Creates the Kubernetes placement provider.
If this fails, the process cannot continue.
*/
func PlacementProvider() services.PlacementProvider {
	p, err := services.NewK8sPlacementProvider()
	if err != nil {
		log.Fatalf("[APP][ERROR] failed to create K8sPlacementProvider: %v", err)
	}
	return p
}
