package dashboard

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/gateway"
	"github.com/omniswitch-dev/omniswitch/internal/gatewayconfig"
	"github.com/omniswitch-dev/omniswitch/internal/provider"
	mcpproxy "github.com/omniswitch-dev/omniswitch/internal/proxy"
	"github.com/omniswitch-dev/omniswitch/internal/router"
	"github.com/omniswitch-dev/omniswitch/internal/store"
)

//go:embed static
var staticFiles embed.FS

// Handler serves the dashboard API and embedded web UI.
type Handler struct {
	store             *store.Store
	configPath        string
	sessionInfo       SessionInfo
	logPayloadsSource func() bool
	registry          *provider.Registry
	router            *router.Router
	providerTypes     map[string]string
	replicaHealth     func(string) []ReplicaHealth
	mcp               *mcpproxy.Handler
	mcpEnabled        bool
	mcpTargets        []MCPConfiguredTarget
}

// New creates a new dashboard handler.
func New(st *store.Store) *Handler {
	return &Handler{store: st}
}

func (h *Handler) SetConfigPath(path string) {
	h.configPath = strings.TrimSpace(path)
}

// RegisterRoutes adds dashboard routes to the given mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// Dashboard API endpoints.
	mux.HandleFunc("/api/session", h.getSession)
	mux.HandleFunc("/api/logs/facets", h.getLogFacets)
	mux.HandleFunc("/api/logs/export", h.exportLogs)
	mux.HandleFunc("/api/metrics/timeseries", h.getTimeseries)
	mux.HandleFunc("/api/metrics/latency", h.getLatency)
	mux.HandleFunc("/api/guardrails/events", h.getGuardrailEvents)
	mux.HandleFunc("/api/guardrails/summary", h.getGuardrailSummary)
	mux.HandleFunc("/api/keys/usage", h.getKeyUsage)
	mux.HandleFunc("/api/mcp/targets", h.getMCPTargets)
	mux.HandleFunc("/api/mcp/tools", h.getMCPTools)
	mux.HandleFunc("/api/mcp/decisions", h.getMCPDecisions)
	mux.HandleFunc("/api/providers/health", h.getProviderHealth)
	mux.HandleFunc("/api/providers/", h.resetProviderCircuit)
	mux.HandleFunc("/api/config/validate", h.validateConfig)
	mux.HandleFunc("/api/logs", h.getLogs)
	mux.HandleFunc("/api/metrics", h.getMetrics)
	mux.HandleFunc("/api/metrics/providers", h.getProviderMetrics)
	mux.HandleFunc("/api/analytics/cost", h.getCostAnalytics)
	mux.HandleFunc("/api/traces/", h.getTrace)
	mux.HandleFunc("/api/config/raw", h.rawConfig)
	mux.HandleFunc("/api/configs", h.configs)
	mux.HandleFunc("/api/health", h.health)

	// Serve embedded static files at root.
	staticFS, _ := fs.Sub(staticFiles, "static")
	fileServer := http.FileServer(http.FS(staticFS))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if isDashboardRoute(r.URL.Path) {
			serveDashboardIndex(w, r)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

// RegisterPrometheus registers a lightweight Prometheus text endpoint. It is
// kept separate from RegisterRoutes so operators can explicitly disable it.
func (h *Handler) RegisterPrometheus(mux *http.ServeMux) {
	mux.HandleFunc("/metrics", h.prometheus)
}

func (h *Handler) getCostAnalytics(w http.ResponseWriter, r *http.Request) {
	since := windowStart(r.URL.Query().Get("window"))
	groupBy := r.URL.Query().Get("groupBy")
	var keyID string
	if scoped := scopedAPIKeyID(r); scoped != "" {
		keyID = scoped
	}
	rows, err := h.store.GetCostAnalytics(r.Context(), since, groupBy, keyID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if rows == nil {
		rows = []store.CostAnalyticsRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"groupBy": firstNonEmpty(groupBy, "day"), "rows": rows})
}

func (h *Handler) configs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ref := strings.TrimSpace(r.URL.Query().Get("id"))
		if ref == "" {
			ref = strings.TrimSpace(r.URL.Query().Get("name"))
		}
		if ref != "" {
			record, err := h.store.GetGatewayConfig(r.Context(), ref)
			if err != nil {
				writeError(w, http.StatusNotFound, "config not found")
				return
			}
			writeJSON(w, http.StatusOK, record)
			return
		}
		includeDisabled, _ := strconv.ParseBool(r.URL.Query().Get("include_disabled"))
		records, err := h.store.ListGatewayConfigs(r.Context(), includeDisabled)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if records == nil {
			records = []store.GatewayConfigRecord{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"configs": records})
	case http.MethodPost:
		var req struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Format      string `json:"format"`
			Body        string `json:"body"`
			Enabled     *bool  `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" || strings.TrimSpace(req.Body) == "" {
			writeError(w, http.StatusBadRequest, "name and body are required")
			return
		}
		format := normalizedConfigFormat(req.Format)
		if _, err := gatewayconfig.Parse([]byte(req.Body), "."+format); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		enabled := true
		if req.Enabled != nil {
			enabled = *req.Enabled
		}
		id := strings.TrimSpace(req.ID)
		if id == "" {
			id = newID("cfg")
		}
		now := time.Now().UTC()
		record := store.GatewayConfigRecord{
			ID: id, Name: req.Name, Description: req.Description, Format: format,
			Body: req.Body, CreatedAt: now, UpdatedAt: now, Enabled: enabled,
		}
		if err := h.store.UpsertGatewayConfig(r.Context(), record); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, record)
	case http.MethodPatch:
		ref := strings.TrimSpace(r.URL.Query().Get("id"))
		if ref == "" {
			ref = strings.TrimSpace(r.URL.Query().Get("name"))
		}
		var req struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if ref == "" || req.Enabled == nil {
			writeError(w, http.StatusBadRequest, "id or name and enabled are required")
			return
		}
		if err := h.store.SetGatewayConfigEnabled(r.Context(), ref, *req.Enabled); err != nil {
			writeError(w, http.StatusNotFound, "config not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"enabled": *req.Enabled})
	case http.MethodDelete:
		ref := strings.TrimSpace(r.URL.Query().Get("id"))
		if ref == "" {
			ref = strings.TrimSpace(r.URL.Query().Get("name"))
		}
		if ref == "" {
			writeError(w, http.StatusBadRequest, "id or name is required")
			return
		}
		if err := h.store.DeleteGatewayConfig(r.Context(), ref); err != nil {
			writeError(w, http.StatusNotFound, "config not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func isDashboardRoute(path string) bool {
	switch path {
	case "/", "/overview", "/playground", "/logs", "/configs", "/guardrails", "/prompts", "/keys", "/providers":
		return true
	default:
		return strings.HasPrefix(path, "/traces/")
	}
}

func serveDashboardIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// scopedAPIKeyID uses the authentication context, not caller-controlled headers.
func scopedAPIKeyID(r *http.Request) string {
	return gateway.ScopedAPIKeyID(r)
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "healthy",
		"time":   time.Now().UTC(),
	})
}

func windowStart(window string) time.Time {
	now := time.Now().UTC()
	switch strings.TrimSpace(window) {
	case "1h":
		return now.Add(-1 * time.Hour)
	case "6h":
		return now.Add(-6 * time.Hour)
	case "7d":
		return now.Add(-7 * 24 * time.Hour)
	case "30d":
		return now.Add(-30 * 24 * time.Hour)
	default:
		return now.Add(-24 * time.Hour)
	}
}

func normalizedConfigFormat(format string) string {
	switch strings.ToLower(strings.TrimPrefix(strings.TrimSpace(format), ".")) {
	case "json":
		return "json"
	default:
		return "yaml"
	}
}

func configExt(path string) string {
	return strings.ToLower(filepath.Ext(path))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func newID(prefix string) string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return fmt.Sprintf("%s_%d_%s", prefix, time.Now().UnixNano(), hex.EncodeToString(b[:]))
}

func (h *Handler) prometheus(w http.ResponseWriter, r *http.Request) {
	metrics, err := h.store.GetMetrics(r.Context(), time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fprintf := func(format string, values ...any) { _, _ = fmt.Fprintf(w, format, values...) }
	fprintf("# HELP omniswitch_requests_total Gateway requests over the last 24 hours.\n")
	fprintf("# TYPE omniswitch_requests_total counter\n")
	fprintf("omniswitch_requests_total %d\n", metrics.TotalRequests)
	fprintf("omniswitch_requests_allowed_total %d\n", metrics.AllowedCount)
	fprintf("omniswitch_requests_denied_total %d\n", metrics.DeniedCount)
	fprintf("omniswitch_requests_errors_total %d\n", metrics.ErrorCount)
	fprintf("omniswitch_cache_hits_total %d\n", metrics.CacheHits)
	fprintf("omniswitch_tokens_total %d\n", metrics.TotalTokens)
	fprintf("omniswitch_cost_usd_total %.6f\n", metrics.TotalCost)
	fprintf("omniswitch_request_latency_ms_average %.3f\n", metrics.AvgLatencyMs)
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	kind, code := "invalid_request_error", "invalid_request"
	if status >= 500 {
		kind, code = "server_error", "server_error"
	} else if status == http.StatusForbidden {
		kind, code = "permission_error", "insufficient_role"
	} else if status == http.StatusUnauthorized {
		kind, code = "authentication_error", "unauthorized"
	} else if status == http.StatusNotFound {
		code = "not_found"
	}
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": msg, "type": kind, "code": code}})
}
