package dashboard

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/gateway"
	"github.com/omniswitch-dev/omniswitch/internal/model"
	mcpproxy "github.com/omniswitch-dev/omniswitch/internal/proxy"
	"github.com/omniswitch-dev/omniswitch/internal/store"
)

// MCPConfiguredTarget preserves disabled targets, which are not instantiated
// by the proxy. Policy paths are supplied separately from live inspection so
// the proxy never needs to retain config-file paths or secrets.
type MCPConfiguredTarget struct {
	Name       string
	Transport  string
	Enabled    bool
	PolicyPath string
}

func (h *Handler) SetMCPInspector(proxy *mcpproxy.Handler, enabled bool, configured []MCPConfiguredTarget) {
	h.mcp = proxy
	h.mcpEnabled = enabled
	h.mcpTargets = append([]MCPConfiguredTarget(nil), configured...)
}

func principal(r *http.Request) model.Agent {
	identity, ok := gateway.IdentityFromContext(r.Context())
	if !ok {
		return model.Agent{Role: "owner"}
	}
	return model.Agent{ID: identity.APIKeyID, Department: identity.WorkspaceID, Role: identity.Role}
}

type mcpTargetResponse struct {
	mcpproxy.TargetInspection
	PolicyPath *string `json:"policy_path,omitempty"`
}

func (h *Handler) getMCPTargets(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	live := map[string]mcpproxy.TargetInspection{}
	if h.mcp != nil && h.mcpEnabled {
		for _, entry := range h.mcp.InspectTargets(ctx, r.Header, principal(r)) {
			live[strings.ToLower(entry.Name)] = entry
		}
	}
	targets := []mcpTargetResponse{}
	for _, configured := range h.mcpTargets {
		entry, exists := live[strings.ToLower(configured.Name)]
		if !exists {
			entry = mcpproxy.TargetInspection{Name: configured.Name, Transport: configured.Transport, Enabled: h.mcpEnabled && configured.Enabled}
		}
		response := mcpTargetResponse{TargetInspection: entry}
		if gateway.IsAdmin(r) {
			path := configured.PolicyPath
			response.PolicyPath = &path
		}
		targets = append(targets, response)
		delete(live, strings.ToLower(configured.Name))
	}
	// Directly constructed proxies may not have a gatewayconfig inventory.
	if h.mcp != nil {
		for _, name := range h.mcp.TargetNames() {
			if entry, exists := live[strings.ToLower(name)]; exists {
				response := mcpTargetResponse{TargetInspection: entry}
				if gateway.IsAdmin(r) {
					path := ""
					response.PolicyPath = &path
				}
				targets = append(targets, response)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": h.mcpEnabled, "targets": targets})
}

func (h *Handler) getMCPTools(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	query, err := requestQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateFilterValue("target", query.Get("target")); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	agent := principal(r)
	if query.Has("as_key") {
		if !gateway.IsAdmin(r) {
			writeError(w, http.StatusForbidden, "as_key requires admin role")
			return
		}
		keyID := strings.TrimSpace(query.Get("as_key"))
		if keyID == "" || validateFilterValue("as_key", keyID) != nil {
			writeError(w, http.StatusBadRequest, "as_key must name an API key")
			return
		}
		key, err := h.store.GetAPIKeyByID(r.Context(), keyID)
		if err != nil {
			writeError(w, http.StatusNotFound, "API key not found")
			return
		}
		role := gateway.EffectiveRole(key.Role)
		agent = model.Agent{ID: key.ID, Department: key.WorkspaceID, Role: role}
	}
	tools := []mcpproxy.ToolInspection{}
	if h.mcp != nil && h.mcpEnabled {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		var err error
		tools, err = h.mcp.InspectTools(ctx, r.Header, agent, query.Get("target"), gateway.IsAdmin(r))
		if err != nil {
			if errors.Is(err, mcpproxy.ErrInspectionTargetNotFound) {
				writeError(w, http.StatusNotFound, "MCP target not found")
			} else {
				writeError(w, http.StatusBadGateway, "MCP tool discovery unavailable")
			}
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"principal": map[string]string{"key_id": agent.ID, "workspace_id": agent.Department, "role": agent.Role}, "tools": tools,
	})
}

func (h *Handler) getMCPDecisions(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	_, since, until, err := timeWindow(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, offset, err := pagination(r, 100)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	decisions, total, err := h.store.ListMCPDecisions(r.Context(), store.LogFilter{Since: since, Until: until, APIKeyID: scopedAPIKeyID(r), Limit: limit, Offset: offset})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read MCP decisions")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"decisions": decisions, "total": total})
}
