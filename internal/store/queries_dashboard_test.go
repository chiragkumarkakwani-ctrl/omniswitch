package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDashboardQueries(t *testing.T) {
	for _, dialect := range []struct {
		name     string
		postgres bool
	}{{"sqlite", false}, {"postgres", true}} {
		t.Run(dialect.name, func(t *testing.T) {
			t.Run("logs_and_facets", func(t *testing.T) { testDashboardLogs(t, newMigrationTestStore(t, dialect.postgres, true)) })
			t.Run("timeseries_and_latency", func(t *testing.T) { testDashboardMetrics(t, newMigrationTestStore(t, dialect.postgres, true)) })
			t.Run("guardrails_and_mcp", func(t *testing.T) { testDashboardEvents(t, newMigrationTestStore(t, dialect.postgres, true)) })
			t.Run("keys_and_usage", func(t *testing.T) { testDashboardKeys(t, newMigrationTestStore(t, dialect.postgres, true)) })
			t.Run("bounded_trace", func(t *testing.T) { testDashboardTrace(t, newMigrationTestStore(t, dialect.postgres, true)) })
			t.Run("export_cap", func(t *testing.T) { testDashboardExportCap(t, newMigrationTestStore(t, dialect.postgres, true)) })
		})
	}
}

func insertDashboardLog(t *testing.T, st *Store, entry RequestLog) {
	t.Helper()
	if err := st.InsertLog(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
}

func testDashboardLogs(t *testing.T, st *Store) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, status := range []string{"success", "error", "denied", "limited"} {
		entry := RequestLog{ID: fmt.Sprint(i), Timestamp: base.Add(time.Duration(i) * 100 * time.Millisecond),
			Provider: "alpha", Model: "model", APIKeyID: "mine", TraceID: "shared", SessionID: "session",
			Endpoint: "chat_completions", Status: status, Decision: "ALLOW", LatencyMs: float64(i),
			RequestBody: "PRIVATE PROMPT", ResponseBody: "PRIVATE RESPONSE", Cached: i == 0}
		if i == 1 {
			entry.Model = "literal-100%_x"
		}
		if i == 2 {
			entry.Decision = "DENY"
		}
		insertDashboardLog(t, st, entry)
	}
	insertDashboardLog(t, st, RequestLog{ID: "outside", Timestamp: base.Add(time.Second), Provider: "beta", Model: "foreign", APIKeyID: "other", Status: "success"})
	filter := LogFilter{Since: base, Until: base.Add(time.Second), APIKeyID: "mine", Sort: "time_asc"}
	check := func(f LogFilter, want []string, total int) {
		t.Helper()
		rows, gotTotal, err := st.ListLogs(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, row := range rows {
			ids = append(ids, row.ID)
			if row.Metadata != "{}" {
				t.Fatalf("legacy metadata = %q", row.Metadata)
			}
		}
		if !reflect.DeepEqual(ids, want) || gotTotal != total {
			t.Fatalf("filter %+v: ids=%v total=%d, want %v/%d", f, ids, gotTotal, want, total)
		}
	}
	check(filter, []string{"0", "1", "2", "3"}, 4)
	f := filter
	f.Since = base.Add(100 * time.Millisecond)
	f.Until = base.Add(300 * time.Millisecond)
	check(f, []string{"1", "2"}, 2)
	f = filter
	f.Statuses = []string{"denied", "limited"}
	f.Provider = "alpha"
	f.Model = "model"
	f.TraceID = "shared"
	f.SessionID = "session"
	f.Endpoint = "chat_completions"
	check(f, []string{"2", "3"}, 2)
	f.Decision = "DENY"
	check(f, []string{"2"}, 1)
	f = filter
	f.Query = "100%_x"
	check(f, []string{"1"}, 1)
	f.Query = "%' OR 1=1 --"
	check(f, []string{}, 0)
	f = filter
	f.Cached = new(bool)
	*f.Cached = true
	check(f, []string{"0"}, 1)
	f = filter
	f.Sort = "latency_desc"
	f.Limit = 2
	f.Offset = 1
	check(f, []string{"2", "1"}, 4)
	f = filter
	f.OmitPayloads = true
	rows, _, err := st.ListLogs(context.Background(), f)
	if err != nil || len(rows) != 4 {
		t.Fatalf("payload-free query: %v", err)
	}
	for _, row := range rows {
		if row.RequestBody != "" || row.ResponseBody != "" {
			t.Fatal("payload queried for export")
		}
	}
	f.Sort = "timestamp; DROP TABLE request_logs"
	if _, _, err := st.ListLogs(context.Background(), f); err == nil {
		t.Fatal("unknown sort accepted")
	}
	facets, err := st.LogFacets(context.Background(), filter)
	if err != nil || !reflect.DeepEqual(facets.Providers, []string{"alpha"}) || !reflect.DeepEqual(facets.Models, []string{"literal-100%_x", "model"}) || len(facets.Statuses) != 4 || !reflect.DeepEqual(facets.Endpoints, []string{"chat_completions"}) {
		t.Fatalf("facets=%+v err=%v", facets, err)
	}
}

func testDashboardMetrics(t *testing.T, st *Store) {
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	statuses := []string{"success", "denied", "limited", "error"}
	for i, status := range statuses {
		insertDashboardLog(t, st, RequestLog{ID: fmt.Sprint(i), Timestamp: base.Add(time.Duration(i) * time.Millisecond), APIKeyID: "mine", Status: status, LatencyMs: float64(i + 1), Cost: .25, TotalTokens: 10, Cached: i == 0})
	}
	insertDashboardLog(t, st, RequestLog{ID: "last", Timestamp: base.Add(2 * time.Minute), APIKeyID: "mine", Status: "success", LatencyMs: 5})
	insertDashboardLog(t, st, RequestLog{ID: "until-exclusive", Timestamp: base.Add(4 * time.Minute), APIKeyID: "mine", Status: "success", LatencyMs: 999})
	insertDashboardLog(t, st, RequestLog{ID: "other", Timestamp: base, APIKeyID: "other", Status: "success", LatencyMs: 999})
	buckets, err := st.MetricsTimeseries(ctx, base, base.Add(4*time.Minute), time.Minute, "mine")
	if err != nil || len(buckets) != 4 {
		t.Fatalf("buckets=%v err=%v", buckets, err)
	}
	first := buckets[0]
	if first.Requests != 4 || first.Allowed != 1 || first.Denied != 1 || first.Limited != 1 || first.Errors != 1 || first.CacheHits != 1 || first.Tokens != 40 || first.Cost != 1 || first.AvgLatencyMs != 2.5 {
		t.Fatalf("first bucket=%+v", first)
	}
	for i, bucket := range buckets {
		if !bucket.Start.Equal(base.Add(time.Duration(i)*time.Minute)) || bucket.Start.Location() != time.UTC {
			t.Fatalf("unaligned bucket=%+v", bucket)
		}
	}
	if buckets[1].Requests != 0 || buckets[1].Cost != 0 || buckets[2].Requests != 1 || buckets[3].Requests != 0 {
		t.Fatalf("zero fill = %+v", buckets)
	}
	if _, err := st.MetricsTimeseries(ctx, base, base.Add(201*time.Second), time.Second, ""); err == nil {
		t.Fatal("more than 200 buckets accepted")
	}
	if _, err := st.MetricsTimeseries(ctx, base, base, time.Second, ""); err == nil {
		t.Fatal("empty range accepted")
	}
	if _, err := st.MetricsTimeseries(ctx, base, base.Add(time.Second), time.Millisecond, ""); err == nil {
		t.Fatal("sub-second bucket accepted")
	}
	aligned, err := st.MetricsTimeseries(ctx, base.Add(15*time.Second), base.Add(75*time.Second), time.Minute, "mine")
	if err != nil || len(aligned) != 2 || !aligned[0].Start.Equal(base) || !aligned[1].Start.Equal(base.Add(time.Minute)) {
		t.Fatalf("partial buckets = %+v, %v", aligned, err)
	}
	for i := 1; i <= 100; i++ {
		insertDashboardLog(t, st, RequestLog{ID: fmt.Sprintf("latency-%d", i), Timestamp: base.Add(time.Second), APIKeyID: "latency", LatencyMs: float64(i)})
	}
	stats, err := st.LatencyPercentiles(ctx, base, base.Add(2*time.Second), "latency")
	if err != nil || stats != (LatencyStats{Count: 100, AvgMs: 50.5, P50Ms: 50, P95Ms: 95, P99Ms: 99}) {
		t.Fatalf("percentiles = %+v err=%v", stats, err)
	}
	empty, err := st.LatencyPercentiles(ctx, base, base.Add(time.Second), "missing")
	if err != nil || empty != (LatencyStats{}) {
		t.Fatalf("empty stats = %+v err=%v", empty, err)
	}
}

func testDashboardEvents(t *testing.T, st *Store) {
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, key := range []string{"mine", "other"} {
		insertDashboardLog(t, st, RequestLog{ID: key, Timestamp: base, APIKeyID: key, Provider: "alpha", Model: "model", TraceID: "shared"})
		for i, action := range []string{"deny", "redact", "warn", "log", "fallback", "reroute", "retry"} {
			if err := st.InsertGuardrailEvent(ctx, GuardrailEvent{ID: fmt.Sprintf("%s-%d", key, i), RequestID: key, Timestamp: base.Add(time.Duration(i) * time.Millisecond), Type: "pii", Action: action, Matched: true, Details: "detail"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, event := range []GuardrailEvent{
		{ID: "not-matched", RequestID: "mine", Timestamp: base, Type: "pii", Action: "log", Matched: false},
		{ID: "orphan", RequestID: "absent", Timestamp: base, Type: "pii", Action: "deny", Matched: true},
		{ID: "until", RequestID: "mine", Timestamp: base.Add(time.Second), Type: "pii", Action: "deny", Matched: true},
	} {
		if err := st.InsertGuardrailEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	filter := GuardrailFilter{Since: base, Until: base.Add(time.Second), APIKeyID: "mine", Limit: 100}
	events, total, err := st.ListGuardrailEvents(ctx, filter)
	if err != nil || total != 8 || len(events) != 8 {
		t.Fatalf("events=%v total=%d err=%v", events, total, err)
	}
	for _, event := range events {
		if event.APIKeyID != "mine" || event.TraceID != "shared" || event.Provider != "alpha" || event.Model != "model" {
			t.Fatalf("join = %+v", event)
		}
	}
	f := filter
	f.Type = "pii"
	f.Action = "deny"
	f.RequestIDs = []string{"mine"}
	rows, total, err := st.ListGuardrailEvents(ctx, f)
	if err != nil || total != 1 || len(rows) != 1 {
		t.Fatalf("combined event filter = %v/%d/%v", rows, total, err)
	}
	f.TraceID = "unknown"
	rows, total, err = st.ListGuardrailEvents(ctx, f)
	if err != nil || total != 0 || len(rows) != 0 {
		t.Fatalf("trace filter = %v/%d/%v", rows, total, err)
	}
	summary, err := st.GuardrailSummary(ctx, filter)
	if err != nil || summary.Matched != 7 || !reflect.DeepEqual(summary.ByType, []GuardrailTypeCount{{Type: "pii", Count: 7}}) {
		t.Fatalf("summary = %+v/%v", summary, err)
	}
	for action, count := range summary.ByAction {
		if count != 1 {
			t.Fatalf("%s=%d", action, count)
		}
	}
	insertDashboardLog(t, st, RequestLog{ID: "mcp", Timestamp: base, Provider: "mcp", Model: "weather", APIKeyID: "mine", TraceID: "mcp-trace", Decision: "DENY", DecisionReason: "policy denied", Metadata: `{"mcp":{"agent_id":"agent","action":"call","target":"ops","rule":"denied-rule","latency_ms":2.5}}`})
	insertDashboardLog(t, st, RequestLog{ID: "legacy-mcp", Timestamp: base.Add(time.Millisecond), Provider: "mcp", Model: "legacy", APIKeyID: "mine", Metadata: ""})
	insertDashboardLog(t, st, RequestLog{ID: "foreign-mcp", Timestamp: base, Provider: "mcp", Model: "private", APIKeyID: "other"})
	decisions, total, err := st.ListMCPDecisions(ctx, LogFilter{Since: base, Until: base.Add(time.Second), APIKeyID: "mine", Sort: "time_asc", Limit: 1000})
	if err != nil || total != 2 || len(decisions) != 2 {
		t.Fatalf("decisions=%v/%d/%v", decisions, total, err)
	}
	if row := decisions[0]; row.AgentID != "agent" || row.Target != "ops" || row.Action != "call" || row.Rule != "denied-rule" || row.LatencyMs != 2.5 || row.TraceID != "mcp-trace" {
		t.Fatalf("decision metadata = %+v", row)
	}
	if row := decisions[1]; row.AgentID != "" || row.Target != "" || row.Rule != "" || row.Action != "" || row.LatencyMs != 0 {
		t.Fatalf("fabricated legacy MCP data: %+v", row)
	}
}

func dashboardPtr[T any](v T) *T { return &v }

func testDashboardKeys(t *testing.T, st *Store) {
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expiry, lastUsed := base.AddDate(1, 0, 0), base.Add(time.Second)
	key := APIKey{ID: "mine", Name: "initial", KeyHash: "secret-hash", KeyPrefix: "display", CreatedAt: base, ExpiresAt: &expiry, LastUsedAt: &lastUsed, Enabled: true, Role: "owner", WorkspaceID: "tenant", BudgetUSD: 10, MonthlyCostBudget: 10, MonthlyTokenBudget: 100, SpendUSD: 9, RateLimit: 30, Metadata: map[string]string{"keep": "value"}}
	if err := st.InsertAPIKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	before, err := st.GetAPIKeyByID(ctx, "mine")
	if err != nil {
		t.Fatal(err)
	}
	update := APIKeyUpdate{Name: dashboardPtr("renamed"), Enabled: dashboardPtr(false), RateLimit: dashboardPtr(0), MonthlyCostBudget: dashboardPtr(0.0), MonthlyTokenBudget: dashboardPtr(0), RequestsPerMinute: dashboardPtr(50), TokensPerMinute: dashboardPtr(500), USDPerDay: dashboardPtr(2.5), Role: dashboardPtr("admin"), WorkspaceID: dashboardPtr("new")}
	after, err := st.UpdateAPIKey(ctx, "mine", update)
	if err != nil {
		t.Fatal(err)
	}
	want := before
	want.Name = "renamed"
	want.Enabled = false
	want.RateLimit = 0
	want.BudgetUSD = 0
	want.MonthlyCostBudget = 0
	want.MonthlyTokenBudget = 0
	want.RequestsPerMinute = 50
	want.TokensPerMinute = 500
	want.USDPerDay = 2.5
	want.Role = "admin"
	want.WorkspaceID = "new"
	if !reflect.DeepEqual(after, want) {
		t.Fatalf("partial update changed protected state: got=%+v want=%+v", after, want)
	}
	if err := st.UpdateAPIKeyHash(ctx, "mine", "rotated"); err != nil {
		t.Fatal(err)
	}
	want.KeyHash = "rotated"
	after, err = st.GetAPIKeyByID(ctx, "mine")
	if err != nil || !reflect.DeepEqual(after, want) {
		t.Fatalf("hash rotation: %+v / %v", after, err)
	}
	if err := st.InsertBootstrapAPIKeyIfAbsent(ctx, APIKey{ID: "mine", Name: "reset", KeyHash: "wrong", CreatedAt: base, Enabled: true, Role: "owner"}); err != nil {
		t.Fatal(err)
	}
	after, err = st.GetAPIKeyByID(ctx, "mine")
	if err != nil || !reflect.DeepEqual(after, want) {
		t.Fatal("bootstrap conflict overwrote administrator state")
	}
	names, err := st.APIKeyNames(ctx, []string{"mine", "mine", "missing", ""})
	if err != nil || !reflect.DeepEqual(names, map[string]string{"mine": "renamed"}) {
		t.Fatalf("key names = %v/%v", names, err)
	}
	for _, usage := range []struct {
		id     string
		cost   float64
		tokens int
		state  string
	}{{"ok", 7.9, 79, "ok"}, {"near", 8, 80, "near"}, {"exceeded", 10, 100, "exceeded"}, {"unused", 0, 0, "ok"}} {
		if err := st.InsertAPIKey(ctx, APIKey{ID: usage.id, Name: usage.id, KeyHash: usage.id, CreatedAt: base, MonthlyCostBudget: 10, MonthlyTokenBudget: 100}); err != nil {
			t.Fatal(err)
		}
		if usage.id != "unused" {
			insertDashboardLog(t, st, RequestLog{ID: usage.id, APIKeyID: usage.id, Timestamp: base, Decision: "ALLOW", Cost: usage.cost, TotalTokens: usage.tokens})
		}
		insertDashboardLog(t, st, RequestLog{ID: usage.id + "-deny", APIKeyID: usage.id, Timestamp: base, Decision: "DENY", Cost: 99, TotalTokens: 999})
		rows, err := st.BudgetUsageByKey(ctx, base, base.Add(time.Second), usage.id)
		if err != nil || len(rows) != 1 || rows[0].State != usage.state || math.Abs(rows[0].CostUSD-usage.cost) > 1e-6 || rows[0].Tokens != usage.tokens || math.Abs(rows[0].CostPct-usage.cost/10) > 1e-6 || rows[0].TokenPct != float64(usage.tokens)/100 {
			t.Fatalf("usage %s: %+v/%v", usage.id, rows, err)
		}
	}
	rows, err := st.BudgetUsageByKey(ctx, base, base.Add(time.Second), "")
	if err != nil || len(rows) != 5 {
		t.Fatalf("grouped usage = %+v/%v", rows, err)
	}
}

func testDashboardTrace(t *testing.T, st *Store) {
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, key := range []string{"mine", "other"} {
		insertDashboardLog(t, st, RequestLog{ID: key, Timestamp: base, TraceID: "shared", APIKeyID: key})
		if err := st.InsertGuardrailEvent(ctx, GuardrailEvent{ID: key, RequestID: key, Timestamp: base, Matched: true}); err != nil {
			t.Fatal(err)
		}
		if err := st.InsertShadowLog(ctx, ShadowLog{ID: key, RequestID: key, TraceID: "shared", Timestamp: base}); err != nil {
			t.Fatal(err)
		}
		if err := st.InsertFeedback(ctx, Feedback{ID: key, RequestID: key, TraceID: "shared", Timestamp: base}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.InsertFeedback(ctx, Feedback{ID: "legacy", TraceID: "shared", Timestamp: base}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertShadowLog(ctx, ShadowLog{ID: "orphan", RequestID: "missing", TraceID: "shared", Timestamp: base}); err != nil {
		t.Fatal(err)
	}
	insertDashboardLog(t, st, RequestLog{ID: "old", Timestamp: base.Add(-time.Hour), TraceID: "shared", APIKeyID: "mine"})
	mine, err := st.GetTraceInRange(ctx, "shared", "mine", base, base.Add(time.Second))
	if err != nil || len(mine.Logs) != 1 || len(mine.GuardrailEvents) != 1 || len(mine.ShadowLogs) != 1 || len(mine.Feedback) != 1 {
		t.Fatalf("scoped trace = %+v / %v", mine, err)
	}
	if mine.Logs[0].ID != "mine" || mine.GuardrailEvents[0].RequestID != "mine" || mine.ShadowLogs[0].RequestID != "mine" || mine.Feedback[0].RequestID != "mine" {
		t.Fatal("shared trace leaked a foreign key's rows")
	}
	all, err := st.GetTraceInRange(ctx, "shared", "", base, base.Add(time.Second))
	if err != nil || len(all.Logs) != 2 || len(all.GuardrailEvents) != 2 || len(all.ShadowLogs) != 2 || len(all.Feedback) != 3 {
		t.Fatalf("admin trace = %+v/%v", all, err)
	}
	if _, err := st.GetTraceInRange(ctx, "shared", "missing", base, base.Add(time.Second)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown trace err = %v", err)
	}
}

func testDashboardExportCap(t *testing.T, st *Store) {
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := st.exec(ctx, `WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < 10001)
		INSERT INTO request_logs (id, timestamp, provider, model, api_key_id, status, request_body, response_body, decision)
		SELECT 'bulk-' || CAST(n AS TEXT), ?, 'alpha', 'model-' || CAST(n AS TEXT), 'mine', 'success', 'PRIVATE', 'PRIVATE', 'ALLOW' FROM seq`, base.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	rows, total, err := st.ListLogs(ctx, LogFilter{Since: base, Until: base.Add(time.Second), Limit: 10001, OmitPayloads: true})
	if err != nil || total != 10001 || len(rows) != MaxExportRows {
		t.Fatalf("export cap: rows=%d total=%d err=%v", len(rows), total, err)
	}
	for _, row := range rows {
		if row.RequestBody != "" || row.ResponseBody != "" {
			t.Fatal("export retrieved payload")
		}
	}
	facets, err := st.LogFacets(ctx, LogFilter{Since: base, Until: base.Add(time.Second)})
	if err != nil || len(facets.Models) != 200 {
		t.Fatalf("facet cap: %d/%v", len(facets.Models), err)
	}
}

func TestAPIKeyUpdateValidation(t *testing.T) {
	for _, update := range []APIKeyUpdate{
		{Name: dashboardPtr(" ")}, {Name: dashboardPtr("bad\x00name")}, {Name: dashboardPtr("bad\xffname")}, {WorkspaceID: dashboardPtr("bad\x00id")}, {Name: dashboardPtr(strings.Repeat("x", 257))}, {WorkspaceID: dashboardPtr(strings.Repeat("x", 257))},
		{Role: dashboardPtr("editor")}, {Role: dashboardPtr("OWNER")}, {RateLimit: dashboardPtr(-1)},
		{RequestsPerMinute: dashboardPtr(-1)}, {TokensPerMinute: dashboardPtr(-1)}, {MonthlyTokenBudget: dashboardPtr(-1)},
		{BudgetUSD: dashboardPtr(-1.0)}, {MonthlyCostBudget: dashboardPtr(math.Inf(1))}, {USDPerDay: dashboardPtr(math.NaN())},
		{RateLimit: dashboardPtr(int(math.MaxInt32) + 1)},
	} {
		if err := update.Validate(); err == nil {
			t.Fatalf("invalid update accepted: %+v", update)
		}
	}
	for _, role := range []string{"viewer", "member", "admin", "owner"} {
		if err := (APIKeyUpdate{Role: &role, RateLimit: dashboardPtr(0), USDPerDay: dashboardPtr(0.0)}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDashboardConcurrentBootstrap(t *testing.T) {
	for _, postgres := range []bool{false, true} {
		t.Run(fmt.Sprint(postgres), func(t *testing.T) {
			st := newMigrationTestStore(t, postgres, true)
			ctx := context.Background()
			key := APIKey{ID: "bootstrap-admin", Name: "bootstrap", KeyHash: "hash", KeyPrefix: "display", CreatedAt: time.Now().UTC(), Enabled: true, Role: "owner"}
			var wg sync.WaitGroup
			failures := make(chan error, 16)
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); failures <- st.InsertBootstrapAPIKeyIfAbsent(ctx, key) }()
			}
			wg.Wait()
			close(failures)
			for err := range failures {
				if err != nil {
					t.Fatal(err)
				}
			}
			keys, err := st.ListAPIKeys(ctx)
			if err != nil || len(keys) != 1 || keys[0].KeyHash != "" || !keys[0].Enabled {
				t.Fatalf("concurrent bootstrap = %+v/%v", keys, err)
			}
			stored, err := st.GetAPIKeyByID(ctx, key.ID)
			if err != nil || stored.KeyHash != "hash" {
				t.Fatalf("stored bootstrap hash changed: %v", err)
			}
		})
	}
}
