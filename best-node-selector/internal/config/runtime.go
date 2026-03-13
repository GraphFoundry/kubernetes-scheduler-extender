package config

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
)

var (
	mu      sync.RWMutex
	current Config
)

// Init sets the initial config after Load().
func Init(cfg Config) {
	mu.Lock()
	defer mu.Unlock()
	current = cfg
}

// Get returns the current config (thread-safe).
func Get() Config {
	mu.RLock()
	defer mu.RUnlock()
	return current
}

// ReloadFromFile reads a KEY=VALUE file and reloads config.
func ReloadFromFile(path string) error {
	return ReloadWithOverrides(path, nil)
}

// ReloadWithOverrides reads a KEY=VALUE file, applies env overrides on top
// (to handle kubelet ConfigMap sync delay), then reloads config.
func ReloadWithOverrides(path string, envOverrides map[string]string) error {
	mu.Lock()
	defer mu.Unlock()

	if err := loadEnvFile(path); err != nil {
		log.Printf("[CONFIG] Could not read runtime config file (may not exist yet): %v", err)
	}

	for k, v := range envOverrides {
		os.Setenv(k, v)
	}

	cfg, err := Load()
	if err != nil {
		return fmt.Errorf("failed to reload config: %w", err)
	}

	current = cfg
	log.Printf("[CONFIG] Runtime config reloaded from %s (overrides=%d)", path, len(envOverrides))
	return nil
}

func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		os.Setenv(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
	}
	return scanner.Err()
}
