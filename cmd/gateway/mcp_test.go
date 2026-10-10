package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/omniswitch-dev/omniswitch/internal/gatewayconfig"
	"github.com/omniswitch-dev/omniswitch/internal/policy"
)

func TestBuildMCPTargetsMapsTokenExchange(t *testing.T) {
	t.Setenv("MCP_CLIENT_SECRET", "test-client-credential")
	t.Setenv("MCP_ACTOR_TOKEN", "test-actor-credential")
	settings := runtimeSettings{mcpTargets: []gatewayconfig.MCPTarget{{
		Name: "ops", Upstream: "https://mcp.example.com/mcp",
		TokenExchange: &gatewayconfig.MCPTokenExchange{
			TokenURL: "https://auth.example.com/token", ClientID: "gateway-client",
			ClientSecretEnv: "MCP_CLIENT_SECRET", SubjectTokenType: "subject-type", RequestedTokenType: "requested-type",
			Scope: "tools.read tools.invoke", Audience: "ops", Resource: "https://mcp.example.com",
			ActorTokenEnv: "MCP_ACTOR_TOKEN", ActorTokenType: "actor-type",
		},
	}}}
	engine, _ := policy.NewEngine()
	targets, err := buildMCPTargets(engine, settings)
	if err != nil || len(targets) != 1 {
		t.Fatalf("buildMCPTargets() count/error = %d/%v", len(targets), err)
	}
	exchange := targets[0].TokenExchange
	if exchange == nil || exchange.TokenURL != "https://auth.example.com/token" || exchange.ClientID != "gateway-client" ||
		exchange.ClientSecret != "test-client-credential" || exchange.SubjectTokenType != "subject-type" ||
		exchange.RequestedTokenType != "requested-type" || exchange.Scope != "tools.read tools.invoke" ||
		exchange.Audience != "ops" || exchange.Resource != "https://mcp.example.com" ||
		exchange.ActorToken != "test-actor-credential" || exchange.ActorTokenType != "actor-type" {
		t.Fatal("token_exchange fields were not mapped to the proxy config")
	}
	// Even accidental marshaling of the internal proxy config must not expose
	// resolved secret values.
	payload, err := json.Marshal(exchange)
	if err != nil || strings.Contains(string(payload), "credential") {
		t.Fatalf("proxy exchange config exposed credentials: error = %v", err)
	}
	summary, err := json.Marshal(runtimeConfigSummary(settings, nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, sensitive := range []string{"test-client-credential", "test-actor-credential", "MCP_CLIENT_SECRET", "MCP_ACTOR_TOKEN", "/token", "gateway-client"} {
		if strings.Contains(string(summary), sensitive) {
			t.Fatalf("summary exposed sensitive configuration %q", sensitive)
		}
	}
	item := mcpTargetSummaries(settings)[0]["token_exchange"].(map[string]any)
	if item["configured"] != true || item["token_url_host"] != "auth.example.com" || item["audience"] != "ops" {
		t.Fatalf("token_exchange summary = %v", item)
	}
	if item := mcpTokenExchangeSummary(nil); item["configured"] != false || item["token_url_host"] != "" || item["audience"] != "" {
		t.Fatalf("unconfigured summary = %v", item)
	}
}

func TestBuildMCPTokenExchangeRequiresReferencedCredentials(t *testing.T) {
	for _, field := range []string{"client", "actor"} {
		t.Run(field, func(t *testing.T) {
			t.Setenv("MCP_MISSING_CREDENTIAL", "")
			cfg := &gatewayconfig.MCPTokenExchange{TokenURL: "https://auth.example.com/token", ClientID: "client"}
			if field == "client" {
				cfg.ClientSecretEnv = "MCP_MISSING_CREDENTIAL"
			} else {
				cfg.ActorTokenEnv = "MCP_MISSING_CREDENTIAL"
			}
			if _, err := buildMCPTokenExchange(cfg); err == nil || !strings.Contains(err.Error(), "MCP_MISSING_CREDENTIAL") {
				t.Fatalf("buildMCPTokenExchange() = %v, want missing env reference", err)
			}
		})
	}
	if config, err := buildMCPTokenExchange(nil); err != nil || config != nil {
		t.Fatalf("absent exchange = %v/%v", config, err)
	}
	config, err := buildMCPTokenExchange(&gatewayconfig.MCPTokenExchange{TokenURL: "https://auth.example.com/token", ClientID: "public-client"})
	if err != nil || config.ClientSecret != "" || config.ActorToken != "" {
		t.Fatalf("public client exchange failed: %v", err)
	}
}
