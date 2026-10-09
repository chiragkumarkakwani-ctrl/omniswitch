package gatewayconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPTokenExchangeValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*MCPTarget)
		want   string
	}{
		{"missing URL", func(t *MCPTarget) { t.TokenExchange.TokenURL = "" }, "token_url"},
		{"invalid URL", func(t *MCPTarget) { t.TokenExchange.TokenURL = ":invalid" }, "token_url"},
		{"public HTTP", func(t *MCPTarget) { t.TokenExchange.TokenURL = "http://auth.example.com/token" }, "HTTPS"},
		{"relative URL", func(t *MCPTarget) { t.TokenExchange.TokenURL = "/token" }, "absolute"},
		{"URL credentials", func(t *MCPTarget) { t.TokenExchange.TokenURL = "https://user:secret@auth.example.com/token" }, "credentials"},
		{"URL fragment", func(t *MCPTarget) { t.TokenExchange.TokenURL += "#fragment" }, "fragments"},
		{"missing client", func(t *MCPTarget) { t.TokenExchange.ClientID = " " }, "client_id"},
		{"forward conflict", func(t *MCPTarget) { b := true; t.ForwardBearerToken = &b }, "cannot be combined"},
		{"literal secret", func(t *MCPTarget) { t.TokenExchange.ClientSecretEnv = "literal-secret-value" }, "environment variable"},
		{"literal actor", func(t *MCPTarget) { t.TokenExchange.ActorTokenEnv = "eyJ.secret.value" }, "environment variable"},
		{"actor type without actor", func(t *MCPTarget) { t.TokenExchange.ActorTokenType = "jwt" }, "actor_token_env"},
		{"stdio exchange", func(t *MCPTarget) { t.Transport = "stdio"; t.Command = "mcp-server" }, "HTTP transport"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{APIVersion: APIVersion, Kind: Kind, MCP: MCP{Targets: []MCPTarget{{
				Name: "ops", Upstream: "https://mcp.example.com/mcp",
				TokenExchange: &MCPTokenExchange{TokenURL: "https://auth.example.com/token", ClientID: "client"},
			}}}}
			tc.mutate(&cfg.MCP.Targets[0])
			if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want %q", err, tc.want)
			}
		})
	}
	for _, tokenURL := range []string{"https://auth.example.com/token", "http://localhost:8080/token", "http://127.0.0.1:8080/token", "http://[::1]:8080/token"} {
		cfg := Config{APIVersion: APIVersion, Kind: Kind, MCP: MCP{Targets: []MCPTarget{{Name: "ops", Upstream: "https://mcp.example.com/mcp", TokenExchange: &MCPTokenExchange{TokenURL: tokenURL, ClientID: "client", ClientSecretEnv: "MCP_CLIENT_SECRET", ActorTokenEnv: "MCP_ACTOR"}}}}}
		if err := Validate(cfg); err != nil {
			t.Fatalf("Validate(%s) = %v", tokenURL, err)
		}
	}
}

func TestMCPTokenExchangeRejectsLiteralSecretFields(t *testing.T) {
	for _, field := range []string{"client_secret", "actor_token"} {
		yaml := "apiVersion: omniswitch.dev/v1\nkind: GatewayConfig\nmcp:\n  targets:\n    - name: ops\n      upstream: https://mcp.example.com\n      token_exchange:\n        token_url: https://auth.example.com/token\n        client_id: client\n        " + field + ": literal-secret\n"
		if _, err := Parse([]byte(yaml), "yaml"); err == nil || !strings.Contains(err.Error(), field) {
			t.Fatalf("Parse(%s) = %v, want rejected secret field", field, err)
		}
		json := `{"apiVersion":"omniswitch.dev/v1","kind":"GatewayConfig","mcp":{"targets":[{"name":"ops","upstream":"https://mcp.example.com","token_exchange":{"token_url":"https://auth.example.com/token","client_id":"client","` + field + `":"literal-secret"}}]}}`
		if _, err := Parse([]byte(json), "json"); err == nil || !strings.Contains(err.Error(), field) {
			t.Fatalf("JSON Parse(%s) = %v, want rejected secret field", field, err)
		}
	}
}

func TestMCPDocumentedTokenExchangeExampleValidates(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "MCP.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, block := range strings.Split(string(data), "```yaml\n")[1:] {
		yaml := strings.Split(block, "```")[0]
		if !strings.Contains(yaml, "token_exchange:") {
			continue
		}
		cfg, err := Parse([]byte(yaml), "yaml")
		if err != nil {
			t.Fatal(err)
		}
		if err := Validate(cfg); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatal("MCP.md has no worked token_exchange example")
}
