package dashboard

import (
	"net/http"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/gateway"
)

// SessionInfo contains non-secret process facts supplied by cmd/gateway.
type SessionInfo struct {
	AuthEnabled bool
	Version     string
	Commit      string
	Listen      string
	MCPEnabled  bool
	LogPayloads bool
}

func (h *Handler) SetSessionInfo(info SessionInfo) {
	h.sessionInfo = info
}

// SetLogPayloadsSource keeps the session posture accurate across live reloads.
func (h *Handler) SetLogPayloadsSource(source func() bool) { h.logPayloadsSource = source }

func (h *Handler) getSession(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	logPayloads := h.sessionInfo.LogPayloads
	if h.logPayloadsSource != nil {
		logPayloads = h.logPayloadsSource()
	}
	identity, authenticated := gateway.IdentityFromContext(r.Context())
	if h.sessionInfo.AuthEnabled && !authenticated {
		writeError(w, http.StatusUnauthorized, "authenticated identity is required")
		return
	}
	name, prefix, keyID := "", "", identity.APIKeyID
	role, method := identity.Role, identity.AuthMethod
	var expiresAt *time.Time
	if !h.sessionInfo.AuthEnabled {
		role, method, keyID = "owner", "none", ""
		identity.WorkspaceID, identity.OrganizationID = "", ""
	} else if identity.AuthMethod == "api_key" {
		key, err := h.store.GetAPIKeyByID(r.Context(), keyID)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "API key is no longer available")
			return
		}
		name, prefix, expiresAt = key.Name, key.KeyPrefix, key.ExpiresAt
		if expiresAt != nil {
			at := expiresAt.UTC()
			expiresAt = &at
		}
	} else {
		name = identity.Subject
	}
	// Explicit fields, rather than serializing a stored key, make it impossible
	// for future key secrets or metadata to leak into the session response.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true, "auth_enabled": h.sessionInfo.AuthEnabled,
		"auth_method": method, "key_id": keyID, "name": name, "key_prefix": prefix,
		"role": role, "workspace_id": identity.WorkspaceID, "organization_id": identity.OrganizationID,
		"expires_at": expiresAt, "is_bootstrap": method == "api_key" && keyID == "bootstrap-admin",
		"gateway": map[string]any{
			"version": h.sessionInfo.Version, "commit": h.sessionInfo.Commit, "storage": h.store.Driver(),
			"listen": h.sessionInfo.Listen, "mcp_enabled": h.sessionInfo.MCPEnabled, "log_payloads": logPayloads,
		},
	})
}
