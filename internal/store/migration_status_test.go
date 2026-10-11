package store

import (
	"context"
	"reflect"
	"testing"
)

func TestPendingMigrationsReadOnlyAndLedgerGaps(t *testing.T) {
	for _, postgres := range []bool{false, true} {
		name := "sqlite"
		if postgres {
			name = "postgres"
		}
		t.Run(name, func(t *testing.T) {
			st := newMigrationTestStore(t, postgres, false)
			ctx := context.Background()
			pending, err := st.PendingMigrations(ctx)
			if err != nil || len(pending) != len(migrations) {
				t.Fatalf("unversioned pending=%+v err=%v", pending, err)
			}
			for i, entry := range pending {
				if entry.Version != migrations[i].Version || entry.Name != migrations[i].Name {
					t.Fatalf("pending migrations not in order: %+v", pending)
				}
			}
			var ledgerExists bool
			query := "SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE name = 'schema_migrations')"
			if postgres {
				query = "SELECT to_regclass('schema_migrations') IS NOT NULL"
			}
			if err := st.queryRow(ctx, query).Scan(&ledgerExists); err != nil || ledgerExists {
				t.Fatalf("listing pending migrations created a ledger: exists=%t err=%v", ledgerExists, err)
			}
			if err := st.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			pending, err = st.PendingMigrations(ctx)
			if err != nil || len(pending) != 0 {
				t.Fatalf("current database pending=%+v err=%v", pending, err)
			}
			if _, err := st.exec(ctx, "DELETE FROM schema_migrations WHERE version = 2"); err != nil {
				t.Fatal(err)
			}
			before := migrationRows(t, st)
			pending, err = st.PendingMigrations(ctx)
			if err != nil || len(pending) != 1 || pending[0].Version != 2 || pending[0].Name != migrations[1].Name {
				t.Fatalf("ledger gap pending=%+v err=%v", pending, err)
			}
			if after := migrationRows(t, st); !reflect.DeepEqual(after, before) {
				t.Fatalf("pending listing modified ledger: before=%v after=%v", before, after)
			}
		})
	}
}
