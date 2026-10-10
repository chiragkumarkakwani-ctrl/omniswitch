package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/omniswitch-dev/omniswitch/internal/gateway"
	"github.com/omniswitch-dev/omniswitch/internal/store"
)

type consoleAuthenticator struct{ identity gateway.Identity }

func (a consoleAuthenticator) Authenticate(_ context.Context, token string) (gateway.Identity, error) {
	if token != "oidc-test-token" {
		return gateway.Identity{}, fmt.Errorf("invalid test token")
	}
	return a.identity, nil
}

func TestConsoleSessionPrivacyAndCredentialExpiry(t *testing.T) {
	f := newConsoleFixture(t, true)
	ctx := context.Background()
	secret := "bootstrap-session-test-secret"
	f.secrets["bootstrap-admin"] = secret
	expires := time.Now().Add(time.Hour).Truncate(time.Second).In(time.FixedZone("test-offset", 2*60*60))
	key := store.APIKey{ID: "bootstrap-admin", Name: "Bootstrap administrator", KeyHash: sha256Hex(secret), KeyPrefix: "stored-display-prefix", Role: "owner", WorkspaceID: "workspace-owner", CreatedAt: time.Now().UTC(), Enabled: true, ExpiresAt: &expires, Metadata: map[string]string{"secret": "PRIVATE_KEY_METADATA"}}
	if err := f.store.InsertAPIKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	w := f.request(http.MethodGet, "/api/session", "bootstrap-admin", "")
	result := consoleJSON(t, w)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || result["auth_method"] != "api_key" || result["is_bootstrap"] != true || result["key_prefix"] != key.KeyPrefix || result["name"] != key.Name || result["expires_at"] != expires.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("bootstrap session: %d/%s", w.Code, w.Body.String())
	}
	for _, private := range []string{secret, key.KeyHash, "PRIVATE_KEY_METADATA", "key_hash", "metadata"} {
		if strings.Contains(w.Body.String(), private) {
			t.Fatalf("session exposed %s", private)
		}
	}
	payloadLogging := false
	f.dash.SetLogPayloadsSource(func() bool { return payloadLogging })
	for _, enabled := range []bool{false, true, false} {
		payloadLogging = enabled
		w = f.request(http.MethodGet, "/api/session", "viewer", "")
		if consoleJSON(t, w)["gateway"].(map[string]any)["log_payloads"] != enabled {
			t.Fatal("session payload posture did not reflect live source")
		}
	}
	for _, disabled := range []bool{false, true} {
		key.Enabled = !disabled
		past := time.Now().Add(-time.Hour)
		key.ExpiresAt = &past
		if disabled {
			key.ExpiresAt = nil
		}
		if err := f.store.InsertAPIKey(ctx, key); err != nil {
			t.Fatal(err)
		}
		w = f.request(http.MethodGet, "/api/session", "bootstrap-admin", "")
		errorBody := consoleJSON(t, w)["error"].(map[string]any)
		if w.Code != 401 || errorBody["type"] != "authentication_error" || errorBody["code"] != "unauthorized" || strings.Contains(w.Body.String(), secret) {
			t.Fatalf("invalid credential: %d/%s", w.Code, w.Body.String())
		}
	}
}

func TestConsoleOIDCSessionOmitsClaimsAndBootstrapFlag(t *testing.T) {
	f := newConsoleFixture(t, true)
	mux := http.NewServeMux()
	f.dash.RegisterRoutes(mux)
	auth := gateway.NewAuthMiddleware(f.store, true)
	auth.SetTokenAuthenticator(consoleAuthenticator{identity: gateway.Identity{Subject: "bootstrap-admin", AuthMethod: "oidc", Role: "editor", WorkspaceID: "verified-workspace", OrganizationID: "verified-org", Claims: map[string]any{"secret": "PRIVATE_OIDC_CLAIM"}}})
	handler := auth.Wrap(gateway.NewAuthorizationMiddleware(true).Wrap(mux))
	r := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	r.Header.Set("Authorization", "Bearer oidc-test-token")
	r.Header.Set("X-Omniswitch-Role", "owner")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	result := consoleJSON(t, w)
	if w.Code != 200 || result["auth_method"] != "oidc" || result["role"] != "member" || result["name"] != "bootstrap-admin" || result["is_bootstrap"] != false || result["key_prefix"] != "" || result["expires_at"] != nil || result["workspace_id"] != "verified-workspace" || result["organization_id"] != "verified-org" {
		t.Fatalf("OIDC session: %d/%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "PRIVATE_OIDC_CLAIM") || strings.Contains(w.Body.String(), "oidc-test-token") || strings.Contains(w.Body.String(), "claims") {
		t.Fatal("OIDC session exposed claims or bearer token")
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/session", nil))
	if w.Code != 401 {
		t.Fatal("auth-enabled session accepted a missing trusted identity")
	}
}

func TestConsoleGuardrailDetailsUnicodeAndBounds(t *testing.T) {
	f := newConsoleFixture(t, true)
	details := strings.Repeat("界", 300)
	if err := f.store.InsertGuardrailEvent(context.Background(), store.GuardrailEvent{ID: "unicode-event", RequestID: "viewer-request", Timestamp: time.Now().UTC().Add(-time.Millisecond), Type: "unicode", Action: "warn", Matched: true, Details: details}); err != nil {
		t.Fatal(err)
	}
	w := f.request(http.MethodGet, "/api/guardrails/events?type=unicode&trace_id=shared-trace", "viewer", "")
	if w.Code != 200 {
		t.Fatalf("unicode events: %d/%s", w.Code, w.Body.String())
	}
	events := consoleJSON(t, w)["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("trace/type filters: %s", w.Body.String())
	}
	redacted := events[0].(map[string]any)["details"].(string)
	if !utf8.ValidString(redacted) || utf8.RuneCountInString(redacted) != 256 || redacted != strings.Repeat("界", 256) {
		t.Fatalf("details split unicode or exceeded bound: %q", redacted)
	}
	w = f.request(http.MethodGet, "/api/traces/shared-trace", "viewer", "")
	for _, entry := range consoleJSON(t, w)["guardrail_events"].([]any) {
		if text := entry.(map[string]any)["details"].(string); !utf8.ValidString(text) || utf8.RuneCountInString(text) > 256 {
			t.Fatal("trace details exceeded bound")
		}
	}
}

func TestConsoleKeyCreateAndPartialUpdateValidation(t *testing.T) {
	f := newConsoleFixture(t, true)
	invalidCreates := []string{
		`{}`, `null`, `{"name":"   "}`, `{"name":"bad\u0000name"}`, `{"name":"bad","workspace_id":"bad\u0000id"}`,
		`{"name":"bad","role":"editor"}`, `{"name":"bad","rate_limit":-1}`, `{"name":"bad","monthly_token_budget":-1}`,
		`{"name":"bad","requests_per_minute":2147483648}`, `{"name":"bad","tokens_per_minute":-1}`, `{"name":"bad","usd_per_day":-0.01}`,
		`{"name":"bad","budget_usd":-1}`, `{"name":"bad","expires_in":"0s"}`, `{"name":"bad","expires_in":"-1h"}`,
		`{"name":"bad","key_hash":"injected"}`, `{"name":"bad"} {}`, `{"name":"bad","rate_limit":1.2}`,
	}
	for _, body := range invalidCreates {
		if w := f.request(http.MethodPost, "/api/keys", "admin", body); w.Code != 400 {
			t.Fatalf("invalid create %s: %d/%s", body, w.Code, w.Body.String())
		}
	}
	before, err := f.store.GetAPIKeyByID(context.Background(), "disposable")
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{}`, `null`, `{"name":null}`, `{"name":""}`, `{"workspace_id":"bad\u0000id"}`,
		`{"role":"superadmin"}`, `{"requests_per_minute":-1}`, `{"monthly_cost_budget":-1}`, `{"enabled":"yes"}`,
		`{"name":"changed","key_prefix":"injected"}`, `{"spend_usd":0}`, `{"expires_at":null}`, `{"last_used_at":null}`, `{"enabled":false} {}`,
	} {
		w := f.request(http.MethodPatch, "/api/keys?id=disposable", "admin", body)
		if w.Code != 400 {
			t.Fatalf("invalid patch %s: %d/%s", body, w.Code, w.Body.String())
		}
		after, err := f.store.GetAPIKeyByID(context.Background(), "disposable")
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("invalid partial update changed the key")
		}
	}
	for _, path := range []string{"/api/keys", "/api/keys?id=", "/api/keys?id=%00"} {
		if w := f.request(http.MethodPatch, path, "admin", `{"enabled":false}`); w.Code != 400 {
			t.Fatalf("invalid id %s: %d/%s", path, w.Code, w.Body.String())
		}
	}
	if w := f.request(http.MethodPatch, "/api/keys?id=missing", "admin", `{"enabled":false}`); w.Code != 404 {
		t.Fatalf("missing key: %d/%s", w.Code, w.Body.String())
	}
	for _, rate := range []struct {
		field string
		want  float64
	}{{"", 60}, {`,"rate_limit":0`, 0}} {
		body := `{"name":"  created-key  ","role":"member","requests_per_minute":12,"tokens_per_minute":1200,"usd_per_day":2,"expires_in":"1h"` + rate.field + `}`
		w := f.request(http.MethodPost, "/api/keys", "admin", body)
		result := consoleJSON(t, w)
		key := result["api_key"].(map[string]any)
		if w.Code != 201 || key["name"] != "created-key" || key["rate_limit"] != rate.want || key["requests_per_minute"] != float64(12) || key["tokens_per_minute"] != float64(1200) || key["usd_per_day"] != float64(2) || key["last_used_at"] != nil {
			t.Fatalf("create limits: %d/%s", w.Code, w.Body.String())
		}
		secret := result["key"].(string)
		id := key["id"].(string)
		stored, err := f.store.GetAPIKeyByID(context.Background(), id)
		if err != nil || stored.KeyHash != sha256Hex(secret) || stored.ExpiresAt == nil || stored.KeyPrefix == "" {
			t.Fatal("key not stored as hash with expiry/display prefix")
		}
		if strings.Contains(w.Body.String(), "key_hash") || strings.Contains(w.Body.String(), stored.KeyHash) {
			t.Fatal("create exposed stored hash")
		}
		body = `{"name":"renamed","workspace_id":"","rate_limit":0,"monthly_cost_budget":0,"monthly_token_budget":0,"requests_per_minute":0,"tokens_per_minute":0,"usd_per_day":0}`
		w = f.request(http.MethodPatch, "/api/keys?id="+id, "admin", body)
		updated, err := f.store.GetAPIKeyByID(context.Background(), id)
		if w.Code != 200 || err != nil || updated.Name != "renamed" || updated.Role != "member" || updated.KeyHash != stored.KeyHash || updated.KeyPrefix != stored.KeyPrefix || !updated.ExpiresAt.Equal(*stored.ExpiresAt) || !updated.CreatedAt.Equal(stored.CreatedAt) {
			t.Fatalf("partial update/protected fields: %d/%s, %v", w.Code, w.Body.String(), err)
		}
		if strings.Contains(w.Body.String(), secret) || strings.Contains(w.Body.String(), stored.KeyHash) {
			t.Fatal("patch exposed secret or hash")
		}
		w = f.request(http.MethodGet, "/api/keys", "admin", "")
		if strings.Contains(w.Body.String(), secret) || strings.Contains(w.Body.String(), stored.KeyHash) {
			t.Fatal("list exposed secret or hash")
		}
		f.secrets["created"] = secret
		w = f.request(http.MethodGet, "/api/keys", "created", "")
		var own struct {
			Keys []store.APIKey `json:"keys"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &own); err != nil || len(own.Keys) != 1 || own.Keys[0].ID != id {
			t.Fatal("created member key list was not scoped")
		}
		if w := f.request(http.MethodDelete, "/api/keys?id="+id, "admin", ""); w.Code != 200 {
			t.Fatalf("disable: %d/%s", w.Code, w.Body.String())
		}
		if f.request(http.MethodGet, "/api/session", "created", "").Code != 401 {
			t.Fatal("disabled created key remained authenticated")
		}
	}
}
