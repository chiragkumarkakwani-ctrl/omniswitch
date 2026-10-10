package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/gateway"
	"github.com/omniswitch-dev/omniswitch/internal/provider"
	"github.com/omniswitch-dev/omniswitch/internal/store"
)

// Item 7 of console-api-contract.md, including every key and JSON leaf type.
const retentionContractFixture = `{
  "policy": {"request_logs":2592000,"payloads":604800,"guardrail_events":2592000,"shadow_requests":604800,"limited_and_denied":2592000},
  "sweep_interval_seconds":3600,
  "payload_logging":false,
  "last_run": {
    "started_at":"2026-10-09T22:00:00Z", "finished_at":"2026-10-09T22:00:02Z",
    "deleted":{"request_logs":1200,"guardrail_events":90,"shadow_requests":40,"semantic_cache":12},
    "payloads_cleared":310,"error":""
  },
  "next_run":"2026-10-09T23:00:00Z"
}`

func TestRetentionAPIContractAndConfigSummary(t *testing.T) {
	st := newTestStore(t)
	settings := retentionTestSettings(t)
	gw := gateway.New(nil, nil, st, nil)
	gw.SetLogPayloads(settings.logPayloads)
	controller := newRetentionController(st, settings.retention, gw.LogPayloads)
	mux := http.NewServeMux()
	registerRetentionRoutes(mux, controller)
	registerInfoRoutes(mux, settings, provider.NewRegistry(), st)

	before := retentionRequest(t, mux, http.MethodGet, "/api/retention", http.StatusOK)
	assertRetentionContract(t, before, false)
	if before["payload_logging"] != false {
		t.Fatal("payload_logging did not reflect disabled log_payloads")
	}
	config := retentionRequest(t, mux, http.MethodGet, "/api/config", http.StatusOK)
	if !reflect.DeepEqual(config["retention"], before["policy"]) {
		t.Fatalf("config retention differs from policy: %v != %v", config["retention"], before["policy"])
	}

	ctx := context.Background()
	at := time.Now().UTC()
	for _, row := range []store.RequestLog{
		{ID: "expired", Timestamp: at.Add(-31 * 24 * time.Hour), RequestBody: "old", ResponseBody: "old answer"},
		{ID: "payload_only", Timestamp: at.Add(-8 * 24 * time.Hour), RequestBody: "prompt", ResponseBody: "answer"},
	} {
		if err := st.InsertLog(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.InsertGuardrailEvent(ctx, store.GuardrailEvent{ID: "guard", Timestamp: at.Add(-31 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertShadowLog(ctx, store.ShadowLog{ID: "shadow", Timestamp: at.Add(-8 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	expired := at.Add(-time.Hour)
	if err := st.InsertSemanticCache(ctx, store.CacheEntry{ID: "cache", CreatedAt: expired, ExpiresAt: &expired}); err != nil {
		t.Fatal(err)
	}
	after := retentionRequest(t, mux, http.MethodPost, "/api/retention/sweep", http.StatusOK)
	assertRetentionContract(t, after, true)
	run := after["last_run"].(map[string]any)
	if run["payloads_cleared"] != float64(2) || run["error"] != "" {
		t.Fatalf("unexpected last_run: %v", run)
	}
	for name, count := range run["deleted"].(map[string]any) {
		if count != float64(1) {
			t.Errorf("deleted.%s=%v, want 1", name, count)
		}
	}
	logs, _, err := st.ListLogs(ctx, 10, 0, "", "")
	if err != nil || len(logs) != 1 || logs[0].ID != "payload_only" || logs[0].RequestBody != "" || logs[0].ResponseBody != "" {
		t.Fatalf("payload policy removed the row or retained bodies: %+v, err=%v", logs, err)
	}
	gw.SetLogPayloads(true) // The endpoint reports live hot-reloaded state.
	live := retentionRequest(t, mux, http.MethodGet, "/api/retention", http.StatusOK)
	if live["payload_logging"] != true {
		t.Fatal("payload_logging did not reflect live log_payloads")
	}
}

func TestRetentionDisabledAndFailureShapes(t *testing.T) {
	st := newTestStore(t)
	policy := store.RetentionPolicy{SweepInterval: time.Hour, BatchSize: 5000}
	controller := newRetentionController(st, policy, func() bool { return false })
	mux := http.NewServeMux()
	registerRetentionRoutes(mux, controller)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		path := "/api/retention"
		if method == http.MethodPost {
			path += "/sweep"
		}
		decoded := retentionRequest(t, mux, method, path, http.StatusOK)
		assertRetentionContract(t, decoded, false)
		for name, value := range decoded["policy"].(map[string]any) {
			if value != float64(0) {
				t.Fatalf("disabled policy.%s=%v", name, value)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	select {
	case <-controller.start(ctx):
	case <-time.After(time.Second):
		t.Fatal("disabled sweeper started a timer")
	}
	controller.policy = store.DefaultRetentionPolicy()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	decoded := retentionRequest(t, mux, http.MethodPost, "/api/retention/sweep", http.StatusServiceUnavailable)
	assertRetentionContract(t, decoded, true)
	if decoded["last_run"].(map[string]any)["error"] == "" {
		t.Fatal("failed sweep did not report an error")
	}
	retentionRequest(t, mux, http.MethodPost, "/api/retention", http.StatusMethodNotAllowed)
	retentionRequest(t, mux, http.MethodGet, "/api/retention/sweep", http.StatusMethodNotAllowed)
}

func retentionRequest(t *testing.T, handler http.Handler, method, path string, wantStatus int) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	if rec.Code != wantStatus {
		t.Fatalf("%s %s status=%d want=%d body=%s", method, path, rec.Code, wantStatus, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func assertRetentionContract(t *testing.T, decoded map[string]any, hasRun bool) {
	t.Helper()
	var expected map[string]any
	if err := json.Unmarshal([]byte(retentionContractFixture), &expected); err != nil {
		t.Fatal(err)
	}
	if !hasRun {
		expected["last_run"], expected["next_run"] = nil, nil
	}
	assertRetentionJSONTypes(t, "retention", expected, decoded)
	if hasRun {
		run := decoded["last_run"].(map[string]any)
		for _, value := range []any{run["started_at"], run["finished_at"], decoded["next_run"]} {
			if _, err := time.Parse(time.RFC3339Nano, value.(string)); err != nil {
				t.Fatalf("invalid retention timestamp %v: %v", value, err)
			}
		}
	}
}

func assertRetentionJSONTypes(t *testing.T, path string, expected, actual any) {
	t.Helper()
	if object, ok := expected.(map[string]any); ok {
		got, ok := actual.(map[string]any)
		if !ok || len(got) != len(object) {
			t.Fatalf("%s keys/types differ: got=%v expected=%v", path, actual, expected)
		}
		for name, value := range object {
			child, ok := got[name]
			if !ok {
				t.Fatalf("%s missing %s", path, name)
			}
			assertRetentionJSONTypes(t, path+"."+name, value, child)
		}
	} else if reflect.TypeOf(expected) != reflect.TypeOf(actual) {
		t.Fatalf("%s type=%T want=%T", path, actual, expected)
	}
}

func TestRetentionSweeperWaitsForIntervalAndStops(t *testing.T) {
	st := newTestStore(t)
	policy := store.DefaultRetentionPolicy()
	policy.SweepInterval = time.Second
	if err := st.InsertLog(context.Background(), store.RequestLog{ID: "expired", Timestamp: time.Now().UTC().Add(-31 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	controller := newRetentionController(st, policy, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := controller.start(ctx)
	defer func() { cancel(); <-done }()
	logs, _, err := st.ListLogs(context.Background(), 10, 0, "", "")
	if err != nil || len(logs) != 1 || controller.snapshot().LastRun != nil || controller.snapshot().NextRun != nil {
		t.Fatalf("sweep ran at startup: logs=%v, err=%v, status=%+v", logs, err, controller.snapshot())
	}
	deadline := time.After(5 * time.Second)
	for controller.snapshot().LastRun == nil {
		select {
		case <-deadline:
			t.Fatal("short-interval sweeper did not run")
		case <-time.After(10 * time.Millisecond):
		}
	}
	status := controller.snapshot()
	if status.LastRun.Deleted.RequestLogs != 1 || status.LastRun.Error != "" || status.NextRun == nil {
		t.Fatalf("scheduled sweep result: %+v", status)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sweeper did not stop on cancellation")
	}
}

func TestRetentionLocalSweepHonorsCanceledWait(t *testing.T) {
	controller := newRetentionController(newTestStore(t), store.DefaultRetentionPolicy(), nil)
	controller.gate <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := controller.sweep(ctx); err != context.DeadlineExceeded {
		t.Fatalf("blocked sweep returned %v", err)
	}
	<-controller.gate
	if controller.snapshot().LastRun != nil {
		t.Fatal("a canceled wait claimed to run a sweep")
	}
}

func TestRetentionAPIRequiresViewerAndAdmin(t *testing.T) {
	st := newTestStore(t)
	settings := retentionTestSettings(t)
	settings.authEnabled = true
	settings.rateLimitRequests = 1000
	controller := newRetentionController(st, settings.retention, nil)
	mux := http.NewServeMux()
	registerRetentionRoutes(mux, controller)
	handler, closeLimiter, err := wrapGatewayMiddleware(settings, st, mux)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLimiter()
	for _, role := range []string{"viewer", "member", "admin", "owner"} {
		key := "retention-test-" + role
		if err := st.InsertAPIKey(context.Background(), store.APIKey{ID: role, Name: role, Role: role, KeyHash: fmt.Sprintf("%x", sha256.Sum256([]byte(key))), Enabled: true}); err != nil {
			t.Fatal(err)
		}
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			path := "/api/retention"
			want := http.StatusOK
			if method == http.MethodPost {
				path += "/sweep"
				if role == "viewer" || role == "member" {
					want = http.StatusForbidden
				}
			}
			req := httptest.NewRequest(method, path, nil)
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("X-OmniSwitch-Role", "owner") // A header cannot escalate the persisted role.
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != want {
				t.Errorf("%s %s as %s=%d want=%d body=%s", method, path, role, rec.Code, want, rec.Body.String())
			}
		}
	}
	retentionRequest(t, handler, http.MethodGet, "/api/retention", http.StatusUnauthorized)
}

func retentionTestSettings(t *testing.T) runtimeSettings {
	t.Helper()
	t.Setenv("OMNISWITCH_CONFIG", "")
	for _, suffix := range []string{"REQUEST_LOGS", "PAYLOADS", "GUARDRAIL_EVENTS", "SHADOW_REQUESTS", "LIMITED_AND_DENIED", "SWEEP_INTERVAL", "BATCH_SIZE"} {
		t.Setenv("OMNISWITCH_RETENTION_"+suffix, "")
	}
	settings, err := loadRuntimeSettings()
	if err != nil {
		t.Fatal(fmt.Errorf("load retention settings: %w", err))
	}
	return settings
}
