package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func testReleaseFields(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	lastUsed := time.Date(2026, 10, 9, 12, 30, 0, 123456789, time.UTC)
	key := APIKey{
		ID: "release_key", Name: "release", KeyHash: "release_hash", KeyPrefix: "test-prefix",
		CreatedAt: lastUsed, Enabled: true, RequestsPerMinute: 23,
		TokensPerMinute: 4500, USDPerDay: 2.75, LastUsedAt: &lastUsed,
	}
	if err := st.InsertAPIKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	assertKey := func(got APIKey, err error) {
		t.Helper()
		if err != nil || got.RequestsPerMinute != key.RequestsPerMinute || got.TokensPerMinute != key.TokensPerMinute || got.USDPerDay != key.USDPerDay || got.LastUsedAt == nil || !got.LastUsedAt.Equal(*key.LastUsedAt) {
			t.Fatalf("key fields = %+v, err=%v", got, err)
		}
	}
	assertKey(st.GetAPIKeyByHash(ctx, key.KeyHash))
	assertKey(st.GetAPIKeyByID(ctx, key.ID))
	keys, err := st.ListAPIKeys(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("key listing = %v, err=%v", keys, err)
	}
	assertKey(keys[0], nil)

	key.RequestsPerMinute, key.TokensPerMinute, key.USDPerDay = 0, 0, 0
	updated := lastUsed.Add(time.Minute)
	key.LastUsedAt = &updated
	if err := st.InsertAPIKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	assertKey(st.GetAPIKeyByID(ctx, key.ID))
	used := updated.Add(time.Minute)
	if err := st.TouchAPIKeyLastUsed(ctx, key.ID, used); err != nil {
		t.Fatal(err)
	}
	// Control-plane edits and bootstrap upserts must not clear last_used_at.
	key.LastUsedAt = nil
	if err := st.InsertAPIKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	key.LastUsedAt = &used
	assertKey(st.GetAPIKeyByID(ctx, key.ID))

	entry := RequestLog{
		ID: "release_request", Timestamp: lastUsed, TraceID: "release_trace", APIKeyID: key.ID,
		Provider: "openai", Model: "test", Status: "success", CostSource: "override",
		CachedTokens: 123, ReasoningTokens: 456, TotalTokens: 700,
	}
	if err := st.InsertLog(WithRequestEndpoint(ctx, "/v1/responses"), entry); err != nil {
		t.Fatal(err)
	}
	assertLog := func(logs []RequestLog, err error) {
		t.Helper()
		if err != nil || len(logs) != 1 {
			t.Fatalf("log fields = %v, err=%v", logs, err)
		}
		got := logs[0]
		if got.Endpoint != "responses" || got.CostSource != entry.CostSource || got.CachedTokens != entry.CachedTokens || got.ReasoningTokens != entry.ReasoningTokens {
			t.Fatalf("log fields = %+v", got)
		}
	}
	logs, _, err := st.ListLogs(ctx, 10, 0, "", "")
	assertLog(logs, err)
	logs, _, err = st.ListLogsForAPIKey(ctx, key.ID, 10, 0, "", "")
	assertLog(logs, err)
	trace, err := st.GetTrace(ctx, entry.TraceID, key.ID)
	assertLog(trace.Logs, err)

	if err := st.InsertAPIKey(ctx, APIKey{ID: "unused", Name: "unused", KeyHash: "unused_hash", CreatedAt: lastUsed}); err != nil {
		t.Fatal(err)
	}
	unused, err := st.GetAPIKeyByID(ctx, "unused")
	if err != nil || unused.LastUsedAt != nil {
		t.Fatalf("unused key = %+v, err=%v", unused, err)
	}
	data, err := json.Marshal(unused)
	if err != nil || !strings.Contains(string(data), `"last_used_at":null`) || !strings.Contains(string(data), `"usd_per_day":0`) {
		t.Fatalf("key JSON must expose nullable last use and zero limits: %s, err=%v", data, err)
	}
}

func newMigrationTestStore(t *testing.T, postgres, migrate bool) *Store {
	t.Helper()
	var st *Store
	var err error
	if postgres {
		st, err = OpenPostgres(testPostgresDSN(t), WithMigrateOnStart(migrate))
	} else {
		st, err = New(t.TempDir(), WithMigrateOnStart(migrate))
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// Every Postgres case owns a schema, so tests never drop or truncate operator
// tables or interfere with other packages using the same test database.
func testPostgresDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("OMNISWITCH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OMNISWITCH_TEST_DATABASE_URL is not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "osw_store_test_" + strings.ToLower(rand.Text())
	if _, err := db.Exec("CREATE SCHEMA " + schema); err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec("DROP SCHEMA " + schema + " CASCADE"); err != nil {
			t.Errorf("clean up Postgres schema: %v", err)
		}
		db.Close()
	})
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		query := u.Query()
		query.Set("search_path", schema)
		u.RawQuery = query.Encode()
		return u.String()
	}
	return dsn + " search_path=" + schema
}
