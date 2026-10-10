package dashboard

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/gateway"
	"github.com/omniswitch-dev/omniswitch/internal/provider"
	"github.com/omniswitch-dev/omniswitch/internal/router"
)

// ReplicaHealth is populated only by the inference health source. Healthy and
// LastCheck stay nullable until a real probe has completed.
type ReplicaHealth struct {
	URL       string     `json:"url"`
	Runtime   string     `json:"runtime"`
	Healthy   *bool      `json:"healthy"`
	LastCheck *time.Time `json:"last_check"`
}

func (h *Handler) SetProviderInspector(registry *provider.Registry, rtr *router.Router, types map[string]string) {
	h.registry = registry
	h.router = rtr
	h.providerTypes = make(map[string]string, len(types))
	for name, kind := range types {
		h.providerTypes[strings.ToLower(name)] = kind
	}
}

// SetReplicaHealthSource is an optional integration hook for P2c's
// InferenceBackend.ReplicaHealth. A nil source never fabricates probe data.
func (h *Handler) SetReplicaHealthSource(source func(string) []ReplicaHealth) {
	h.replicaHealth = source
}

type providerHealth struct {
	Name         string                 `json:"name"`
	Type         string                 `json:"type"`
	Circuit      router.CircuitSnapshot `json:"circuit"`
	Requests     int                    `json:"requests"`
	AvgLatencyMs float64                `json:"avg_latency_ms"`
	ErrorRate    float64                `json:"error_rate"`
	Cost         float64                `json:"cost"`
	Replicas     []ReplicaHealth        `json:"replicas,omitempty"`
}

func (h *Handler) getProviderHealth(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	_, since, _, err := timeWindow(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	metrics, err := h.store.GetProviderMetricsForAPIKey(r.Context(), since, scopedAPIKeyID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read provider health")
		return
	}
	out := []providerHealth{}
	if h.registry != nil {
		circuits := map[string]router.CircuitSnapshot{}
		if h.router != nil {
			circuits = h.router.CircuitSnapshot()
		}
		names := h.registry.Names()
		sort.Strings(names)
		for _, name := range names {
			backend, err := h.registry.Get(name)
			if err != nil {
				continue
			}
			circuit, exists := circuits[name]
			if !exists {
				circuit = router.CircuitSnapshot{State: router.CircuitClosed}
			}
			kind := h.providerTypes[name]
			if kind == "" {
				kind = concreteProviderType(backend)
			}
			entry := providerHealth{Name: name, Type: kind, Circuit: circuit}
			for _, metric := range metrics {
				if strings.EqualFold(metric.Provider, name) {
					entry.Requests, entry.AvgLatencyMs = metric.RequestCount, metric.AvgLatencyMs
					entry.ErrorRate, entry.Cost = metric.ErrorRate, metric.TotalCost
					break
				}
			}
			if kind == "inference" && h.replicaHealth != nil {
				entry.Replicas = h.replicaHealth(name)
			}
			out = append(out, entry)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
}

func concreteProviderType(backend provider.Provider) string {
	switch backend.(type) {
	case *provider.OpenAI:
		return "openai"
	case *provider.AzureOpenAI:
		return "azure"
	case *provider.Anthropic:
		return "anthropic"
	case *provider.Google:
		return "google"
	case *provider.Groq:
		return "groq"
	case *provider.Bedrock:
		return "bedrock"
	case *provider.Cohere:
		return "cohere"
	case *provider.Custom:
		return "custom"
	case *provider.InferenceBackend:
		return "inference"
	case *provider.AliasProvider:
		return "alias"
	default:
		return "unknown"
	}
}

func (h *Handler) resetProviderCircuit(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !gateway.IsAdmin(r) {
		writeError(w, http.StatusForbidden, "circuit reset requires admin role")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/providers/")
	name, suffix, found := strings.Cut(path, "/")
	if !found || name == "" || suffix != "circuit/reset" || h.router == nil {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	if err := h.router.ResetCircuit(name); err != nil {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": strings.ToLower(name), "circuit": map[string]string{"state": "closed"}})
}
