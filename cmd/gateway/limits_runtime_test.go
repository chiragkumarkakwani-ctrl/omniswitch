package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/gateway"
	"github.com/omniswitch-dev/omniswitch/internal/gatewayconfig"
	"github.com/omniswitch-dev/omniswitch/internal/guardrail"
	"github.com/omniswitch-dev/omniswitch/internal/limits"
	"github.com/omniswitch-dev/omniswitch/internal/provider"
	"github.com/omniswitch-dev/omniswitch/internal/provider/catalog"
	"github.com/omniswitch-dev/omniswitch/internal/router"
)

func TestRuntimeLoadsRelativePricingFileAndPolicyFields(t *testing.T) {
	clearGatewayEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(filepath.Join(dir, "prices.json"), []byte(`{"models":[{"provider":"openai","id":"gpt-4o-mini","input_per_million":2}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`rate_limit:
  policies:
    - name: tokens
      scope: key
      unit: total_tokens
      limit: 1000
      window: 1m
pricing:
  catalog_file: prices.json
  overrides:
    - provider: openai
      model: gpt-4o-mini
      input_per_million: 3
      output_per_million: 4
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMNISWITCH_CONFIG", path)
	settings, err := loadRuntimeSettings()
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.rateLimitPolicies) != 1 || settings.pricingCatalogFile != "prices.json" || settings.pricingCatalog.File() != filepath.Join(dir, "prices.json") || settings.pricingCatalog.OverrideCount() != 1 {
		t.Fatalf("runtime policy/pricing fields missing: %+v", settings)
	}
	model, _ := settings.pricingCatalog.Lookup("openai", "gpt-4o-mini")
	if model.InputPerMillion != 3 || model.OutputPerMillion != 4 || model.Source != "override" {
		t.Fatalf("override order = %+v", model)
	}
}

func TestHotReloadPoliciesAndPricingPreservesUsageAndRejectsInvalidConfig(t *testing.T) {
	clearGatewayEnv(t)
	st := newTestStore(t)
	registry := provider.NewRegistry()
	rtr := router.New(registry)
	gw := gateway.New(registry, rtr, st, guardrail.NewEngine())
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	policy := limits.Policy{Name: "tokens", Scope: limits.ScopeKey, Unit: limits.TotalTokens, Limit: 1000, Window: "1m"}
	enforcer, err := limits.NewEnforcer(limits.NewMemoryBackendWithClock(clock), limits.Options{Requests: 120, Window: time.Minute, Policies: []limits.Policy{policy}, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	gw.SetLimits(enforcer)
	identity := limits.Identity{KeyID: "key"}
	reservation, _, err := enforcer.Reserve(context.Background(), identity, "gpt-4o-mini", limits.Usage{InputTokens: 200})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reservation.Commit(context.Background(), limits.Usage{InputTokens: 250}); err != nil {
		t.Fatal(err)
	}
	base := runtimeSettings{rateLimitRequests: 120, rateLimitWindow: time.Minute, rateLimitRedisURL: "redis://user:private-password@localhost:6379/0"}
	cfg := gatewayconfig.Config{Routes: map[string]router.Route{"production": {Provider: "openai"}}}
	policy.Limit = 300
	cfg.RateLimit.Policies = []limits.Policy{policy}
	cfg.Pricing.Overrides = []catalog.Override{{Provider: "openai", Model: "gpt-4o-mini", InputPerMillion: 2, OutputPerMillion: 3}}
	previous := catalog.Active()
	t.Cleanup(func() { catalog.Activate(previous) })
	if _, _, err := applyHotConfigChecked(cfg, rtr, gw, base, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := enforcer.Reserve(context.Background(), identity, "gpt-4o-mini", limits.Usage{InputTokens: 100}); err == nil {
		t.Fatal("reload reset existing token usage")
	}
	live := catalog.Active()
	invalid := cfg
	invalid.RateLimit.Policies = []limits.Policy{{Name: "bad", Scope: limits.ScopeKey, Unit: limits.TotalTokens, Limit: 0, Window: "1m"}}
	invalid.Routes = map[string]router.Route{"invalid": {Provider: "openai"}}
	if _, _, err := applyHotConfigChecked(invalid, rtr, gw, base, nil); err == nil || catalog.Active() != live || enforcer.Options().Policies[0].Limit != 300 {
		t.Fatal("invalid reload changed live state")
	}
	if _, ok := rtr.RouteFor("invalid"); ok {
		t.Fatal("invalid routes applied")
	}
	invalid = cfg
	invalid.Pricing.CatalogFile = filepath.Join(t.TempDir(), "missing")
	if _, _, err := applyHotConfigChecked(invalid, rtr, gw, base, nil); err == nil || catalog.Active() != live || enforcer.Options().Policies[0].Limit != 300 {
		t.Fatal("invalid catalog changed live limits")
	}

	mux := http.NewServeMux()
	registerInfoRoutes(mux, base, registry, st, gw)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if response.Code != 200 || strings.Contains(response.Body.String(), "private-password") || strings.Contains(response.Body.String(), "redis://") {
		t.Fatalf("summary = %d/%s", response.Code, response.Body.String())
	}
	var summary struct {
		RateLimit struct {
			Policies []limits.Policy `json:"policies"`
		} `json:"rate_limit"`
		Pricing struct {
			Date      string `json:"catalog_verified_on"`
			File      string `json:"catalog_file"`
			Overrides int    `json:"overrides"`
		} `json:"pricing"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &summary); err != nil || len(summary.RateLimit.Policies) != 1 || summary.RateLimit.Policies[0].Limit != 300 || summary.RateLimit.Policies[0].Action != limits.Reject || summary.Pricing.Overrides != 1 || summary.Pricing.Date == "" {
		t.Fatalf("live summary = %s/%v", response.Body.String(), err)
	}
	var raw map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	match := raw["rate_limit"].(map[string]any)["policies"].([]any)[0].(map[string]any)["match"]
	if _, ok := match.(map[string]any); !ok {
		t.Fatal("policy match must be an object, per the approved policy schema")
	}
	if _, _, err := applyHotConfigChecked(gatewayconfig.Config{}, rtr, gw, base, nil); err != nil || len(enforcer.Options().Policies) != 0 || catalog.Active().File() != "" || catalog.Active().OverrideCount() != 0 {
		t.Fatal("removing policy/pricing arrays did not reset overlays")
	}
}

func TestHotReloadKeepsExplicitEnvironmentQuotaOverrides(t *testing.T) {
	clearGatewayEnv(t)
	t.Setenv("OMNISWITCH_RATE_LIMIT_REQUESTS", "7")
	t.Setenv("OMNISWITCH_RATE_LIMIT_WINDOW", "2m")
	t.Setenv("OMNISWITCH_RATE_LIMIT_FAIL_OPEN", "true")
	registry := provider.NewRegistry()
	rtr := router.New(registry)
	gw := gateway.New(registry, rtr, newTestStore(t), guardrail.NewEngine())
	enforcer, err := limits.NewEnforcer(nil, limits.Options{})
	if err != nil {
		t.Fatal(err)
	}
	gw.SetLimits(enforcer)
	previous := catalog.Active()
	t.Cleanup(func() { catalog.Activate(previous) })
	cfg := gatewayconfig.Config{}
	cfg.RateLimit.Requests = 99
	cfg.RateLimit.Window.Duration = time.Second
	if _, _, err := applyHotConfigChecked(cfg, rtr, gw, runtimeSettings{}, nil); err != nil {
		t.Fatal(err)
	}
	options := enforcer.Options()
	if options.Requests != 7 || options.Window != 2*time.Minute || !options.FailOpen {
		t.Fatalf("env overrides = %+v", options)
	}
}
