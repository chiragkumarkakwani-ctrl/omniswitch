package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"time"
)

// RetentionPolicy controls the independently retained classes of gateway data.
// Zero durations keep that class forever; an all-zero policy disables sweeps.
// RequestLogs excludes limited requests and DENY decisions, which have their
// own policy. Payloads are retained only as long as their request-log row.
type RetentionPolicy struct {
	RequestLogs      time.Duration
	Payloads         time.Duration
	GuardrailEvents  time.Duration
	ShadowRequests   time.Duration
	LimitedAndDenied time.Duration
	SweepInterval    time.Duration
	BatchSize        int
}

func DefaultRetentionPolicy() RetentionPolicy {
	return RetentionPolicy{
		RequestLogs:      30 * 24 * time.Hour,
		Payloads:         7 * 24 * time.Hour,
		GuardrailEvents:  30 * 24 * time.Hour,
		ShadowRequests:   7 * 24 * time.Hour,
		LimitedAndDenied: 30 * 24 * time.Hour,
		SweepInterval:    time.Hour,
		BatchSize:        5000,
	}
}

func (p RetentionPolicy) Enabled() bool {
	return p.RequestLogs > 0 || p.Payloads > 0 || p.GuardrailEvents > 0 || p.ShadowRequests > 0 || p.LimitedAndDenied > 0
}

func (p RetentionPolicy) Validate() error {
	for _, field := range []struct {
		name     string
		duration time.Duration
	}{
		{"request_logs", p.RequestLogs},
		{"payloads", p.Payloads},
		{"guardrail_events", p.GuardrailEvents},
		{"shadow_requests", p.ShadowRequests},
		{"limited_and_denied", p.LimitedAndDenied},
	} {
		if field.duration < 0 {
			return fmt.Errorf("retention.%s must be nonnegative", field.name)
		}
	}
	if p.SweepInterval <= 0 {
		return fmt.Errorf("retention.sweep_interval must be positive")
	}
	if p.BatchSize <= 0 {
		return fmt.Errorf("retention.batch_size must be positive")
	}
	return nil
}

// RetentionDeleted always includes all four keys, even when no rows are removed.
type RetentionDeleted struct {
	RequestLogs     int64 `json:"request_logs"`
	GuardrailEvents int64 `json:"guardrail_events"`
	ShadowRequests  int64 `json:"shadow_requests"`
	SemanticCache   int64 `json:"semantic_cache"`
}

type RetentionResult struct {
	Deleted         RetentionDeleted
	PayloadsCleared int64
	// Skipped means retention is disabled or another Postgres replica is sweeping.
	Skipped bool
}

type retentionExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

const (
	retentionOrdinaryLogs = "COALESCE(status, '') != 'limited' AND COALESCE(decision, '') != 'DENY'"
	retentionLimitedLogs  = "status = 'limited' OR decision = 'DENY'"
	retentionPayloads     = "COALESCE(request_body, '') != '' OR COALESCE(response_body, '') != ''"
)

// Prune deletes rows older than before, using at most batch rows per statement.
// Cache entries expiring exactly at before are also expired. Only retention-owned
// table/column pairs are permitted; feedback,
// keys, prompts, configurations, and other durable records cannot be pruned.
func (s *Store) Prune(ctx context.Context, table, column string, before time.Time, batch int) (int64, error) {
	return s.prune(ctx, s.db, table, column, before, batch, "")
}

func (s *Store) prune(ctx context.Context, executor retentionExecutor, table, column string, before time.Time, batch int, filter string) (int64, error) {
	allowed := map[string]string{
		"request_logs": "timestamp", "guardrail_events": "timestamp",
		"shadow_requests": "timestamp", "semantic_cache": "expires_at",
	}
	if expected, ok := allowed[table]; !ok || expected != column {
		return 0, fmt.Errorf("unsupported retention target %q.%q", table, column)
	}
	if batch <= 0 {
		return 0, fmt.Errorf("retention batch size must be positive")
	}
	query := fmt.Sprintf("DELETE FROM %s WHERE id IN (%s)", table, s.retentionSelection(table, column, filter))
	return s.retentionBatches(ctx, executor, query, before, batch)
}

// Shared by the batch statements and their EXPLAIN regression tests.
func (s *Store) retentionSelection(table, column, filter string) string {
	predicate := s.retentionBefore(column)
	if filter != "" {
		predicate += " AND (" + filter + ")"
	}
	return fmt.Sprintf("SELECT id FROM %s WHERE %s ORDER BY %s LIMIT ?", table, predicate, column)
}

// NullPayloads removes stored request/response bodies without removing their
// rows. Empty or already-null bodies are not counted as cleared payloads.
func (s *Store) NullPayloads(ctx context.Context, before time.Time, batch int) (int64, error) {
	return s.nullPayloads(ctx, s.db, before, batch)
}

func (s *Store) nullPayloads(ctx context.Context, executor retentionExecutor, before time.Time, batch int) (int64, error) {
	if batch <= 0 {
		return 0, fmt.Errorf("retention batch size must be positive")
	}
	query := `UPDATE request_logs SET request_body = NULL, response_body = NULL
		WHERE id IN (` + s.retentionSelection("request_logs", "timestamp", retentionPayloads) + `)`
	return s.retentionBatches(ctx, executor, query, before, batch)
}

// Timestamps are legacy RFC3339 text. The lexical upper bound (cutoff + 24h
// in UTC) lets both dialects range-search their text timestamp indexes without
// excluding expired timestamps with offsets or fractional seconds. The parsed
// predicate still decides expiry precisely. Empty/null cache expiry values
// represent no expiry and are also excluded from the partial expiry index.
func (s *Store) retentionBefore(column string) string {
	comparison := " < "
	predicate := column + " < ? AND "
	if column == "expires_at" {
		comparison = " <= "
		predicate = column + " > '' AND " + predicate
	}
	if s.pg {
		return predicate + "NULLIF(" + column + ", '')::timestamptz" + comparison + "?::timestamptz"
	}
	return predicate + "julianday(" + column + ")" + comparison + "julianday(?)"
}

func (s *Store) retentionBatches(ctx context.Context, executor retentionExecutor, query string, before time.Time, batch int) (int64, error) {
	if s.pg {
		query = rewriteSQL(query)
	}
	var total int64
	for {
		result, err := executor.ExecContext(ctx, query, before.UTC().Add(24*time.Hour).Format(time.RFC3339Nano), before.UTC().Format(time.RFC3339Nano), batch)
		if err != nil {
			return total, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return total, err
		}
		total += count
		if count < int64(batch) {
			return total, nil
		}
	}
}

// SweepRetention clears expired payloads first, then prunes each independently
// retained class and expired semantic-cache entries. Partial counts survive an
// error, since each batch commits separately. at fixes all cutoffs for a run.
// Postgres holds a nonblocking advisory lock on the sweep's dedicated session;
// SQLite releases its single connection between statements for other requests.
func (s *Store) SweepRetention(ctx context.Context, policy RetentionPolicy, at time.Time) (result RetentionResult, err error) {
	if err = policy.Validate(); err != nil {
		return result, err
	}
	if !policy.Enabled() {
		result.Skipped = true
		return result, nil
	}
	status, err := s.SchemaStatus(ctx)
	if err != nil {
		return result, err
	}
	if status.Pending != 0 {
		return result, fmt.Errorf("schema_pending: %d migrations must be applied before retention", status.Pending)
	}

	var executor retentionExecutor = s.db
	if s.pg {
		conn, connErr := s.db.Conn(ctx)
		if connErr != nil {
			return result, connErr
		}
		defer conn.Close()
		var locked bool
		if err = conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(hashtext('omniswitch_retention'))").Scan(&locked); err != nil {
			return result, err
		}
		if !locked {
			result.Skipped = true
			return result, nil
		}
		defer func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var unlocked bool
			unlockErr := conn.QueryRowContext(unlockCtx, "SELECT pg_advisory_unlock(hashtext('omniswitch_retention'))").Scan(&unlocked)
			if unlockErr != nil || !unlocked {
				// Never return a session that may still hold the lock to the pool.
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
				if err == nil {
					err = fmt.Errorf("release retention advisory lock: unlocked=%t, error=%v", unlocked, unlockErr)
				}
			}
		}()
		executor = conn
	}

	if policy.Payloads > 0 {
		result.PayloadsCleared, err = s.nullPayloads(ctx, executor, at.Add(-policy.Payloads), policy.BatchSize)
		if err != nil {
			return result, fmt.Errorf("clear request payloads: %w", err)
		}
	}
	for _, target := range []struct {
		table, column, filter string
		duration              time.Duration
		count                 *int64
	}{
		{"request_logs", "timestamp", retentionOrdinaryLogs, policy.RequestLogs, &result.Deleted.RequestLogs},
		{"request_logs", "timestamp", retentionLimitedLogs, policy.LimitedAndDenied, &result.Deleted.RequestLogs},
		{"guardrail_events", "timestamp", "", policy.GuardrailEvents, &result.Deleted.GuardrailEvents},
		{"shadow_requests", "timestamp", "", policy.ShadowRequests, &result.Deleted.ShadowRequests},
	} {
		if target.duration == 0 {
			continue
		}
		count, pruneErr := s.prune(ctx, executor, target.table, target.column, at.Add(-target.duration), policy.BatchSize, target.filter)
		*target.count += count
		if pruneErr != nil {
			return result, fmt.Errorf("prune %s: %w", target.table, pruneErr)
		}
	}
	// Expiry is intrinsic to cache entries, not an operator retention window.
	result.Deleted.SemanticCache, err = s.prune(ctx, executor, "semantic_cache", "expires_at", at, policy.BatchSize, "")
	if err != nil {
		return result, fmt.Errorf("prune semantic_cache: %w", err)
	}
	return result, nil
}
