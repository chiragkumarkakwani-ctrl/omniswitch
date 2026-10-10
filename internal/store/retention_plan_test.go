package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRetentionQueriesUseIndexRanges(t *testing.T) {
	for _, postgres := range []bool{false, true} {
		name := "sqlite"
		if postgres {
			name = "postgres"
		}
		t.Run(name, func(t *testing.T) {
			st := newMigrationTestStore(t, postgres, true)
			before := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
			seedRetentionPlanRows(t, st, before)
			targets := []struct {
				table, column, filter, index string
			}{
				{"request_logs", "timestamp", "", "idx_logs_timestamp"},
				{"request_logs", "timestamp", retentionOrdinaryLogs, "idx_logs_retention_timestamp"},
				{"request_logs", "timestamp", retentionLimitedLogs, "idx_logs_limited_timestamp"},
				{"request_logs", "timestamp", retentionPayloads, "idx_logs_payload_timestamp"},
				{"guardrail_events", "timestamp", "", "idx_guardrail_timestamp"},
				{"shadow_requests", "timestamp", "", "idx_shadow_timestamp"},
				{"semantic_cache", "expires_at", "", "idx_cache_expires_at"},
			}
			for _, target := range targets {
				t.Run(target.index, func(t *testing.T) {
					selection := st.retentionSelection(target.table, target.column, target.filter)
					query := fmt.Sprintf("DELETE FROM %s WHERE id IN (%s)", target.table, selection)
					if target.filter == retentionPayloads {
						query = "UPDATE request_logs SET request_body = NULL, response_body = NULL WHERE id IN (" + selection + ")"
					}
					assertRetentionRangePlan(t, st, query, before, target.index, target.column, 2)
				})
			}
			ctx := context.Background()
			if count, err := st.NullPayloads(ctx, before, 2); err != nil || count != 12 {
				t.Fatalf("initial payload clear count=%d err=%v", count, err)
			}
			if count, err := st.NullPayloads(ctx, before, 2); err != nil || count != 0 {
				t.Fatalf("repeated payload clear count=%d err=%v", count, err)
			}
			selection := st.retentionSelection("request_logs", "timestamp", retentionPayloads)
			assertRetentionRangePlan(t, st, selection, before, "idx_logs_payload_timestamp", "timestamp", 2)
			// An index, unlike a lower-bound watermark, also catches late imports.
			insertRetentionLog(t, st, RequestLog{ID: "late_payload", Timestamp: before.Add(-365 * 24 * time.Hour), ResponseBody: "late output"})
			if count, err := st.NullPayloads(ctx, before, 2); err != nil || count != 1 {
				t.Fatalf("late payload clear count=%d err=%v", count, err)
			}
			// Empty final batches must range-search, too, including at the shipped
			// batch size. Keep the large retained/future population in each table.
			batch := DefaultRetentionPolicy().BatchSize
			for _, target := range retentionPlanInserts {
				if _, err := st.Prune(ctx, target.table, target.column, before, batch); err != nil {
					t.Fatal(err)
				}
				if _, err := st.exec(ctx, "ANALYZE "+target.table); err != nil {
					t.Fatal(err)
				}
			}
			for _, target := range targets {
				t.Run(target.index+"_steady_state", func(t *testing.T) {
					selection := st.retentionSelection(target.table, target.column, target.filter)
					query := fmt.Sprintf("DELETE FROM %s WHERE id IN (%s)", target.table, selection)
					if target.filter == retentionPayloads {
						query = "UPDATE request_logs SET request_body = NULL, response_body = NULL WHERE id IN (" + selection + ")"
					}
					assertRetentionRangePlan(t, st, query, before, target.index, target.column, batch)
				})
			}
		})
	}
}

func assertRetentionRangePlan(t *testing.T, st *Store, query string, before time.Time, index, column string, batch int) {
	t.Helper()
	explain := "EXPLAIN QUERY PLAN "
	if st.pg {
		explain = "EXPLAIN "
	}
	rows, err := st.query(context.Background(), explain+query,
		before.Add(24*time.Hour).Format(time.RFC3339Nano), before.Format(time.RFC3339Nano), batch)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var detail string
		if st.pg {
			err = rows.Scan(&detail)
		} else {
			var id, parent, unused int
			err = rows.Scan(&id, &parent, &unused, &detail)
		}
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(lines, "\n")
	if st.pg {
		rangeCondition := false
		for _, line := range lines {
			if strings.Contains(line, "Index Cond:") && strings.Contains(strings.ReplaceAll(line, `"`, ""), column+" <") {
				rangeCondition = true
			}
		}
		indexed := strings.Contains(plan, "using "+index) || strings.Contains(plan, "Bitmap Index Scan on "+index)
		if strings.Contains(plan, "Seq Scan") || !indexed || !rangeCondition {
			t.Fatalf("expected an index range on %s:\n%s", index, plan)
		}
	} else {
		for _, line := range lines {
			if strings.Contains(line, "SEARCH ") && strings.Contains(line, "USING INDEX "+index+" (") && strings.Contains(line, column+"<?") {
				return
			}
		}
		t.Fatalf("expected an index range on %s:\n%s", index, plan)
	}
}
