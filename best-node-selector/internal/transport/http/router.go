package httptransport

import "net/http"

type Handlers struct {
	Health         http.HandlerFunc
	Prioritize     http.HandlerFunc
	List           http.HandlerFunc
	RestartPod     http.HandlerFunc
	GetOptimalNode http.HandlerFunc
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
	mux.HandleFunc("/prioritize", h.Prioritize)
	mux.HandleFunc("/decisions", h.List)
	mux.HandleFunc("/restart", h.RestartPod)
	mux.HandleFunc("/optimal", h.GetOptimalNode)

	return corsMiddleware(mux)
}
