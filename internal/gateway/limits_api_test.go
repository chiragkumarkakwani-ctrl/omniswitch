package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/limits"
	"github.com/omniswitch-dev/omniswitch/internal/provider"
	"github.com/omniswitch-dev/omniswitch/internal/store"
)

// Values in the console fixtures are examples; compare their required JSON
// keys/types without treating their sample names, dates or unit aliases as config.
func requireContractShape(t *testing.T, expected, actual any, path string) {
	t.Helper()
	if reflect.TypeOf(expected) != reflect.TypeOf(actual) {
		t.Fatalf("%s type = %T, want %T", path, actual, expected)
	}
	switch value := expected.(type) {
	case map[string]any:
		got := actual.(map[string]any)
		for key, want := range value {
			field, ok := got[key]
			if !ok {
				t.Fatalf("%s.%s missing", path, key)
			}
			requireContractShape(t, want, field, path+"."+key)
		}
	case []any:
		got := actual.([]any)
		if len(value) > 0 && len(got) == 0 {
			t.Fatalf("%s empty", path)
		}
		for index, item := range got {
			requireContractShape(t, value[0], item, fmt.Sprintf("%s[%d]", path, index))
		}
	}
}

func TestLimitsAndCatalogConsoleJSONContract(t *testing.T) {
	h := newLimitsHarness(t, store.APIKey{TokensPerMinute: 1000}, provider.Usage{})
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"catalog", h.gateway.Catalog}, {"limits_usage", h.gateway.LimitsUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture, err := os.ReadFile(filepath.Join("testdata", "contract", tc.name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "/api/"+tc.name, nil)
			request = request.WithContext(withIdentity(request.Context(), Identity{APIKeyID: h.key.ID, Role: "viewer", TokensPerMinute: 1000}))
			response := httptest.NewRecorder()
			tc.handler(response, request)
			if response.Code != 200 {
				t.Fatalf("contract = %d/%s", response.Code, response.Body.String())
			}
			var want, got any
			if err := json.Unmarshal(fixture, &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			requireContractShape(t, want, got, "$")
		})
	}
}

func TestLimitsUsageScopeIsOwnKeyUnlessAdmin(t *testing.T) {
	h := newLimitsHarness(t, store.APIKey{TokensPerMinute: 1000}, provider.Usage{})
	other := store.APIKey{ID: "other-key", Name: "other", KeyHash: "fixture-other-hash", Role: "viewer", Enabled: true, TokensPerMinute: 500, CreatedAt: time.Now()}
	if err := h.st.InsertAPIKey(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	reservation, _, err := h.enforcer.Reserve(context.Background(), limits.Identity{KeyID: other.ID, TokensPerMinute: 500}, "gpt-4o-mini", limits.Usage{InputTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reservation.Commit(context.Background(), limits.Usage{InputTokens: 80}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		role, key string
		status    int
		used      float64
	}{
		{"viewer", "", 200, 0}, {"member", h.key.ID, 200, 0}, {"viewer", other.ID, 403, 0},
		{"member", other.ID, 403, 0}, {"admin", other.ID, 200, 80}, {"owner", other.ID, 200, 80}, {"admin", "missing", 404, 0}, {"", "", 401, 0},
	} {
		t.Run(tc.role+"/"+tc.key, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/limits/usage?key_id="+tc.key, nil)
			if tc.role != "" {
				request = request.WithContext(withIdentity(request.Context(), Identity{APIKeyID: h.key.ID, Role: tc.role, TokensPerMinute: 1000}))
			}
			response := httptest.NewRecorder()
			h.gateway.LimitsUsage(response, request)
			if response.Code != tc.status {
				t.Fatalf("scope = %d/%s", response.Code, response.Body.String())
			}
			if response.Code != 200 {
				return
			}
			var body struct {
				Policies []limits.Snapshot `json:"policies"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			var found bool
			for _, policy := range body.Policies {
				if policy.Name == "key-tokens-per-minute" {
					found = true
					if policy.Used != tc.used {
						t.Fatalf("wrong key usage = %+v", policy)
					}
				}
			}
			if !found {
				t.Fatal("key token policy missing")
			}
		})
	}
}
