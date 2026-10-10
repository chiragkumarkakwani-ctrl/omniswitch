package store

import (
	"context"
	"testing"
)

func TestRetentionIndexMigrationIsTransactional(t *testing.T) {
	for _, postgres := range []bool{false, true} {
		name := "sqlite"
		if postgres {
			name = "postgres"
		}
		t.Run(name, func(t *testing.T) {
			st := newMigrationTestStore(t, postgres, false)
			ctx := context.Background()
			seedBaseline(t, st)
			if _, err := st.exec(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			conn, err := st.db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, migration := range migrations[:3] {
				if err := st.applyMigration(ctx, conn, migration); err != nil {
					conn.Close()
					t.Fatal(err)
				}
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			want := SchemaStatus{Current: 3, Latest: 4, Pending: 1}
			if status, err := st.SchemaStatus(ctx); err != nil || status != want {
				t.Fatalf("version-three status = %+v, err=%v", status, err)
			}
			// The final index statement fails after the other indexes are created.
			// None of those indexes or the version-four ledger row may survive.
			if _, err := st.exec(ctx, "ALTER TABLE semantic_cache RENAME TO unavailable_cache"); err != nil {
				t.Fatal(err)
			}
			if err := st.Migrate(ctx); err == nil {
				t.Fatal("migration with a missing cache table succeeded")
			}
			if status, err := st.SchemaStatus(ctx); err != nil || status != want {
				t.Fatalf("failed migration changed ledger: %+v err=%v", status, err)
			}
			indexes := []string{"idx_shadow_timestamp", "idx_logs_payload_timestamp", "idx_logs_retention_timestamp", "idx_logs_limited_timestamp", "idx_cache_expires_at"}
			for _, index := range indexes {
				assertRetentionIndex(t, st, index, false)
			}
			if _, err := st.exec(ctx, "ALTER TABLE unavailable_cache RENAME TO semantic_cache"); err != nil {
				t.Fatal(err)
			}
			if err := st.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			assertLatestSchema(t, st)
			for _, index := range indexes {
				assertRetentionIndex(t, st, index, true)
			}
			logs, count, err := st.ListLogs(ctx, 10, 0, "", "")
			if err != nil || count != 1 || len(logs) != 1 || logs[0].RequestBody != "original prompt" || logs[0].ResponseBody != "original output" {
				t.Fatalf("index migration changed row data: %+v count=%d err=%v", logs, count, err)
			}
		})
	}
}

func assertRetentionIndex(t *testing.T, st *Store, index string, want bool) {
	t.Helper()
	query := "SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'index' AND name = ?)"
	if st.pg {
		query = "SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = current_schema() AND indexname = ?)"
	}
	var exists bool
	if err := st.queryRow(context.Background(), query, index).Scan(&exists); err != nil || exists != want {
		t.Fatalf("index %s exists=%t want=%t err=%v", index, exists, want, err)
	}
}
