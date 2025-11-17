package httptransport

//
//import (
//	"encoding/json"
//	"log"
//	"net/http"
//	"time"
//
//	"scheduler-extender/internal/models"
//	"scheduler-extender/internal/services"
//)
//
//type API struct {
//	svc           *services.SchedulerService
//	topK          int
//	targetService string
//}
//
//func NewAPI(svc *services.SchedulerService, topK int, targetService string) *API {
//	return &API{svc: svc, topK: topK, targetService: targetService}
//}
//
//func (a *API) Health(w http.ResponseWriter, r *http.Request) {
//	writeJSON(w, map[string]any{"status": "ok"})
//}
//
//func (a *API) Prioritize(w http.ResponseWriter, r *http.Request) {
//
//	start := time.Now()
//
//	log.Printf(
//		"[HTTP][PRIORITIZE] request received from kube-scheduler method=%s path=%s remote=%s",
//		r.Method,
//		r.URL.Path,
//		r.RemoteAddr,
//	)
//
//	var args models.ExtenderArgs
//
//	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
//		log.Printf("[HTTP][PRIORITIZE][ERROR] failed to decode request body: %v", err)
//		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
//		return
//	}
//	if args.Pod != nil {
//		log.Printf(
//			"[HTTP][PRIORITIZE] pod namespace=%s name=%s uid=%s",
//			args.Pod.Namespace,
//			args.Pod.Name,
//			args.Pod.UID,
//		)
//	} else {
//		log.Printf("[HTTP][PRIORITIZE][WARN] request contains no pod information")
//	}
//
//	nodes := extractNodeNames(args)
//	log.Printf(
//		"[HTTP][PRIORITIZE] candidate nodes received count=%d",
//		len(nodes),
//	)
//
//	if len(nodes) == 0 {
//		log.Printf("[HTTP][PRIORITIZE] no candidate nodes provided, returning empty result")
//		writeJSON(w, []models.HostPriority{})
//		return
//	}
//
//	log.Printf(
//		"[HTTP][PRIORITIZE] invoking scheduler service targetService=%s topK=%d",
//		a.targetService,
//		a.topK,
//	)
//
//	out := a.svc.PrioritizeNodes(r.Context(), args.Pod, a.targetService, nodes, a.topK)
//
//	log.Printf(
//		"[HTTP][PRIORITIZE] scheduler service completed scoredNodes=%d duration_ms=%d",
//		len(out),
//		time.Since(start).Milliseconds(),
//	)
//
//	log.Printf("[HTTP][PRIORITIZE] sending response back to kube-scheduler")
//
//	writeJSON(w, out)
//}
//
//func extractNodeNames(args models.ExtenderArgs) []string {
//	if args.Nodes != nil && len(args.Nodes.Items) > 0 {
//		nodes := make([]string, 0, len(args.Nodes.Items))
//		for _, n := range args.Nodes.Items {
//			nodes = append(nodes, n.Metadata.Name)
//		}
//		return nodes
//	}
//	if args.NodeNames != nil {
//		return append([]string{}, (*args.NodeNames)...)
//	}
//	return nil
//}
//
//func writeJSON(w http.ResponseWriter, v any) {
//	w.Header().Set("Content-Type", "application/json")
//	enc := json.NewEncoder(w)
//	enc.SetIndent("", "  ")
//	_ = enc.Encode(v)
//}
