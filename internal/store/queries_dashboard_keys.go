package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// APIKeyUpdate is a partial update of administratively writable fields. It
// deliberately excludes hashes, prefixes, expiry, last use, and accrued spend.
type APIKeyUpdate struct {
	Name               *string  `json:"name"`
	Enabled            *bool    `json:"enabled"`
	RateLimit          *int     `json:"rate_limit"`
	BudgetUSD          *float64 `json:"budget_usd"`
	MonthlyCostBudget  *float64 `json:"monthly_cost_budget"`
	MonthlyTokenBudget *int     `json:"monthly_token_budget"`
	Role               *string  `json:"role"`
	WorkspaceID        *string  `json:"workspace_id"`
	RequestsPerMinute  *int     `json:"requests_per_minute"`
	TokensPerMinute    *int     `json:"tokens_per_minute"`
	USDPerDay          *float64 `json:"usd_per_day"`
}

// Validate checks limits and roles before any key mutation. Zero budgets and
// limits are valid and mean unlimited.
func (u APIKeyUpdate) Validate() error {
	if u.Name != nil && (strings.TrimSpace(*u.Name) == "" || len(*u.Name) > 256 || !utf8.ValidString(*u.Name) || strings.ContainsRune(*u.Name, 0)) {
		return fmt.Errorf("name must contain 1 to 256 bytes of valid text without NUL characters")
	}
	if u.WorkspaceID != nil && (len(*u.WorkspaceID) > 256 || !utf8.ValidString(*u.WorkspaceID) || strings.ContainsRune(*u.WorkspaceID, 0)) {
		return fmt.Errorf("workspace_id must not exceed 256 bytes and must be valid text without NUL characters")
	}
	if u.Role != nil {
		switch *u.Role {
		case "viewer", "member", "admin", "owner":
		default:
			return fmt.Errorf("role must be viewer, member, admin, or owner")
		}
	}
	for _, field := range []struct {
		name  string
		value *int
	}{{"rate_limit", u.RateLimit}, {"monthly_token_budget", u.MonthlyTokenBudget}, {"requests_per_minute", u.RequestsPerMinute}, {"tokens_per_minute", u.TokensPerMinute}} {
		if field.value != nil && (*field.value < 0 || int64(*field.value) > math.MaxInt32) {
			return fmt.Errorf("%s must be between 0 and %d", field.name, math.MaxInt32)
		}
	}
	for _, field := range []struct {
		name  string
		value *float64
	}{{"budget_usd", u.BudgetUSD}, {"monthly_cost_budget", u.MonthlyCostBudget}, {"usd_per_day", u.USDPerDay}} {
		if field.value != nil && (*field.value < 0 || math.IsNaN(*field.value) || math.IsInf(*field.value, 0)) {
			return fmt.Errorf("%s must be a finite non-negative number", field.name)
		}
	}
	return nil
}

func (s *Store) UpdateAPIKey(ctx context.Context, id string, update APIKeyUpdate) (APIKey, error) {
	if err := update.Validate(); err != nil {
		return APIKey{}, err
	}
	sets := []string{}
	args := []any{}
	add := func(column string, value any) {
		sets = append(sets, column+" = ?")
		args = append(args, value)
	}
	if update.Name != nil {
		add("name", strings.TrimSpace(*update.Name))
	}
	if update.Enabled != nil {
		add("enabled", boolToInt(*update.Enabled))
	}
	if update.RateLimit != nil {
		add("rate_limit", *update.RateLimit)
	}
	if update.BudgetUSD != nil {
		if update.MonthlyCostBudget == nil {
			sets = append(sets, "monthly_cost_budget = CASE WHEN monthly_cost_budget = budget_usd THEN ? ELSE monthly_cost_budget END")
			args = append(args, *update.BudgetUSD)
		}
		add("budget_usd", *update.BudgetUSD)
	}
	if update.MonthlyCostBudget != nil {
		if update.BudgetUSD == nil {
			// Older create APIs copied the monthly budget into budget_usd.
			// Keep that alias in sync without changing independently set limits.
			sets = append(sets, "budget_usd = CASE WHEN budget_usd = monthly_cost_budget THEN ? ELSE budget_usd END")
			args = append(args, *update.MonthlyCostBudget)
		}
		add("monthly_cost_budget", *update.MonthlyCostBudget)
	}
	if update.MonthlyTokenBudget != nil {
		add("monthly_token_budget", *update.MonthlyTokenBudget)
	}
	if update.Role != nil {
		add("role", *update.Role)
	}
	if update.WorkspaceID != nil {
		add("workspace_id", strings.TrimSpace(*update.WorkspaceID))
	}
	if update.RequestsPerMinute != nil {
		add("requests_per_minute", *update.RequestsPerMinute)
	}
	if update.TokensPerMinute != nil {
		add("tokens_per_minute", *update.TokensPerMinute)
	}
	if update.USDPerDay != nil {
		add("usd_per_day", *update.USDPerDay)
	}
	if len(sets) == 0 {
		return APIKey{}, fmt.Errorf("at least one key field is required")
	}
	result, err := s.exec(ctx, "UPDATE api_keys SET "+strings.Join(sets, ", ")+" WHERE id = ?", append(args, id)...)
	if err != nil {
		return APIKey{}, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return APIKey{}, err
	}
	if count == 0 {
		return APIKey{}, sql.ErrNoRows
	}
	return s.GetAPIKeyByID(ctx, id)
}

// UpdateAPIKeyHash rotates only the bootstrap secret. It cannot reset an
// administrator's disabled state, name, role, workspace, expiry, or limits.
func (s *Store) UpdateAPIKeyHash(ctx context.Context, id, hash string) error {
	_, err := s.exec(ctx, "UPDATE api_keys SET key_hash = ? WHERE id = ? AND key_hash <> ?", hash, id, hash)
	return err
}

// KeyBudgetUsage includes keys with zero traffic without a per-key query.
type KeyBudgetUsage struct {
	KeyID       string  `json:"key_id"`
	CostUSD     float64 `json:"cost_usd"`
	Tokens      int     `json:"tokens"`
	CostBudget  float64 `json:"cost_budget"`
	TokenBudget int     `json:"token_budget"`
	CostPct     float64 `json:"cost_pct"`
	TokenPct    float64 `json:"token_pct"`
	State       string  `json:"state"`
}

// BudgetUsageByKey is one grouped query, not an N+1 GetBudgetUsage loop. Budget
// selection matches enforcement: budget_usd, then monthly_cost_budget.
func (s *Store) BudgetUsageByKey(ctx context.Context, since, until time.Time, apiKeyID string) ([]KeyBudgetUsage, error) {
	where, args := s.indexedTimeRange("timestamp", since, until)
	where += " AND decision = 'ALLOW'"
	if apiKeyID != "" {
		where += " AND api_key_id = ?"
		args = append(args, apiKeyID)
	}
	query := `SELECT k.id, COALESCE(u.cost, 0), COALESCE(u.tokens, 0),
		CASE WHEN k.budget_usd > 0 THEN k.budget_usd ELSE k.monthly_cost_budget END,
		k.monthly_token_budget
		FROM api_keys k LEFT JOIN (
			SELECT api_key_id, SUM(cost) AS cost, SUM(total_tokens) AS tokens
			FROM request_logs WHERE ` + where + ` GROUP BY api_key_id
		) u ON u.api_key_id = k.id`
	if apiKeyID != "" {
		query += " WHERE k.id = ?"
		args = append(args, apiKeyID)
	}
	query += " ORDER BY k.id LIMIT 10000"
	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KeyBudgetUsage{}
	for rows.Next() {
		row := KeyBudgetUsage{State: "ok"}
		if err := rows.Scan(&row.KeyID, &row.CostUSD, &row.Tokens, &row.CostBudget, &row.TokenBudget); err != nil {
			return nil, err
		}
		if row.CostBudget > 0 {
			row.CostPct = row.CostUSD / row.CostBudget
		}
		if row.TokenBudget > 0 {
			row.TokenPct = float64(row.Tokens) / float64(row.TokenBudget)
		}
		if row.CostPct >= 1 || row.TokenPct >= 1 {
			row.State = "exceeded"
		} else if row.CostPct >= 0.8 || row.TokenPct >= 0.8 {
			row.State = "near"
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// InsertBootstrapAPIKeyIfAbsent is safe when replicas start concurrently. A
// conflict never upserts administrative fields (including a disabled state).
func (s *Store) InsertBootstrapAPIKeyIfAbsent(ctx context.Context, key APIKey) error {
	_, err := s.exec(ctx, `INSERT INTO api_keys
		(id, name, key_hash, key_prefix, created_at, rate_limit, enabled, workspace_id, role, metadata)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '{}') ON CONFLICT(id) DO NOTHING`,
		key.ID, key.Name, key.KeyHash, key.KeyPrefix, key.CreatedAt.UTC().Format(time.RFC3339),
		key.RateLimit, boolToInt(key.Enabled), key.WorkspaceID, key.Role)
	return err
}

// APIKeyNames fetches the display names needed by a bounded trace in one query.
func (s *Store) APIKeyNames(ctx context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string)
	unique := make(map[string]bool)
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		if id != "" && !unique[id] && len(args) < 500 {
			unique[id] = true
			args = append(args, id)
		}
	}
	if len(args) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	rows, err := s.query(ctx, "SELECT id, name FROM api_keys WHERE id IN ("+placeholders+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}
