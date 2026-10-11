package gatewayconfig

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// MCPTokenExchange configures beta RFC 8693 delegation for an HTTP MCP target.
// Credentials are environment-variable references, never literal secret values.
type MCPTokenExchange struct {
	TokenURL           string `json:"token_url" yaml:"token_url"`
	ClientID           string `json:"client_id" yaml:"client_id"`
	ClientSecretEnv    string `json:"client_secret_env,omitempty" yaml:"client_secret_env,omitempty"`
	SubjectTokenType   string `json:"subject_token_type,omitempty" yaml:"subject_token_type,omitempty"`
	RequestedTokenType string `json:"requested_token_type,omitempty" yaml:"requested_token_type,omitempty"`
	Scope              string `json:"scope,omitempty" yaml:"scope,omitempty"`
	Audience           string `json:"audience,omitempty" yaml:"audience,omitempty"`
	Resource           string `json:"resource,omitempty" yaml:"resource,omitempty"`
	ActorTokenEnv      string `json:"actor_token_env,omitempty" yaml:"actor_token_env,omitempty"`
	ActorTokenType     string `json:"actor_token_type,omitempty" yaml:"actor_token_type,omitempty"`
}

var environmentVariableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validateMCPTokenExchange(exchange MCPTokenExchange) error {
	parsed, err := url.Parse(strings.TrimSpace(exchange.TokenURL))
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("token_url must be an absolute HTTPS URL without credentials or fragments (HTTP is accepted for localhost)")
	}
	ip := net.ParseIP(parsed.Hostname())
	localhost := strings.EqualFold(parsed.Hostname(), "localhost") || (ip != nil && ip.IsLoopback())
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && localhost) {
		return fmt.Errorf("token_url must use HTTPS (HTTP is accepted for localhost)")
	}
	if strings.TrimSpace(exchange.ClientID) == "" {
		return fmt.Errorf("client_id is required")
	}
	for _, field := range []struct{ name, value string }{
		{"client_secret_env", exchange.ClientSecretEnv}, {"actor_token_env", exchange.ActorTokenEnv},
	} {
		if field.value != "" && !environmentVariableName.MatchString(field.value) {
			return fmt.Errorf("%s must name an environment variable, not a literal secret", field.name)
		}
	}
	if exchange.ActorTokenType != "" && exchange.ActorTokenEnv == "" {
		return fmt.Errorf("actor_token_type requires actor_token_env")
	}
	return nil
}
