package httptransport

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"scheduler-extender/internal/models"
)

// DecisionReader matches app.DecisionReader
type DecisionReader interface {
	Get(ctx context.Context, namespace, service string) (*models.Decision, error)
	List(ctx context.Context, namespace string) ([]*models.Decision, error)
}

type API struct {
	repo          DecisionReader
	topK          int
	targetService string
}

func NewAPI(repo DecisionReader, topK int, targetService string) *API {
	return &API{
		repo:          repo,
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

	nodes := extractNodeNames(args)
	if len(nodes) == 0 {
		log.Printf("[HTTP][PRIORITIZE] no nodes provided")
		writeJSON(w, []models.HostPriority{})
		return
	}

	namespace := "default"
	if args.Pod != nil && args.Pod.Namespace != "" {
		namespace = args.Pod.Namespace
	}

	service := ""

	if args.Pod != nil {
		if v, ok := args.Pod.Labels["extender.kubernetes.io/name"]; ok {
			service = v
		}
	}

	if service == "" {
		log.Printf("[HTTP][PRIORITIZE][WARN] service label missing, using neutral scores")
		writeJSON(w, neutral(nodes))
		return
	}

	log.Printf(
		"[HTTP][PRIORITIZE] resolving decision namespace=%s service=%s nodes=%d",
		namespace,
		service,
		len(nodes),
	)

	decision, err := a.repo.Get(r.Context(), namespace, service)
	if err != nil {
		log.Printf("[HTTP][PRIORITIZE][WARN] decision not found, using neutral scores error=%v", err)
		writeJSON(w, neutral(nodes))
		return
	}

	out := scoresFromDecision(nodes, decision)

	log.Printf(
		"[HTTP][PRIORITIZE] response sent scoredNodes=%d duration_ms=%d",
		len(out),
		time.Since(start).Milliseconds(),
	)

	writeJSON(w, out)
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
	if args.Nodes != nil && len(args.Nodes.Items) > 0 {
		nodes := make([]string, 0, len(args.Nodes.Items))
		for _, n := range args.Nodes.Items {
			nodes = append(nodes, n.Metadata.Name)
			log.Printf(
				"[API][DEBUG] node=%s ", n.Metadata.Name)
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
