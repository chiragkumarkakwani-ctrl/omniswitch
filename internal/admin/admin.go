package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/store"
)

// Handler serves admin API endpoints.
type Handler struct {
	store *store.Store
}

// New creates a new admin handler.
func New(st *store.Store) *Handler {
	return &Handler{store: st}
}

// CreateKeyRequest is the request body for creating an API key.
type CreateKeyRequest struct {
	Name               string  `json:"name"`
	WorkspaceID        string  `json:"workspace_id,omitempty"`
	Role               string  `json:"role,omitempty"`
	RateLimit          int     `json:"rate_limit,omitempty"`
	BudgetUSD          float64 `json:"budget_usd,omitempty"`
	MonthlyCostBudget  float64 `json:"monthly_cost_budget,omitempty"`
	MonthlyTokenBudget int     `json:"monthly_token_budget,omitempty"`
	ExpiresIn          string  `json:"expires_in,omitempty"`
	RequestsPerMinute  int     `json:"requests_per_minute"`
	TokensPerMinute    int     `json:"tokens_per_minute"`
	USDPerDay          float64 `json:"usd_per_day"`
}

// CreateKeyResponse includes the full API key (shown only once).
type CreateKeyResponse struct {
	Key    string       `json:"key"`
	APIKey store.APIKey `json:"api_key"`
}

// CreateAPIKey handles POST /api/keys.
func (h *Handler) CreateAPIKey(w http.ResponseWriter, r *http.Request) {
	if !requireKeyAdmin(w, r) {
		return
	}
	req := CreateKeyRequest{RateLimit: 60}
	if err := decodeKeyRequest(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Role == "" {
		req.Role = "viewer"
	}
	validation := store.APIKeyUpdate{
		Name: &req.Name, WorkspaceID: &req.WorkspaceID, Role: &req.Role, RateLimit: &req.RateLimit,
		BudgetUSD: &req.BudgetUSD, MonthlyCostBudget: &req.MonthlyCostBudget, MonthlyTokenBudget: &req.MonthlyTokenBudget,
		RequestsPerMinute: &req.RequestsPerMinute, TokensPerMinute: &req.TokensPerMinute, USDPerDay: &req.USDPerDay,
	}
	if err := validation.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var expiresAt *time.Time
	if req.ExpiresIn != "" {
		duration, err := time.ParseDuration(req.ExpiresIn)
		if err != nil || duration <= 0 {
			writeError(w, http.StatusBadRequest, "expires_in must be a positive duration")
			return
		}
		at := time.Now().UTC().Add(duration)
		expiresAt = &at
	}

	rawKey, err := generateKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	hash := sha256.Sum256([]byte(rawKey))

	key := store.APIKey{
		ID:                 newID("key"),
		Name:               req.Name,
		KeyHash:            hex.EncodeToString(hash[:]),
		KeyPrefix:          rawKey[:12] + "...",
		WorkspaceID:        req.WorkspaceID,
		Role:               req.Role,
		CreatedAt:          time.Now().UTC(),
		RateLimit:          req.RateLimit,
		BudgetUSD:          firstPositive(req.BudgetUSD, req.MonthlyCostBudget),
		MonthlyCostBudget:  req.MonthlyCostBudget,
		MonthlyTokenBudget: req.MonthlyTokenBudget,
		RequestsPerMinute:  req.RequestsPerMinute, TokensPerMinute: req.TokensPerMinute, USDPerDay: req.USDPerDay,
		ExpiresAt: expiresAt,
		Enabled:   true,
	}

	if err := h.store.InsertAPIKey(r.Context(), key); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"key": rawKey, "api_key": keyResponse(key)})
}

// DeleteAPIKey handles DELETE /api/keys/{id}.
func (h *Handler) DeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	if !requireKeyAdmin(w, r) {
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if err := h.store.DeleteAPIKey(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func generateKey() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate API key: %w", err)
	}
	return "sk-sentinel-" + hex.EncodeToString(b[:]), nil
}

func newID(prefix string) string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return fmt.Sprintf("%s_%d_%s", prefix, time.Now().UnixNano(), hex.EncodeToString(b[:]))
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
	} else if status == http.StatusNotFound {
		code = "not_found"
	}
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": msg, "type": kind, "code": code}})
}

func firstPositive(values ...float64) float64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}
