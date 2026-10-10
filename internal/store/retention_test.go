package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRetentionBoundaries(t *testing.T) {
	for _, postgres := range []bool{false, true} {
		name := "sqlite"
		if postgres {
			name = "postgres"
		}
		t.Run(name, func(t *testing.T) {
			st := newMigrationTestStore(t, postgres, true)
			ctx := context.Background()
			at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
			policy := DefaultRetentionPolicy()
			policy.LimitedAndDenied = 3 * 24 * time.Hour
			policy.BatchSize = 2 // Exercise multiple batches, not just a LIMIT.
			for _, class := range []struct {
				name, status, decision string
				duration               time.Duration
			}{
				{"ordinary", "success", "ALLOW", policy.RequestLogs},
				{"limited", "limited", "ALLOW", policy.LimitedAndDenied},
				{"denied", "error", "DENY", policy.LimitedAndDenied},
			} {
				for i, offset := range []time.Duration{-time.Second, 0, time.Second} {
					insertRetentionLog(t, st, RequestLog{ID: fmt.Sprintf("%s_%d", class.name, i), Timestamp: at.Add(-class.duration + offset), Status: class.status, Decision: class.decision})
				}
			}
			for i, offset := range []time.Duration{-time.Second, 0, time.Second} {
				insertRetentionLog(t, st, RequestLog{ID: fmt.Sprintf("payload_%d", i), Timestamp: at.Add(-policy.Payloads + offset), RequestBody: "prompt", ResponseBody: "answer"})
				if err := st.InsertGuardrailEvent(ctx, GuardrailEvent{ID: fmt.Sprintf("guard_%d", i), Timestamp: at.Add(-policy.GuardrailEvents + offset)}); err != nil {
					t.Fatal(err)
				}
				if err := st.InsertShadowLog(ctx, ShadowLog{ID: fmt.Sprintf("shadow_%d", i), Timestamp: at.Add(-policy.ShadowRequests + offset)}); err != nil {
					t.Fatal(err)
				}
				expires := at.Add(offset)
				if err := st.InsertSemanticCache(ctx, CacheEntry{ID: fmt.Sprintf("cache_%d", i), CreatedAt: at, ExpiresAt: &expires}); err != nil {
					t.Fatal(err)
				}
			}
			for _, id := range []string{"cache_forever_null", "cache_forever_empty"} {
				if err := st.InsertSemanticCache(ctx, CacheEntry{ID: id, CreatedAt: at}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := st.exec(ctx, "UPDATE semantic_cache SET expires_at = '' WHERE id = 'cache_forever_empty'"); err != nil {
				t.Fatal(err)
			}
			insertRetentionLog(t, st, RequestLog{ID: "old_body", Timestamp: at.Add(-31 * 24 * time.Hour), RequestBody: "old prompt", ResponseBody: "old answer"})
			installPayloadOrderCheck(t, st)
			seedRetentionDurableRows(t, st, at.Add(-365*24*time.Hour))

			result, err := st.SweepRetention(ctx, policy, at)
			if err != nil || result.Skipped || result.PayloadsCleared != 2 || result.Deleted != (RetentionDeleted{RequestLogs: 4, GuardrailEvents: 1, ShadowRequests: 1, SemanticCache: 2}) {
				t.Fatalf("sweep result=%+v, err=%v", result, err)
			}
			for table, want := range map[string]int{"request_logs": 9, "guardrail_events": 2, "shadow_requests": 2, "semantic_cache": 3, "feedback": 1, "api_keys": 1, "virtual_keys": 1, "prompts": 1, "gateway_configs": 1} {
				assertRetentionCount(t, st, table, want)
			}
			for i := 0; i < 3; i++ {
				var request, response *string
				if err := st.queryRow(ctx, "SELECT request_body, response_body FROM request_logs WHERE id = ?", fmt.Sprintf("payload_%d", i)).Scan(&request, &response); err != nil {
					t.Fatal(err)
				}
				if i == 0 && (request != nil || response != nil) {
					t.Fatal("expired payloads were not nulled")
				}
				if i != 0 && (request == nil || *request != "prompt" || response == nil || *response != "answer") {
					t.Fatalf("boundary/newer payload was changed: %v/%v", request, response)
				}
			}
			result, err = st.SweepRetention(ctx, policy, at)
			if err != nil || result.PayloadsCleared != 0 || result.Deleted != (RetentionDeleted{}) {
				t.Fatalf("second sweep was not a no-op: %+v, err=%v", result, err)
			}
		})
	}
}

func insertRetentionLog(t *testing.T, st *Store, entry RequestLog) {
	t.Helper()
	if err := st.InsertLog(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
}

func assertRetentionCount(t *testing.T, st *Store, table string, want int) {
	t.Helper()
	var count int
	if err := st.queryRow(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != want {
		t.Fatalf("%s count=%d, want=%d, err=%v", table, count, want, err)
	}
}

func installPayloadOrderCheck(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	query := `CREATE TRIGGER cleared_before_delete BEFORE DELETE ON request_logs
		WHEN COALESCE(OLD.request_body, '') != '' OR COALESCE(OLD.response_body, '') != ''
		BEGIN SELECT RAISE(ABORT, 'payloads must be cleared before deleting'); END`
	if st.pg {
		if _, err := st.exec(ctx, `CREATE FUNCTION check_cleared_payloads() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF COALESCE(OLD.request_body, '') != '' OR COALESCE(OLD.response_body, '') != '' THEN
					RAISE EXCEPTION 'payloads must be cleared before deleting';
				END IF;
				RETURN OLD;
			END $$`); err != nil {
			t.Fatal(err)
		}
		query = `CREATE TRIGGER cleared_before_delete BEFORE DELETE ON request_logs FOR EACH ROW EXECUTE FUNCTION check_cleared_payloads()`
	}
	if _, err := st.exec(ctx, query); err != nil {
		t.Fatal(err)
	}
}

func seedRetentionDurableRows(t *testing.T, st *Store, at time.Time) {
	t.Helper()
	ctx := context.Background()
	for _, err := range []error{
		st.InsertFeedback(ctx, Feedback{ID: "durable_feedback", RequestID: "old_body", Timestamp: at, Comment: "keep feedback"}),
		st.InsertAPIKey(ctx, APIKey{ID: "durable_key", Name: "keep key", KeyHash: "retention_fixture_hash", KeyPrefix: "fixture", CreatedAt: at, Enabled: true}),
		st.InsertVirtualKey(ctx, VirtualKeyRecord{ID: "durable_provider_key", Name: "keep provider key", ProviderType: "fixture", EncryptedKey: "fixture-ciphertext", CreatedAt: at, UpdatedAt: at, Enabled: true}),
		st.InsertPrompt(ctx, Prompt{ID: "durable_prompt", Name: "keep prompt", Template: "keep template", CreatedAt: at, UpdatedAt: at, Version: 1}),
		st.UpsertGatewayConfig(ctx, GatewayConfigRecord{ID: "durable_config", Name: "keep config", Body: "{}", CreatedAt: at, UpdatedAt: at, Enabled: true}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestRetentionZeroPoliciesAndPruneSafety(t *testing.T) {
	for _, postgres := range []bool{false, true} {
		name := "sqlite"
		if postgres {
			name = "postgres"
		}
		t.Run(name, func(t *testing.T) {
			for _, active := range []string{"none", "ordinary", "limited"} {
				t.Run(active, func(t *testing.T) {
					st := newMigrationTestStore(t, postgres, true)
					ctx := context.Background()
					at := time.Now().UTC()
					old := at.Add(-48 * time.Hour)
					policy := RetentionPolicy{SweepInterval: time.Hour, BatchSize: 1}
					if active == "ordinary" {
						policy.RequestLogs = time.Hour
					} else if active == "limited" {
						policy.LimitedAndDenied = time.Hour
					}
					insertRetentionLog(t, st, RequestLog{ID: "ordinary", Timestamp: old, RequestBody: "preserve payload"})
					insertRetentionLog(t, st, RequestLog{ID: "limited", Timestamp: old, Status: "limited"})
					insertRetentionLog(t, st, RequestLog{ID: "denied", Timestamp: old, Decision: "DENY"})
					if err := st.InsertSemanticCache(ctx, CacheEntry{ID: "expired", CreatedAt: old, ExpiresAt: &old}); err != nil {
						t.Fatal(err)
					}
					result, err := st.SweepRetention(ctx, policy, at)
					wantDeleted := int64(0)
					if active == "ordinary" {
						wantDeleted = 1
					} else if active == "limited" {
						wantDeleted = 2
					}
					if err != nil || result.Skipped != (active == "none") || result.Deleted.RequestLogs != wantDeleted || result.PayloadsCleared != 0 {
						t.Fatalf("result=%+v, err=%v", result, err)
					}
					assertRetentionCount(t, st, "request_logs", 3-int(wantDeleted))
					wantCache := 0
					if active == "none" {
						wantCache = 1
					}
					assertRetentionCount(t, st, "semantic_cache", wantCache)
					if active != "ordinary" {
						var body string
						if err := st.queryRow(ctx, "SELECT request_body FROM request_logs WHERE id = 'ordinary'").Scan(&body); err != nil || body != "preserve payload" {
							t.Fatalf("zero payload policy changed body=%q, err=%v", body, err)
						}
					}
					for _, target := range []struct{ table, column string }{
						{"feedback", "timestamp"}, {"api_keys", "created_at"}, {"virtual_keys", "created_at"}, {"prompts", "created_at"}, {"gateway_configs", "created_at"},
						{"request_logs", "timestamp OR 1=1"}, {"request_logs; DROP TABLE api_keys", "timestamp"},
					} {
						if count, err := st.Prune(ctx, target.table, target.column, at, 1); err == nil || count != 0 {
							t.Fatalf("unsafe target accepted: %+v count=%d err=%v", target, count, err)
						}
					}
				})
			}
		})
	}
}

func TestPruneTimestampParsingAndErrors(t *testing.T) {
	for _, postgres := range []bool{false, true} {
		name := "sqlite"
		if postgres {
			name = "postgres"
		}
		t.Run(name, func(t *testing.T) {
			st := newMigrationTestStore(t, postgres, true)
			ctx := context.Background()
			before := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
			for i, at := range []time.Time{before.Add(-time.Second), before, before.Add(100 * time.Millisecond), before.Add(time.Hour).In(time.FixedZone("offset", -2*60*60))} {
				insertRetentionLog(t, st, RequestLog{ID: fmt.Sprint(i), Timestamp: at})
			}
			if count, err := st.Prune(ctx, "request_logs", "timestamp", before, 1); err != nil || count != 1 {
				t.Fatalf("parsed timestamp pruning count=%d, err=%v", count, err)
			}
			assertRetentionCount(t, st, "request_logs", 3)
			for _, batch := range []int{0, -1} {
				if _, err := st.Prune(ctx, "request_logs", "timestamp", before, batch); err == nil {
					t.Fatal("invalid prune batch accepted")
				}
				if _, err := st.NullPayloads(ctx, before, batch); err == nil {
					t.Fatal("invalid payload batch accepted")
				}
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := st.Prune(canceled, "request_logs", "timestamp", before, 1); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled prune returned %v", err)
			}
			if _, err := st.NullPayloads(canceled, before, 1); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled payload clearing returned %v", err)
			}
		})
	}
}

func TestRetentionPartialFailure(t *testing.T) {
	for _, postgres := range []bool{false, true} {
		name := "sqlite"
		if postgres {
			name = "postgres"
		}
		t.Run(name, func(t *testing.T) {
			st := newMigrationTestStore(t, postgres, true)
			at := time.Now().UTC()
			insertRetentionLog(t, st, RequestLog{ID: "old", Timestamp: at.Add(-31 * 24 * time.Hour), RequestBody: "expired"})
			if _, err := st.exec(context.Background(), "DROP TABLE guardrail_events"); err != nil {
				t.Fatal(err)
			}
			result, err := st.SweepRetention(context.Background(), DefaultRetentionPolicy(), at)
			if err == nil || !strings.Contains(err.Error(), "guardrail_events") || result.PayloadsCleared != 1 || result.Deleted.RequestLogs != 1 {
				t.Fatalf("partial counts lost: %+v, err=%v", result, err)
			}
		})
	}
}

func TestRetentionWaitsForSchemaAndRejectsInvalidPolicies(t *testing.T) {
	st := newMigrationTestStore(t, false, false)
	policy := DefaultRetentionPolicy()
	if _, err := st.SweepRetention(context.Background(), policy, time.Now()); err == nil || !strings.Contains(err.Error(), "schema_pending") {
		t.Fatalf("pending schema sweep returned %v", err)
	}
	for _, field := range []*time.Duration{&policy.RequestLogs, &policy.Payloads, &policy.GuardrailEvents, &policy.ShadowRequests, &policy.LimitedAndDenied, &policy.SweepInterval} {
		original := *field
		*field = -time.Second
		if _, err := st.SweepRetention(context.Background(), policy, time.Now()); err == nil {
			t.Fatal("negative retention accepted")
		}
		*field = original
	}
	policy.BatchSize = 0
	if err := policy.Validate(); err == nil {
		t.Fatal("zero retention batch accepted")
	}
}
