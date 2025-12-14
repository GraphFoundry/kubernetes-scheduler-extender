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

	// Namespaces to watch for scheduling decisions (comma-separated)
	WatchNamespaces []string

	//Redis
	RedisAddr string
}

func Load() (Config, error) {
	cfg := Config{
		Port:              getEnv("PORT", "9000"),
		MetricsProvider:   ProviderType(getEnv("METRICS_PROVIDER", string(ProviderHTTP))),
		MetricsBaseURL:    getEnv("METRICS_BASE_URL", "http://localhost:3000"),
		MetricsTimeout:    getDurationMs("METRICS_TIMEOUT_MS", 1200),
		TimeWindowSeconds: getInt("TIME_WINDOW_SECONDS", 300),
		TopKPeers:         getInt("TOPK_PEERS", 5),
		TargetServiceID:   getEnv("TARGET_SERVICE_ID", "default:checkoutservice"),
		WatchNamespaces:   getStringSlice("WATCH_NAMESPACES", "default"),
		RedisAddr:         getEnv("REDIS_ADDR", "localhost:6379"),
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

func getStringSlice(key, def string) []string {
	v := os.Getenv(key)
	if v == "" {
		v = def
	}
	if v == "" {
		return []string{}
	}
	result := []string{}
	for _, ns := range splitAndTrim(v, ",") {
		if ns != "" {
			result = append(result, ns)
		}
	}
	return result
}

func splitAndTrim(s, sep string) []string {
	parts := []string{}
	for _, part := range splitString(s, sep) {
		trimmed := trimSpace(part)
		if trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return parts
}

func splitString(s, sep string) []string {
	if s == "" {
		return []string{}
	}
	result := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		if i < len(s)-len(sep)+1 && s[i:i+len(sep)] == sep {
			result = append(result, s[start:i])
			start = i + len(sep)
			i += len(sep) - 1
		}
	}
	result = append(result, s[start:])
	return result
}

func trimSpace(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for start < end && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}
