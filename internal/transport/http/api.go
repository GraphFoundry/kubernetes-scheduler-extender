package httptransport

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"scheduler-extender/internal/models"
	"scheduler-extender/internal/scheduler"
)

// DecisionReader matches app.DecisionReader
type DecisionReader interface {
	Get(ctx context.Context, namespace, service string) (*models.Decision, error)
	List(ctx context.Context, namespace string) ([]*models.Decision, error)
}

type API struct {
	repo          DecisionReader
	scheduler     *scheduler.Scheduler
	topK          int
	targetService string
}

func NewAPI(repo DecisionReader, sched *scheduler.Scheduler, topK int, targetService string) *API {
	return &API{
		repo:          repo,
		scheduler:     sched,
		topK:          topK,
		targetService: targetService,
	}
}

func (a *API) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"status": "ok"})
}

func (a *API) Prioritize(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	log.Printf(
		"[HTTP][PRIORITIZE] request received method=%s remote=%s",
		r.Method,
		r.RemoteAddr,
	)

	var args models.ExtenderArgs
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		log.Printf("[HTTP][PRIORITIZE][ERROR] decode failed error=%v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	log.Printf("[HTTP][PRIORITIZE] decoded pod=%s/%s nodes=%d",
		args.Pod.Namespace, args.Pod.Name, len(args.Nodes))

	nodes := extractNodeNames(args)
	if len(nodes) == 0 {
		log.Printf("[HTTP][PRIORITIZE] no nodes provided")
		writeJSON(w, []models.HostPriority{})
		return
	}

	// 1. Try to find a precomputed decision for this service
	namespace := args.Pod.Namespace
	if namespace == "" {
		namespace = "default"
	}

	service := ""
	if v, ok := args.Pod.Labels["extender.kubernetes.io/name"]; ok {
		service = v
	}

	// Only look for decision if we have a service name
	if service != "" {
		log.Printf(
			"[HTTP][PRIORITIZE] resolving decision namespace=%s service=%s nodes=%d",
			namespace,
			service,
			len(nodes),
		)

		decision, err := a.repo.Get(r.Context(), namespace, service)
		if err == nil {
			// Found a decision! Use it.
			out := scoresFromDecision(nodes, decision)
			log.Printf(
				"[HTTP][PRIORITIZE] decision found! response sent scoredNodes=%d duration_ms=%d",
				len(out),
				time.Since(start).Milliseconds(),
			)
			writeJSON(w, out)
			return
		}

		log.Printf("[HTTP][PRIORITIZE] decision not found (err=%v), falling back to scheduler", err)
	} else {
		log.Printf("[HTTP][PRIORITIZE] service label missing, skipping decision lookup")
	}

	// 2. Fallback to production-grade scheduler
	if a.scheduler != nil {
		nodeName, err := a.scheduler.Schedule(r.Context(), &args.Pod, args.Nodes)
		if err != nil {
			log.Printf("[HTTP][PRIORITIZE][ERROR] scheduling failed: %v", err)
			// Fall back to neutral scores
			writeJSON(w, neutral(nodes))
			return
		}

		// Return scores with selected node having highest score
		scores := generateScoresForNode(nodes, nodeName)
		log.Printf("[HTTP][PRIORITIZE] scheduled to node=%s duration_ms=%d",
			nodeName, time.Since(start).Milliseconds())
		writeJSON(w, scores)
		return
	}

	// 3. Final Fallback (if no scheduler and no decision)
	writeJSON(w, neutral(nodes))
}

func (a *API) ListDecisions(w http.ResponseWriter, r *http.Request) {
	namespace := r.URL.Query().Get("namespace")
	if namespace == "" {
		namespace = "default"
	}

	decisions, err := a.repo.List(r.Context(), namespace)
	if err != nil {
		http.Error(w, "failed to load decisions", http.StatusInternalServerError)
		return
	}

	writeJSON(w, decisions)
}

/* ---------------- helpers ---------------- */

func scoresFromDecision(nodes []string, d *models.Decision) []models.HostPriority {
	out := make([]models.HostPriority, 0, len(nodes))

	for _, n := range nodes {
		score := 50 // fallback
		if v, ok := d.Scores[n]; ok {
			score = v
		}
		out = append(out, models.HostPriority{
			Host:  n,
			Score: score,
		})
	}

	return out
}

func neutral(nodes []string) []models.HostPriority {
	out := make([]models.HostPriority, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, models.HostPriority{
			Host:  n,
			Score: 50,
		})
	}
	return out
}

func extractNodeNames(args models.ExtenderArgs) []string {
	if len(args.Nodes) > 0 {
		nodes := make([]string, 0, len(args.Nodes))
		for _, n := range args.Nodes {
			nodes = append(nodes, n.Name)
		}
		return nodes
	}
	if args.NodeNames != nil {
		return append([]string{}, (*args.NodeNames)...)
	}
	return nil
}

// generateScoresForNode creates scores with selected node getting 100, others getting lower scores
func generateScoresForNode(allNodes []string, selectedNode string) []models.HostPriority {
	out := make([]models.HostPriority, 0, len(allNodes))
	for _, n := range allNodes {
		score := 50 // default score
		if n == selectedNode {
			score = 100 // selected node gets highest score
		}
		out = append(out, models.HostPriority{
			Host:  n,
			Score: score,
		})
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
