package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type ProviderType string

const (
	ProviderSample ProviderType = "sample"
	ProviderHTTP   ProviderType = "http"
)

type Config struct {
	Port string

	MetricsProvider ProviderType

	// Leader endpoint settings (used later)
	MetricsBaseURL string
	MetricsTimeout time.Duration

	// Algorithm tuning
	TimeWindowSeconds int
	TopKPeers         int

	// Target service id for now (later you can derive from pod labels/annotations)
	TargetServiceID string

	//Redis
	RedisAddr string
}

func Load() (Config, error) {
	cfg := Config{
		Port:              getEnv("PORT", "9000"),
		MetricsProvider:   ProviderType(getEnv("METRICS_PROVIDER", string(ProviderHTTP))),
		MetricsBaseURL:    getEnv("METRICS_BASE_URL", "http://service-graph-engine.default.svc.cluster.local:3000"),
		MetricsTimeout:    getDurationMs("METRICS_TIMEOUT_MS", 1200),
		TimeWindowSeconds: getInt("TIME_WINDOW_SECONDS", 300),
		TopKPeers:         getInt("TOPK_PEERS", 5),
		TargetServiceID:   getEnv("TARGET_SERVICE_ID", "default:checkoutservice"),
		RedisAddr:         getEnv("REDIS_ADDR", "redis.default.svc.cluster.local:6379"),
	}

	if cfg.MetricsProvider != ProviderSample && cfg.MetricsProvider != ProviderHTTP {
		return Config{}, fmt.Errorf("invalid METRICS_PROVIDER: %s", cfg.MetricsProvider)
	}

	return cfg, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
func getInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
func getDurationMs(key string, defMs int) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return time.Duration(defMs) * time.Millisecond
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return time.Duration(defMs) * time.Millisecond
	}
	return time.Duration(n) * time.Millisecond
}
