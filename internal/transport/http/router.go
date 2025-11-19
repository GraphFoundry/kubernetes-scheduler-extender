package httptransport

import "net/http"

type Handlers struct {
	Health     http.HandlerFunc
	Prioritize http.HandlerFunc
	List       http.HandlerFunc
}

func NewRouter(h Handlers) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", h.Health)
	mux.HandleFunc("/prioritize", h.Prioritize)
	mux.HandleFunc("/decisions", h.List)

	return mux
}
