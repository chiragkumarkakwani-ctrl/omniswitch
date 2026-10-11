package store

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestSQLiteMigrations(t *testing.T) {
	testMigrations(t, false)
}

func TestPostgresMigrations(t *testing.T) {
	if os.Getenv("OMNISWITCH_TEST_DATABASE_URL") == "" {
		t.Skip("OMNISWITCH_TEST_DATABASE_URL is not set")
	}
	testMigrations(t, true)
}

func testMigrations(t *testing.T, postgres bool) {
	t.Helper()
	ctx := context.Background()
	t.Run("fresh_and_noop", func(t *testing.T) {
		st := newMigrationTestStore(t, postgres, true)
		assertLatestSchema(t, st)
		before := migrationRows(t, st)
		if err := st.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		if after := migrationRows(t, st); !reflect.DeepEqual(before, after) {
			t.Fatalf("migration rows changed on rerun: before=%v after=%v", before, after)
		}
	})
	t.Run("baseline_upgrade", func(t *testing.T) {
		st := newMigrationTestStore(t, postgres, false)
		seedBaseline(t, st)
		status, err := st.SchemaStatus(ctx)
		if err != nil || status.Current != 0 || status.Pending != len(migrations) {
			t.Fatalf("unversioned status = %+v, err = %v", status, err)
		}
		if err := st.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		assertLatestSchema(t, st)
		logs, total, err := st.ListLogs(ctx, 10, 0, "", "")
		if err != nil || total != 1 || len(logs) != 1 {
			t.Fatalf("baseline logs = %v, total=%d, err=%v", logs, total, err)
		}
		if logs[0].ID != "legacy_request" || logs[0].RequestBody != "original prompt" || logs[0].Cost != 1.25 || logs[0].TotalTokens != 30 || logs[0].Endpoint != "" || logs[0].CostSource != "none" || logs[0].CachedTokens != 0 || logs[0].ReasoningTokens != 0 {
			t.Fatalf("baseline log changed: %+v", logs[0])
		}
		key, err := st.GetAPIKeyByID(ctx, "legacy_key")
		if err != nil || key.Name != "legacy" || key.KeyHash != "legacy_hash" || key.MonthlyTokenBudget != 100 || key.RequestsPerMinute != 0 || key.TokensPerMinute != 0 || key.USDPerDay != 0 || key.LastUsedAt != nil {
			t.Fatalf("baseline key changed: %+v, err=%v", key, err)
		}
	})
	t.Run("disabled_start_does_not_migrate", func(t *testing.T) {
		st := newMigrationTestStore(t, postgres, false)
		status, err := st.SchemaStatus(ctx)
		if err != nil || status.Current != 0 || status.Pending != len(migrations) {
			t.Fatalf("disabled migration status = %+v, err=%v", status, err)
		}
		rows, err := st.query(ctx, "SELECT version FROM schema_migrations")
		if err == nil {
			rows.Close()
			t.Fatal("opening without migrations created schema_migrations")
		}
		if err := st.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		assertLatestSchema(t, st)
	})
	t.Run("atomic_failure", func(t *testing.T) {
		st := newMigrationTestStore(t, postgres, true)
		conn, err := st.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		failed := migration{
			Version: migrations[len(migrations)-1].Version + 1,
			Name:    "rollback_test",
			SQLite:  []string{"CREATE TABLE rollback_probe (id INTEGER)", "INSERT INTO nonexistent_table VALUES (1)"},
		}
		if err := st.applyMigration(ctx, conn, failed); err == nil {
			t.Fatal("invalid migration succeeded")
		}
		var count int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM rollback_probe").Scan(&count); err == nil {
			t.Fatal("failed migration left its table behind")
		}
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE name = 'rollback_test'").Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed migration recorded: count=%d err=%v", count, err)
		}
	})
	t.Run("fields_round_trip", func(t *testing.T) {
		testReleaseFields(t, newMigrationTestStore(t, postgres, true))
	})
}

func assertLatestSchema(t *testing.T, st *Store) {
	t.Helper()
	status, err := st.SchemaStatus(context.Background())
	if err != nil || status.Current != migrations[len(migrations)-1].Version || status.Current != status.Latest || status.Pending != 0 {
		t.Fatalf("schema status = %+v, err=%v", status, err)
	}
	if got := len(migrationRows(t, st)); got != len(migrations) {
		t.Fatalf("applied migrations = %d, want %d", got, len(migrations))
	}
}

func migrationRows(t *testing.T, st *Store) []string {
	t.Helper()
	rows, err := st.query(context.Background(), "SELECT version, name, applied_at FROM schema_migrations ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var version int
		var name, appliedAt string
		if err := rows.Scan(&version, &name, &appliedAt); err != nil {
			t.Fatal(err)
		}
		if _, err := time.Parse(time.RFC3339Nano, appliedAt); err != nil {
			t.Fatal(err)
		}
		result = append(result, fmt.Sprintf("%d:%s:%s", version, name, appliedAt))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

// Build a v0.1.0 fixture entirely from the unchanged version-one baseline,
// without a migration ledger or any v0.2.0 fields. No binary DB is committed.
func seedBaseline(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, statement := range baselineStatements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, column := range migrations[0].Columns {
		if err := st.ensureColumn(ctx, tx, column); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range migrations[0].After {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.exec(ctx, `INSERT INTO request_logs
		(id, timestamp, trace_id, session_id, provider, model, api_key_id, status,
		input_tokens, output_tokens, total_tokens, latency_ms, cost, request_body,
		response_body, decision, decision_reason, error_message, cached, metadata)
		VALUES ('legacy_request', '2026-01-01T00:00:00Z', 'legacy_trace', '', 'openai', 'test',
		'legacy_key', 'success', 10, 20, 30, 25, 1.25, 'original prompt', 'original output', 'ALLOW', '', '', 0, '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.exec(ctx, `INSERT INTO api_keys (id, name, key_hash, key_prefix, created_at, monthly_token_budget, metadata)
		VALUES ('legacy_key', 'legacy', 'legacy_hash', 'legacy_prefix', '2026-01-01T00:00:00Z', 100, '{}')`); err != nil {
		t.Fatal(err)
	}
}
