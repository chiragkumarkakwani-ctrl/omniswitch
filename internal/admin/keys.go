package admin

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/omniswitch-dev/omniswitch/internal/gateway"
	"github.com/omniswitch-dev/omniswitch/internal/store"
)

// apiKeyResponse keeps zero budgets and an empty workspace in the console
// contract while preserving the existing optional fields on store.APIKey.
// The embedded key's hash is already json:"-"; raw secrets only leave create.
type apiKeyResponse struct {
	store.APIKey
	WorkspaceID        string  `json:"workspace_id"`
	Role               string  `json:"role"`
	BudgetUSD          float64 `json:"budget_usd"`
	MonthlyCostBudget  float64 `json:"monthly_cost_budget"`
	MonthlyTokenBudget int     `json:"monthly_token_budget"`
}

func keyResponse(key store.APIKey) apiKeyResponse {
	key.CreatedAt = key.CreatedAt.UTC()
	if key.ExpiresAt != nil {
		at := key.ExpiresAt.UTC()
		key.ExpiresAt = &at
	}
	if key.LastUsedAt != nil {
		at := key.LastUsedAt.UTC()
		key.LastUsedAt = &at
	}
	role := gateway.EffectiveRole(key.Role)
	return apiKeyResponse{APIKey: key, WorkspaceID: key.WorkspaceID, Role: role, BudgetUSD: key.BudgetUSD, MonthlyCostBudget: key.MonthlyCostBudget, MonthlyTokenBudget: key.MonthlyTokenBudget}
}

func decodeKeyRequest(w http.ResponseWriter, r *http.Request, value any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("invalid key request: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("request must contain exactly one JSON object")
	}
	return nil
}

// ListAPIKeys handles GET /api/keys. A non-admin query never fetches or returns
// other keys, even if the caller supplies a workspace or forged role header.
func (h *Handler) ListAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys := []store.APIKey{}
	if scope := gateway.ScopedAPIKeyID(r); scope != "" {
		key, err := h.store.GetAPIKeyByID(r.Context(), scope)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusInternalServerError, "could not read API key")
			return
		}
		if err == nil {
			keys = append(keys, key)
		}
	} else {
		var err error
		keys, err = h.store.ListAPIKeys(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not read API keys")
			return
		}
	}
	rows := []apiKeyResponse{}
	workspace := strings.TrimSpace(r.URL.Query().Get("workspace_id"))
	for _, key := range keys {
		if gateway.IsAdmin(r) && workspace != "" && key.WorkspaceID != workspace {
			continue
		}
		rows = append(rows, keyResponse(key))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": rows})
}

// UpdateAPIKey handles PATCH /api/keys?id= with an atomic partial update.
func (h *Handler) UpdateAPIKey(w http.ResponseWriter, r *http.Request) {
	if !gateway.IsAdmin(r) {
		writeError(w, http.StatusForbidden, "key updates require admin role")
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" || len(id) > 256 || !utf8.ValidString(id) || strings.ContainsRune(id, 0) {
		writeError(w, http.StatusBadRequest, "a valid key id is required")
		return
	}
	var update store.APIKeyUpdate
	if err := decodeKeyRequest(w, r, &update); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if update == (store.APIKeyUpdate{}) {
		writeError(w, http.StatusBadRequest, "at least one key field is required")
		return
	}
	if err := update.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	key, err := h.store.UpdateAPIKey(r.Context(), id, update)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "API key not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update API key")
		return
	}
	writeJSON(w, http.StatusOK, keyResponse(key))
}

func requireKeyAdmin(w http.ResponseWriter, r *http.Request) bool {
	if gateway.IsAdmin(r) {
		return true
	}
	writeError(w, http.StatusForbidden, "key mutations require admin role")
	return false
}
