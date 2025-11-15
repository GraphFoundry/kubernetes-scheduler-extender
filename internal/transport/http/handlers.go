package httptransport

import (
	"encoding/json"
	"log"
	"net/http"

	"scheduler-extender/internal/models"
	"scheduler-extender/internal/services"
)

type API struct {
	svc           *services.SchedulerService
	topK          int
	targetService string
}

func NewAPI(svc *services.SchedulerService, topK int, targetService string) *API {
	return &API{svc: svc, topK: topK, targetService: targetService}
}

func (a *API) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"status": "ok"})
}

func (a *API) Filter(w http.ResponseWriter, r *http.Request) {
	// Pass-through now: don’t block scheduling while you’re integrating.
	var args models.ExtenderArgs
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	res := models.ExtenderFilterResult{
		Nodes:       args.Nodes,
		NodeNames:   args.NodeNames,
		FailedNodes: map[string]string{},
	}
	writeJSON(w, res)
}

func (a *API) Prioritize(w http.ResponseWriter, r *http.Request) {
	var args models.ExtenderArgs
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	nodes := extractNodeNames(args)
	if len(nodes) == 0 {
		writeJSON(w, []models.HostPriority{})
		return
	}

	out := a.svc.PrioritizeNodes(r.Context(), args.Pod, a.targetService, nodes, a.topK)
	log.Printf("prioritize called nodes=%d", len(nodes))
	writeJSON(w, out)
}

func extractNodeNames(args models.ExtenderArgs) []string {
	if args.Nodes != nil && len(args.Nodes.Items) > 0 {
		nodes := make([]string, 0, len(args.Nodes.Items))
		for _, n := range args.Nodes.Items {
			nodes = append(nodes, n.Metadata.Name)
		}
		return nodes
	}
	if args.NodeNames != nil {
		return append([]string{}, (*args.NodeNames)...)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
