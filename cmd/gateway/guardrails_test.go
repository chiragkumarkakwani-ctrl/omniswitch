package main

import (
	"testing"

	"github.com/omniswitch-dev/omniswitch/internal/gatewayconfig"
	"github.com/omniswitch-dev/omniswitch/internal/guardrail"
	"github.com/omniswitch-dev/omniswitch/internal/provider"
)

func TestFileGuardrailFallbackReachesRuntime(t *testing.T) {
	cfg, err := gatewayconfig.Parse([]byte(`guardrails:
  rules:
    - name: sensitive
      stage: input
      pattern: private
      action: fallback
      fallback: safe/model
`), ".yaml")
	if err != nil {
		t.Fatal(err)
	}
	engine := guardrail.NewEngineWithConfig(guardrailConfigFromGatewayConfig(cfg, guardrail.Config{}))
	results := engine.EvaluateInput([]provider.Message{{Role: "user", Content: "private text"}})
	for _, result := range results {
		if result.Type == "sensitive" {
			if !result.Triggered || result.Action != "fallback" || result.Fallback != "safe/model" {
				t.Fatalf("fallback result = %+v, want triggered safe/model reroute", result)
			}
			return
		}
	}
	t.Fatalf("configured regex guardrail missing from runtime results: %+v", results)
}
