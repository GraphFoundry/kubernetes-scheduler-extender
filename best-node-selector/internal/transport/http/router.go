package httptransport

import (
	"encoding/json"
	"log"
	"net/http"

	"best-node-selector/internal/config"
)

type Handlers struct {
	Health         http.HandlerFunc
	Metrics        http.HandlerFunc
	Prioritize     http.HandlerFunc
	List           http.HandlerFunc
	RestartPod     http.HandlerFunc
	GetOptimalNode http.HandlerFunc
	ChangeNode     http.HandlerFunc
	SetPreference  http.HandlerFunc
	DelPreference  http.HandlerFunc
	GetPreference  http.HandlerFunc
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			origin = "*"
		}

		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Request-Id, X-Correlation-Id, Authorization")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Max-Age", "3600")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func NewRouter(h Handlers) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", h.Health)
	mux.HandleFunc("/metrics", h.Metrics)
	mux.HandleFunc("/prioritize", h.Prioritize)
	mux.HandleFunc("/decisions", h.List)
	mux.HandleFunc("/restart", h.RestartPod)
	mux.HandleFunc("/optimal", h.GetOptimalNode)
	mux.HandleFunc("/change-node", h.ChangeNode)

	// Preference: multiplex GET / POST / DELETE on a single path
	mux.HandleFunc("/preference", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			h.GetPreference(w, r)
		case http.MethodPost:
			h.SetPreference(w, r)
		case http.MethodDelete:
			h.DelPreference(w, r)
		case http.MethodOptions:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Runtime config reload endpoint
	mux.HandleFunc("POST /admin/reload-config", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Env map[string]string `json:"env"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if err := config.ReloadWithOverrides("/etc/runtime-config/runtime.env", body.Env); err != nil {
			log.Printf("[CONFIG] Reload failed: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"status":"error","message":"` + err.Error() + `"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"reloaded"}`))
	})

	return corsMiddleware(mux)
}
