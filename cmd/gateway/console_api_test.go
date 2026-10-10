package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/admin"
	"github.com/omniswitch-dev/omniswitch/internal/audit"
	"github.com/omniswitch-dev/omniswitch/internal/dashboard"
	"github.com/omniswitch-dev/omniswitch/internal/eval"
	"github.com/omniswitch-dev/omniswitch/internal/feedback"
	"github.com/omniswitch-dev/omniswitch/internal/gateway"
	"github.com/omniswitch-dev/omniswitch/internal/gatewayconfig"
	"github.com/omniswitch-dev/omniswitch/internal/guardrail"
	"github.com/omniswitch-dev/omniswitch/internal/org"
	"github.com/omniswitch-dev/omniswitch/internal/policy"
	"github.com/omniswitch-dev/omniswitch/internal/prompt"
	"github.com/omniswitch-dev/omniswitch/internal/provider"
	mcpproxy "github.com/omniswitch-dev/omniswitch/internal/proxy"
	"github.com/omniswitch-dev/omniswitch/internal/router"
	"github.com/omniswitch-dev/omniswitch/internal/store"
	"github.com/omniswitch-dev/omniswitch/internal/vault"
)

const validConsoleConfig = "apiVersion: omniswitch.dev/v1\nkind: GatewayConfig\nroutes:\n  fake-model:\n    provider: openai\n"

type consoleFixture struct {
	handler  http.Handler
	store    *store.Store
	dash     *dashboard.Handler
	router   *router.Router
	settings runtimeSettings
	secrets  map[string]string
	audit    bytes.Buffer
}

func newConsoleFixture(t *testing.T, auth bool) *consoleFixture {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := &consoleFixture{store: st, secrets: map[string]string{}}
	fixture.settings = runtimeSettings{authEnabled: auth, listenAddr: "127.0.0.1:18170", rateLimitRequests: 10000, rateLimitWindow: time.Minute, mcpEnabled: true, mcpPolicyPath: "/private/policy.yaml", circuitBreakerFailures: 2, circuitBreakerCooldown: time.Minute}
	configPath := filepath.Join(t.TempDir(), "omniswitch.yaml")
	if err := os.WriteFile(configPath, []byte(validConsoleConfig), 0600); err != nil {
		t.Fatal(err)
	}
	fixture.settings.configPath = configPath
	ctx := context.Background()
	stamp := time.Now().UTC().Add(-time.Second)
	for _, role := range []string{"viewer", "member", "admin", "owner", "disposable"} {
		secret := "sk-osw-test-" + role + "-secret"
		fixture.secrets[role] = secret
		if err := st.InsertWorkspace(ctx, store.Workspace{ID: "workspace-" + role, OrganizationID: "organization-" + role, Name: "workspace-" + role, CreatedAt: stamp}); err != nil {
			t.Fatal(err)
		}
		keyRole := role
		if role == "disposable" {
			keyRole = "viewer"
		}
		if err := st.InsertAPIKey(ctx, store.APIKey{ID: role, Name: "personal-" + role, KeyHash: sha256Hex(secret), KeyPrefix: "display-" + role, Role: keyRole, WorkspaceID: "workspace-" + role, Enabled: true, CreatedAt: stamp, MonthlyCostBudget: 10, MonthlyTokenBudget: 1000}); err != nil {
			t.Fatal(err)
		}
		if role == "disposable" {
			continue
		}
		if err := st.InsertLog(ctx, store.RequestLog{ID: role + "-request", Timestamp: stamp, Provider: "fake", Model: "fake-model", APIKeyID: role, TraceID: "shared-trace", SessionID: "session", Endpoint: "chat_completions", Status: "success", Decision: "ALLOW", Cost: 1, TotalTokens: 10, LatencyMs: 12, RequestBody: "private-prompt-" + role, ResponseBody: "private-response-" + role, Metadata: "{}", CostSource: "catalog"}); err != nil {
			t.Fatal(err)
		}
		if err := st.InsertGuardrailEvent(ctx, store.GuardrailEvent{ID: role + "-guardrail", RequestID: role + "-request", Timestamp: stamp, Type: "pii", Action: "warn", Matched: true, Details: `email alice@example.com "api_key":"private-api-value" sk-osw-private-value Bearer private-bearer-value`}); err != nil {
			t.Fatal(err)
		}
		if err := st.InsertShadowLog(ctx, store.ShadowLog{ID: role + "-shadow", RequestID: role + "-request", Timestamp: stamp, TraceID: "shared-trace", PrimaryProvider: "fake", ShadowProvider: "shadow", Model: "fake-model", Status: "success"}); err != nil {
			t.Fatal(err)
		}
		if err := st.InsertFeedback(ctx, store.Feedback{ID: role + "-feedback", RequestID: role + "-request", Timestamp: stamp, TraceID: "shared-trace", Comment: "comment-" + role, Score: 1}); err != nil {
			t.Fatal(err)
		}
		if err := st.InsertLog(ctx, store.RequestLog{ID: role + "-mcp", Timestamp: stamp, Provider: "mcp", Model: "weather", APIKeyID: role, TraceID: "mcp-" + role, Endpoint: "mcp", Status: "success", Decision: "ALLOW", Metadata: fmt.Sprintf(`{"mcp":{"agent_id":%q,"action":"call","target":"ops","rule":"none","latency_ms":1}}`, role)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.InsertFeedback(ctx, store.Feedback{ID: "legacy", TraceID: "shared-trace", Timestamp: stamp, Comment: "ambiguous legacy feedback"}); err != nil {
		t.Fatal(err)
	}
	engine, err := policy.NewEngine(policy.Rule{Name: "hide-danger", Expression: `tool.name == "danger" && action.name == "list" && agent.role != "admin" && agent.role != "owner"`, Reason: "Hidden dangerous tool"}, policy.Rule{Name: "deny-danger", Expression: `tool.name == "danger" && action.name == "call"`, Reason: "Dangerous calls are denied"})
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rpc struct {
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
			t.Error(err)
			return
		}
		if rpc.Method != "tools/list" {
			t.Errorf("inspection forwarded a real call: %s", rpc.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"weather","description":"Current weather"},{"name":"danger","description":"Dangerous tool"}]}}`))
	}))
	t.Cleanup(upstream.Close)
	proxy, err := mcpproxy.NewMultiHandler(engine, audit.NewStdoutLogger(&fixture.audit), []mcpproxy.TargetConfig{{Name: "ops", Upstream: upstream.URL}})
	if err != nil {
		t.Fatal(err)
	}
	fixture.settings.mcpTargets = []gatewayconfig.MCPTarget{{Name: "ops", Upstream: upstream.URL, Policy: "/private/ops-policy.yaml"}, {Name: "disabled", Upstream: upstream.URL, Enabled: consolePtr(false), Policy: "/private/disabled-policy.yaml"}}
	registry := provider.NewRegistry()
	registry.Register(&fakeHealthProvider{name: "fake"})
	fixture.router = router.New(registry)
	fixture.router.SetCircuitBreaker(router.NewCircuitBreaker(2, time.Minute))
	fixture.dash = dashboard.New(st)
	fixture.dash.SetConfigPath(configPath)
	handlers := routeHandlers{settings: fixture.settings, registry: registry, router: fixture.router, store: st, gateway: gateway.New(registry, fixture.router, st, guardrail.NewEngine()), admin: admin.New(st), feedback: feedback.New(st), prompt: prompt.New(st), org: org.New(st), eval: eval.NewHandler(t.TempDir()), vault: vault.NewHandler(vault.New(st, "")), dash: fixture.dash, mcp: proxy}
	var cleanup func()
	fixture.handler, cleanup, err = wrapGatewayMiddleware(fixture.settings, st, newGatewayMux(handlers))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	return fixture
}

func consolePtr[T any](v T) *T { return &v }

func (f *consoleFixture) request(method, path, role, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if role != "" {
		r.Header.Set("Authorization", "Bearer "+f.secrets[role])
	}
	// Authorization and scoping must ignore these caller-controlled values.
	r.Header.Set("X-Omniswitch-Role", "owner")
	r.Header.Set("X-Omniswitch-Key-Id", "admin")
	r.Header.Set("X-Omniswitch-Workspace-Id", "workspace-admin")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func consoleJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON: %s: %v", w.Body.String(), err)
	}
	return result
}

func TestConsoleRoleMatrix(t *testing.T) {
	f := newConsoleFixture(t, true)
	validateBody, _ := json.Marshal(map[string]string{"format": "yaml", "body": validConsoleConfig})
	cases := []struct {
		method, path, body, minimum string
		status                      int
	}{
		{http.MethodGet, "/api/session", "", "viewer", 200},
		{http.MethodGet, "/api/logs", "", "viewer", 200},
		{http.MethodGet, "/api/logs/facets", "", "viewer", 200},
		{http.MethodGet, "/api/metrics?window=24h", "", "viewer", 200},
		{http.MethodGet, "/api/metrics/providers?window=24h", "", "viewer", 200},
		{http.MethodGet, "/api/metrics/timeseries?window=24h", "", "viewer", 200},
		{http.MethodGet, "/api/metrics/latency?window=24h", "", "viewer", 200},
		{http.MethodGet, "/api/guardrails/events", "", "viewer", 200},
		{http.MethodGet, "/api/guardrails/summary", "", "viewer", 200},
		{http.MethodGet, "/api/traces/shared-trace", "", "viewer", 200},
		{http.MethodGet, "/api/mcp/targets", "", "viewer", 200},
		{http.MethodGet, "/api/mcp/tools", "", "viewer", 200},
		{http.MethodGet, "/api/mcp/decisions", "", "viewer", 200},
		{http.MethodGet, "/api/providers/health", "", "viewer", 200},
		{http.MethodGet, "/api/keys", "", "viewer", 200},
		{http.MethodGet, "/api/keys/usage?window=month", "", "viewer", 200},
		{http.MethodGet, "/api/config", "", "viewer", 200},
		{http.MethodGet, "/api/config/raw", "", "viewer", 200},
		{http.MethodGet, "/api/logs/export?format=csv", "", "admin", 200},
		{http.MethodGet, "/api/mcp/tools?as_key=viewer", "", "admin", 200},
		{http.MethodPost, "/api/config/validate", string(validateBody), "admin", 200},
		{http.MethodPost, "/api/config/raw", string(validateBody), "admin", 200},
		{http.MethodPost, "/api/providers/fake/circuit/reset", "", "admin", 200},
		{http.MethodPatch, "/api/keys?id=member", `{"requests_per_minute":0,"tokens_per_minute":0,"usd_per_day":0}`, "admin", 200},
		{http.MethodPost, "/api/keys", `{"name":"created-by-matrix","role":"member"}`, "admin", 201},
		{http.MethodDelete, "/api/keys?id=disposable", "", "admin", 200},
		{http.MethodPost, "/api/evals/policy", `{}`, "member", 400},
	}
	ranks := map[string]int{"viewer": 1, "member": 2, "admin": 3, "owner": 4}
	for _, test := range cases {
		for _, role := range []string{"", "viewer", "member", "admin", "owner"} {
			w := f.request(test.method, test.path, role, test.body)
			want := test.status
			if role == "" {
				want = 401
			} else if ranks[role] < ranks[test.minimum] {
				want = 403
			}
			if w.Code != want {
				t.Fatalf("%s %s role=%q: status %d want %d body=%s", test.method, test.path, role, w.Code, want, w.Body.String())
			}
			if want == 401 || want == 403 {
				err, ok := consoleJSON(t, w)["error"].(map[string]any)
				if !ok || err["message"] == nil || err["type"] == nil || err["code"] == nil || err["message"] == "" || err["type"] == "" || err["code"] == "" {
					t.Fatalf("non-contract error: %s", w.Body.String())
				}
			}
		}
	}
	if f.audit.Len() != 0 {
		t.Fatalf("inspection wrote audit events: %s", f.audit.String())
	}
}

func TestConsoleTrustedScoping(t *testing.T) {
	f := newConsoleFixture(t, true)
	for _, role := range []string{"viewer", "member"} {
		for _, path := range []string{"/api/logs", "/api/logs/facets", "/api/guardrails/events", "/api/traces/shared-trace", "/api/mcp/decisions", "/api/keys", "/api/keys/usage?window=month"} {
			w := f.request(http.MethodGet, path, role, "")
			if w.Code != 200 {
				t.Fatalf("%s %s: %d/%s", role, path, w.Code, w.Body.String())
			}
			for _, other := range []string{"admin", "owner"} {
				for _, marker := range []string{"private-prompt-" + other, "private-response-" + other, "comment-" + other, "personal-" + other, other + "-request", other + "-guardrail", other + "-mcp"} {
					if strings.Contains(w.Body.String(), marker) {
						t.Fatalf("%s %s leaked %q: %s", role, path, marker, w.Body.String())
					}
				}
			}
			if strings.Contains(w.Body.String(), "ambiguous legacy feedback") {
				t.Fatalf("non-admin saw ambiguous trace-only feedback: %s", w.Body.String())
			}
		}
		w := f.request(http.MethodGet, "/api/logs?api_key_id=admin", role, "")
		if w.Code != http.StatusForbidden {
			t.Fatalf("foreign key filter: %d/%s", w.Code, w.Body.String())
		}
		w = f.request(http.MethodGet, "/api/mcp/tools?as_key=", role, "")
		if w.Code != http.StatusForbidden {
			t.Fatalf("empty as_key bypass: %d/%s", w.Code, w.Body.String())
		}
		for _, path := range []string{"/api/config", "/api/mcp/targets"} {
			w := f.request(http.MethodGet, path, role, "")
			if strings.Contains(w.Body.String(), "/private/") || strings.Contains(w.Body.String(), "config_path") || strings.Contains(w.Body.String(), "policy_path") {
				t.Fatalf("non-admin server path leak: %s", w.Body.String())
			}
		}
	}
	w := f.request(http.MethodGet, "/api/metrics/timeseries?window=1h&bucket=5m", "viewer", "")
	var requests, tokens, costs float64
	for _, raw := range consoleJSON(t, w)["buckets"].([]any) {
		bucket := raw.(map[string]any)
		requests += bucket["requests"].(float64)
		tokens += bucket["tokens"].(float64)
		costs += bucket["cost"].(float64)
	}
	if requests != 2 || tokens != 10 || costs != 1 {
		t.Fatalf("scoped aggregate = %.0f/%.0f/%.2f", requests, tokens, costs)
	}
	w = f.request(http.MethodGet, "/api/providers/health", "viewer", "")
	if providers := consoleJSON(t, w)["providers"].([]any); len(providers) != 1 || providers[0].(map[string]any)["requests"] != float64(1) {
		t.Fatalf("scoped health: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "replicas") {
		t.Fatal("fabricated inference replicas without probe source")
	}
	w = f.request(http.MethodGet, "/api/guardrails/events", "viewer", "")
	for _, secret := range []string{"alice@example.com", "private-api-value", "sk-osw-private-value", "private-bearer-value"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("unredacted detail %q: %s", secret, w.Body.String())
		}
	}
	w = f.request(http.MethodGet, "/api/mcp/tools", "viewer", "")
	if tools := consoleJSON(t, w)["tools"].([]any); len(tools) != 1 || tools[0].(map[string]any)["name"] != "weather" {
		t.Fatalf("hidden tool visible: %s", w.Body.String())
	}
	w = f.request(http.MethodGet, "/api/mcp/tools?as_key=viewer", "admin", "")
	tools := consoleJSON(t, w)["tools"].([]any)
	var danger map[string]any
	for _, raw := range tools {
		tool := raw.(map[string]any)
		if tool["name"] == "danger" {
			danger = tool
		}
	}
	if len(tools) != 2 || consoleJSON(t, w)["principal"].(map[string]any)["key_id"] != "viewer" || danger == nil || danger["list"].(map[string]any)["allowed"] != false || danger["call"].(map[string]any)["allowed"] != false {
		t.Fatalf("as_key policy dry run: %s", w.Body.String())
	}
}

func TestConsoleAuthDisabledOwner(t *testing.T) {
	f := newConsoleFixture(t, false)
	w := f.request(http.MethodGet, "/api/session", "", "")
	result := consoleJSON(t, w)
	if w.Code != 200 || result["authenticated"] != true || result["auth_enabled"] != false || result["auth_method"] != "none" || result["role"] != "owner" || result["key_id"] != "" {
		t.Fatalf("auth-off session: %s", w.Body.String())
	}
	for _, request := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/logs/export?format=csv", ""}, {http.MethodGet, "/api/mcp/tools?as_key=viewer", ""},
		{http.MethodPost, "/api/providers/fake/circuit/reset", ""}, {http.MethodPatch, "/api/keys?id=viewer", `{"name":"changed"}`},
	} {
		if w := f.request(request.method, request.path, "", request.body); w.Code != 200 {
			t.Fatalf("auth-off %s: %d/%s", request.path, w.Code, w.Body.String())
		}
	}
}

// Contract samples define response keys and JSON types, not invented fixture
// values. Empty arrays and the documented optional legacy fields are allowed.
func assertConsoleFixture(t *testing.T, actual any, sample any, path string) {
	t.Helper()
	if sample == nil {
		return
	}
	switch want := sample.(type) {
	case map[string]any:
		got, ok := actual.(map[string]any)
		if !ok {
			t.Fatalf("%s: want object, got %T", path, actual)
		}
		for key, value := range want {
			field, exists := got[key]
			if !exists {
				if path == "$.logs[]" && (key == "decision_reason" || key == "session_id" || key == "trace_id") || path == "$.feedback[]" && (key == "rating" || key == "comment") {
					continue
				}
				t.Fatalf("%s missing fixture key %q", path, key)
			}
			if field == nil && (key == "last_used_at" || key == "expires_at" || key == "open_until" || key == "last_failure") {
				continue
			}
			assertConsoleFixture(t, field, value, path+"."+key)
		}
	case []any:
		got, ok := actual.([]any)
		if !ok {
			t.Fatalf("%s: want array, got %T", path, actual)
		}
		if len(want) > 0 {
			for _, row := range got {
				assertConsoleFixture(t, row, want[0], path+"[]")
			}
		}
	default:
		if reflect.TypeOf(actual) != reflect.TypeOf(sample) {
			t.Fatalf("%s: want JSON %T, got %T", path, sample, actual)
		}
	}
}

func TestConsoleResponseFixtures(t *testing.T) {
	f := newConsoleFixture(t, true)
	validateBody, _ := json.Marshal(map[string]string{"format": "yaml", "body": validConsoleConfig})
	for _, test := range []struct {
		method, path, body, file string
		status                   int
	}{
		{http.MethodGet, "/api/session", "", "session", 200},
		{http.MethodGet, "/api/logs", "", "logs", 200},
		{http.MethodGet, "/api/logs/facets", "", "logs_facets", 200},
		{http.MethodGet, "/api/metrics/timeseries?window=24h&bucket=1h", "", "metrics_timeseries", 200},
		{http.MethodGet, "/api/metrics/latency", "", "metrics_latency", 200},
		{http.MethodGet, "/api/guardrails/events", "", "guardrails_events", 200},
		{http.MethodGet, "/api/guardrails/summary", "", "guardrails_summary", 200},
		{http.MethodGet, "/api/traces/shared-trace", "", "traces", 200},
		{http.MethodGet, "/api/mcp/targets", "", "mcp_targets", 200},
		{http.MethodGet, "/api/mcp/tools", "", "mcp_tools", 200},
		{http.MethodGet, "/api/mcp/decisions", "", "mcp_decisions", 200},
		{http.MethodGet, "/api/providers/health", "", "providers_health", 200},
		{http.MethodPost, "/api/providers/fake/circuit/reset", "", "providers_circuit_reset", 200},
		{http.MethodGet, "/api/keys", "", "keys", 200},
		{http.MethodGet, "/api/keys/usage?window=month", "", "keys_usage", 200},
		{http.MethodPost, "/api/keys", `{"name":"contract","monthly_cost_budget":1,"requests_per_minute":20,"tokens_per_minute":200,"usd_per_day":1}`, "keys_create", 201},
		{http.MethodPatch, "/api/keys?id=viewer", `{"name":"contract-updated","requests_per_minute":5,"tokens_per_minute":10,"usd_per_day":0}`, "keys_update", 200},
		{http.MethodGet, "/api/config/raw", "", "config_raw", 200},
		{http.MethodPost, "/api/config/validate", string(validateBody), "config_validate", 200},
		{http.MethodPost, "/api/config/raw", string(validateBody), "config_raw_save", 200},
	} {
		t.Run(test.file, func(t *testing.T) {
			w := f.request(test.method, test.path, "admin", test.body)
			if w.Code != test.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			content, err := os.ReadFile(filepath.Join("..", "..", "internal", "dashboard", "testdata", "contract", test.file+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var sample any
			if err := json.Unmarshal(content, &sample); err != nil {
				t.Fatal(err)
			}
			assertConsoleFixture(t, consoleJSON(t, w), sample, "$")
		})
	}
	// Other streams own the additional config blocks. Assert P4's final
	// fixture keys without requiring their unmerged implementations here.
	w := f.request(http.MethodGet, "/api/config", "admin", "")
	config := consoleJSON(t, w)
	content, err := os.ReadFile(filepath.Join("..", "..", "internal", "dashboard", "testdata", "contract", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sample map[string]any
	if err := json.Unmarshal(content, &sample); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "commit", "config_path", "auth_enabled"} {
		if _, ok := config[key]; !ok {
			t.Fatalf("config missing %s", key)
		}
		assertConsoleFixture(t, config[key], sample[key], "$."+key)
	}
	if !strings.Contains(w.Body.String(), "/private/") {
		t.Fatal("admin config lost policy paths")
	}
}

func TestConsoleCSVContract(t *testing.T) {
	f := newConsoleFixture(t, true)
	stamp := time.Now().UTC().Add(-time.Second)
	if err := f.store.InsertLog(context.Background(), store.RequestLog{ID: "csv", Timestamp: stamp, Model: " \t=HYPERLINK(\"evil\")", Provider: "+evil", APIKeyID: "viewer", Status: "success", CostSource: "none", Decision: "ALLOW", RequestBody: "CSV_PRIVATE_PROMPT", ResponseBody: "CSV_PRIVATE_RESPONSE", ErrorMessage: "CSV_PRIVATE_ERROR", Metadata: `{"secret":"CSV_PRIVATE_METADATA"}`}); err != nil {
		t.Fatal(err)
	}
	w := f.request(http.MethodGet, "/api/logs/export?format=csv&model="+"%20%09%3DHYPERLINK%28%22evil%22%29", "admin", "")
	if w.Code != 200 {
		t.Fatalf("export: %d/%s", w.Code, w.Body.String())
	}
	var sidecar struct {
		ContentType    string   `json:"content_type"`
		FilenamePrefix string   `json:"filename_prefix"`
		MaxRows        int      `json:"max_rows"`
		Columns        []string `json:"columns"`
	}
	content, err := os.ReadFile(filepath.Join("..", "..", "internal", "dashboard", "testdata", "contract", "logs_export.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, &sidecar); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), sidecar.ContentType) || !strings.Contains(w.Header().Get("Content-Disposition"), sidecar.FilenamePrefix) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("CSV headers = %v", w.Header())
	}
	rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil || len(rows) != 2 || !reflect.DeepEqual(rows[0], sidecar.Columns) {
		t.Fatalf("CSV rows=%v err=%v", rows, err)
	}
	if len(rows)-1 > sidecar.MaxRows || sidecar.MaxRows != store.MaxExportRows {
		t.Fatal("CSV cap differs from contract")
	}
	for i, key := range rows[0] {
		if key == "model" && !strings.HasPrefix(rows[1][i], "'") || key == "provider" && rows[1][i] != "'+evil" {
			t.Fatalf("unsafe CSV %s=%q", key, rows[1][i])
		}
	}
	if strings.Contains(w.Body.String(), "CSV_PRIVATE") {
		t.Fatal("CSV exported payload/error/metadata")
	}
	if f.request(http.MethodGet, "/api/logs/export?format=json", "admin", "").Code != http.StatusBadRequest {
		t.Fatal("non-CSV export accepted")
	}
}

func TestConsoleRejectsInvalidFilters(t *testing.T) {
	f := newConsoleFixture(t, true)
	for _, path := range []string{
		"/api/logs?window=2h", "/api/logs?limit=0", "/api/logs?limit=-1", "/api/logs?offset=-1",
		"/api/logs?cached=maybe", "/api/logs?status=alien", "/api/logs?decision=PASS", "/api/logs?sort=cost_desc",
		"/api/logs?since=invalid", "/api/logs?since=2026-01-02T00:00:00Z&until=2026-01-01T00:00:00Z",
		"/api/logs?since=0000-01-01T00:00:00Z&until=0000-01-02T00:00:00Z", "/api/logs?q=%00", "/api/logs?provider=%FF", "/api/logs?model=%ZZ",
		"/api/logs/facets?window=2h", "/api/metrics?window=bad", "/api/metrics/providers?window=bad",
		"/api/metrics/timeseries?window=1h&bucket=1s", "/api/metrics/timeseries?bucket=0s", "/api/metrics/timeseries?bucket=100ms", "/api/metrics/latency?window=bad",
		"/api/guardrails/events?limit=0", "/api/guardrails/events?trace_id=%00", "/api/guardrails/summary?window=bad",
		"/api/mcp/decisions?offset=-1", "/api/mcp/tools?as_key=", "/api/mcp/tools?as_key=%00", "/api/mcp/tools?target=%ZZ", "/api/traces/shared-trace?window=%ZZ", "/api/keys/usage?window=day",
	} {
		w := f.request(http.MethodGet, path, "admin", "")
		if w.Code != 400 {
			t.Fatalf("%s: %d/%s", path, w.Code, w.Body.String())
		}
		err, ok := consoleJSON(t, w)["error"].(map[string]any)
		if !ok || err["code"] == nil || err["type"] == nil || err["message"] == nil {
			t.Fatalf("bad error envelope: %s", w.Body.String())
		}
	}
	w := f.request(http.MethodGet, "/api/logs?limit=500&status=success,error&status=success", "admin", "")
	if w.Code != 200 || consoleJSON(t, w)["limit"] != float64(100) {
		t.Fatalf("validated pagination/statuses: %d/%s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/api/mcp/tools?as_key=missing", "/api/mcp/tools?target=missing", "/api/traces/missing"} {
		if w := f.request(http.MethodGet, path, "admin", ""); w.Code != 404 {
			t.Fatalf("%s: %d/%s", path, w.Code, w.Body.String())
		}
	}
	if w := f.request(http.MethodPost, "/api/providers/missing/circuit/reset", "admin", ""); w.Code != 404 {
		t.Fatalf("unknown reset: %d/%s", w.Code, w.Body.String())
	}
}

func TestConsoleConfigValidationAndAtomicSave(t *testing.T) {
	f := newConsoleFixture(t, true)
	original, err := os.ReadFile(f.settings.configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		format, body string
		syntax       bool
	}{
		{"yaml", "apiVersion: [\n", true}, {"yaml", "apiVersion: omniswitch.dev/v1\nkind: WrongKind\n", false},
		{"yaml", validConsoleConfig + "---\n" + validConsoleConfig, false}, {"json", "{\n\"kind\":\n}", true},
		{"json", `{"apiVersion":"omniswitch.dev/v1","kind":"GatewayConfig"} {}`, false}, {"toml", "x=1", false},
	} {
		body, _ := json.Marshal(map[string]string{"format": test.format, "body": test.body})
		w := f.request(http.MethodPost, "/api/config/validate", "admin", string(body))
		result := consoleJSON(t, w)
		if w.Code != 200 || result["valid"] != false {
			t.Fatalf("validation: %d/%s", w.Code, w.Body.String())
		}
		errors := result["errors"].([]any)
		if len(errors) == 0 {
			t.Fatal("no validation explanation")
		}
		line := errors[0].(map[string]any)["line"].(float64)
		if test.syntax && line < 1 || !test.syntax && line != 0 {
			t.Fatalf("invented/missing source line: %s", w.Body.String())
		}
		w = f.request(http.MethodPost, "/api/config/raw", "admin", string(body))
		if w.Code != 400 {
			t.Fatalf("invalid save: %d/%s", w.Code, w.Body.String())
		}
		result = consoleJSON(t, w)
		if result["valid"] != false || !reflect.DeepEqual(result["errors"], errors) {
			t.Fatalf("validate/save disagreement: %s", w.Body.String())
		}
		actual, err := os.ReadFile(f.settings.configPath)
		if err != nil || !bytes.Equal(actual, original) {
			t.Fatal("invalid config modified file")
		}
	}
	jsonBody, _ := json.Marshal(map[string]string{"format": "json", "body": `{"apiVersion":"omniswitch.dev/v1","kind":"GatewayConfig"}`})
	w := f.request(http.MethodPost, "/api/config/validate", "admin", string(jsonBody))
	if w.Code != 200 || consoleJSON(t, w)["valid"] != true {
		t.Fatalf("valid JSON: %d/%s", w.Code, w.Body.String())
	}
	w = f.request(http.MethodPost, "/api/config/raw", "admin", string(jsonBody))
	if w.Code != 400 {
		t.Fatal("save changed configured file format")
	}
	for _, body := range []string{`{"format":"yaml","body":"x","path":"/etc/passwd"}`, `{}`, `{} {}`, `null`} {
		w := f.request(http.MethodPost, "/api/config/validate", "admin", body)
		if w.Code == 200 && consoleJSON(t, w)["valid"] != false {
			t.Fatalf("bad validation envelope accepted: %s", body)
		}
		if f.request(http.MethodPost, "/api/config/raw", "admin", body).Code != 400 {
			t.Fatalf("bad save envelope accepted: %s", body)
		}
	}
	if err := os.Chmod(f.settings.configPath, 0640); err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(validConsoleConfig, "fake-model", "updated-model", 1)
	body, _ := json.Marshal(map[string]string{"format": "yaml", "body": updated})
	w = f.request(http.MethodPost, "/api/config/validate", "admin", string(body))
	if w.Code != 200 || consoleJSON(t, w)["valid"] != true {
		t.Fatalf("valid YAML: %s", w.Body.String())
	}
	actual, _ := os.ReadFile(f.settings.configPath)
	if !bytes.Equal(actual, original) {
		t.Fatal("validate wrote to disk")
	}
	w = f.request(http.MethodPost, "/api/config/raw", "admin", string(body))
	if w.Code != 200 {
		t.Fatalf("save: %d/%s", w.Code, w.Body.String())
	}
	actual, _ = os.ReadFile(f.settings.configPath)
	info, err := os.Stat(f.settings.configPath)
	if err != nil || string(actual) != updated || info.Mode().Perm() != 0640 {
		t.Fatalf("atomic save changed permissions/body: %v", err)
	}
	files, err := os.ReadDir(filepath.Dir(f.settings.configPath))
	if err != nil || len(files) != 1 {
		t.Fatalf("temporary save files left behind: %v/%v", files, err)
	}
}
