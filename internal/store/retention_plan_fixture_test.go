package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

var retentionPlanInserts = []struct {
	table, column, query string
}{
	{"request_logs", "timestamp", `INSERT INTO request_logs (id, timestamp, status, decision, request_body) VALUES (?, ?, 'success', 'ALLOW', 'payload')`},
	{"guardrail_events", "timestamp", `INSERT INTO guardrail_events (id, timestamp) VALUES (?, ?)`},
	{"shadow_requests", "timestamp", `INSERT INTO shadow_requests (id, timestamp, request_id) VALUES (?, ?, 'parent')`},
	{"semantic_cache", "expires_at", `INSERT INTO semantic_cache (id, expires_at, created_at, model, prompt_text, vector_json, response_json) VALUES (?, ?, '2026-01-01T00:00:00Z', 'test', '', '[]', '{}')`},
}

func seedRetentionPlanRows(t *testing.T, st *Store, before time.Time) {
	t.Helper()
	ctx := context.Background()
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, target := range retentionPlanInserts {
		query := target.query
		if st.pg {
			query = rewriteSQL(query)
		}
		stmt, err := tx.PrepareContext(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 2060 {
			at := before.Add(365 * 24 * time.Hour)
			if i >= 2048 {
				at = before.Add(-time.Hour)
			}
			if _, err := stmt.ExecContext(ctx, fmt.Sprint(i), at.Format(time.RFC3339Nano)); err != nil {
				stmt.Close()
				t.Fatal(err)
			}
		}
		if err := stmt.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// Both log classes have candidates; the other class may be kept forever.
	if _, err := tx.ExecContext(ctx, "UPDATE request_logs SET status = 'limited', decision = 'DENY' WHERE id IN ('2048', '2049', '2050')"); err != nil {
		t.Fatal(err)
	}
	for i := range 512 {
		query := "INSERT INTO request_logs (id, timestamp, status, decision) VALUES (?, ?, ?, ?)"
		if st.pg {
			query = rewriteSQL(query)
		}
		status, decision := "success", "ALLOW"
		if i%2 == 0 {
			status, decision = "limited", "DENY"
		}
		if _, err := tx.ExecContext(ctx, query, "cleared_"+fmt.Sprint(i), before.Add(-365*24*time.Hour).Format(time.RFC3339Nano), status, decision); err != nil {
			t.Fatal(err)
		}
		query = retentionPlanInserts[3].query
		if st.pg {
			query = rewriteSQL(query)
		}
		// Entries with no expiry must not accumulate in the expiry range.
		var expiry any
		if i%2 == 0 {
			expiry = ""
		}
		if _, err := tx.ExecContext(ctx, query, "immortal_"+fmt.Sprint(i), expiry); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Let the PostgreSQL planner choose naturally; no enable_seqscan override.
	for _, target := range retentionPlanInserts {
		if _, err := st.exec(ctx, "ANALYZE "+target.table); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRetentionIndexBoundsPreserveOffsetsAndFractions(t *testing.T) {
	for _, postgres := range []bool{false, true} {
		name := "sqlite"
		if postgres {
			name = "postgres"
		}
		t.Run(name, func(t *testing.T) {
			st := newMigrationTestStore(t, postgres, true)
			ctx := context.Background()
			before := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
			positive, negative := time.FixedZone("positive", 14*60*60), time.FixedZone("negative", -14*60*60)
			timestamps := []time.Time{
				before.Add(-time.Second).In(positive),
				before.Add(-100 * time.Millisecond).In(negative),
				before.In(positive),
				before.Add(100 * time.Millisecond).In(negative),
				before.Add(time.Second).In(positive),
			}
			for _, target := range retentionPlanInserts {
				for i, at := range timestamps {
					if _, err := st.exec(ctx, target.query, fmt.Sprint(i), at.Format(time.RFC3339Nano)); err != nil {
						t.Fatal(err)
					}
				}
			}
			if count, err := st.NullPayloads(ctx, before, 1); err != nil || count != 2 {
				t.Fatalf("offset/fraction payload clearing count=%d err=%v", count, err)
			}
			for _, target := range retentionPlanInserts {
				want := int64(2)
				if target.table == "semantic_cache" {
					want++ // Cache entries expire at the boundary, unlike other rows.
				}
				if count, err := st.Prune(ctx, target.table, target.column, before, 1); err != nil || count != want {
					t.Fatalf("%s offset/fraction pruning count=%d want=%d err=%v", target.table, count, want, err)
				}
			}
		})
	}
}
