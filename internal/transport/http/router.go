package httptransport

import "net/http"

type Handlers struct {
	Health     http.HandlerFunc
	Filter     http.HandlerFunc
	Prioritize http.HandlerFunc
}

func NewRouter(h Handlers) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", h.Health)
	mux.HandleFunc("/filter", h.Filter)
	mux.HandleFunc("/prioritize", h.Prioritize)
	return mux
}
