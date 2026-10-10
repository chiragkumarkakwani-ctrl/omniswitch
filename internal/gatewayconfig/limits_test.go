package gatewayconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omniswitch-dev/omniswitch/internal/limits"
)

func TestParseLimitsAndPricing(t *testing.T) {
	cfg, err := Parse([]byte(`rate_limit:
  policies:
    - name: workspace-spend
      scope: workspace
      unit: usd
      limit: 3.5
      window: 1d
      match:
        key_id: "worker-*"
        workspace_id: "team-*"
        model: "vendor/*"
      action: log
pricing:
  catalog_file: prices.json
  overrides:
    - provider: local
      model: small
      input_per_million: 2
      output_per_million: 3
      cached_input_per_million: 0
`), ".yaml")
	if err != nil {
		t.Fatal(err)
	}
	policy := cfg.RateLimit.Policies[0]
	if policy.Scope != limits.ScopeWorkspace || policy.Unit != limits.USD || policy.Action != limits.Log || policy.Match.KeyID != "worker-*" || policy.Match.WorkspaceID != "team-*" || policy.Match.Model != "vendor/*" {
		t.Fatalf("policy = %+v", policy)
	}
	if cfg.Pricing.CatalogFile != "prices.json" || cfg.Pricing.Overrides[0].CachedInputPerMillion == nil || *cfg.Pricing.Overrides[0].CachedInputPerMillion != 0 {
		t.Fatal("pricing fields did not parse")
	}
	for _, tc := range []struct{ fields, want string }{
		{"scope: planet", "scope"}, {"unit: bytes", "unit"}, {"window: tomorrow", "window"}, {"limit: 0", "limit"}, {"action: allow", "action"}, {"match: {model: '['}", "match.model"}, {"limit: .nan", "limit"},
	} {
		body := "rate_limit:\n  policies:\n    - name: quota\n      scope: key\n      unit: total_tokens\n      limit: 10\n      window: 1m\n      " + tc.fields + "\n"
		// Replace the matching default rather than introducing duplicate YAML keys.
		field := strings.SplitN(tc.fields, ":", 2)[0]
		for _, defaultField := range []string{"scope: key", "unit: total_tokens", "limit: 10", "window: 1m"} {
			if strings.HasPrefix(defaultField, field+":") {
				body = strings.Replace(body, "      "+defaultField+"\n", "", 1)
			}
		}
		if _, err := Parse([]byte(body), ".yaml"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s = %v", tc.fields, err)
		}
	}
	if _, err := Parse([]byte(`{"pricing":{"overrides":[{"provider":"p","model":"m","input_per_million":-1}]}}`), ".json"); err == nil || !strings.Contains(err.Error(), "pricing.overrides[0]") {
		t.Fatalf("negative price = %v", err)
	}
}

func TestLoadFileValidatesRelativePricingCatalog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(path, []byte("pricing:\n  catalog_file: prices.json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err == nil || !strings.Contains(err.Error(), "pricing.catalog_file") {
		t.Fatalf("missing catalog = %v", err)
	}
	priceFile := filepath.Join(dir, "prices.json")
	if err := os.WriteFile(priceFile, []byte(`{"models":[{"provider":"local","id":"m","input_per_million":-1}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err == nil || !strings.Contains(err.Error(), "pricing.catalog_file") {
		t.Fatalf("invalid catalog = %v", err)
	}
	if err := os.WriteFile(priceFile, []byte(`{"models":[{"provider":"local","id":"m","input_per_million":0,"output_per_million":0}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err != nil {
		t.Fatal(err)
	}
}
