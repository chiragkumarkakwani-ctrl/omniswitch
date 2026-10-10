package store

import (
	"context"
	"testing"
	"time"
)

func TestSQLiteBaselineAdoptionOnOpen(t *testing.T) {
	dir := t.TempDir()
	baseline, err := New(dir, WithMigrateOnStart(false))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { baseline.Close() })
	seedBaseline(t, baseline)
	if err := baseline.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	assertLatestSchema(t, st)
	logs, total, err := st.ListLogs(context.Background(), LogFilter{Since: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), Limit: 10})
	if err != nil || total != 1 || logs[0].RequestBody != "original prompt" {
		t.Fatalf("baseline adoption changed data: logs=%+v, total=%d, err=%v", logs, total, err)
	}
}

func TestVersionOneRemainsPendingWhenStartupMigrationsDisabled(t *testing.T) {
	dir := t.TempDir()
	baseline, err := New(dir, WithMigrateOnStart(false))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { baseline.Close() })
	seedBaseline(t, baseline)
	ctx := context.Background()
	if _, err := baseline.exec(ctx, `CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := baseline.exec(ctx, `INSERT INTO schema_migrations VALUES (1, 'baseline', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := baseline.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := New(dir, WithMigrateOnStart(false))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	status, err := st.SchemaStatus(ctx)
	if err != nil || status.Current != 1 || status.Pending != len(migrations)-1 {
		t.Fatalf("disabled version-one schema = %+v, err=%v", status, err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	assertLatestSchema(t, st)
	if got := migrationRows(t, st)[0]; got != "1:baseline:2026-01-01T00:00:00Z" {
		t.Fatalf("already-applied version one changed: %q", got)
	}
}

func TestMigrationIndexes(t *testing.T) {
	for _, postgres := range []bool{false, true} {
		name := "sqlite"
		if postgres {
			name = "postgres"
		}
		t.Run(name, func(t *testing.T) {
			st := newMigrationTestStore(t, postgres, true)
			query := "SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name IN ('request_logs', 'guardrail_events')"
			if postgres {
				query = "SELECT indexname FROM pg_indexes WHERE schemaname = current_schema() AND tablename IN ('request_logs', 'guardrail_events')"
			}
			rows, err := st.query(context.Background(), query)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			counts := make(map[string]int)
			for rows.Next() {
				var index string
				if err := rows.Scan(&index); err != nil {
					t.Fatal(err)
				}
				counts[index]++
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			for _, index := range []string{"idx_logs_timestamp", "idx_logs_api_key_timestamp", "idx_logs_provider_timestamp", "idx_logs_decision", "idx_guardrail_timestamp", "idx_guardrail_request"} {
				if counts[index] != 1 {
					t.Errorf("index %s occurs %d times, want one", index, counts[index])
				}
			}
		})
	}
}
