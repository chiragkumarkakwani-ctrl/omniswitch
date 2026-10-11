package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/omniswitch-dev/omniswitch/internal/router"
)

func TestRuntimeSettingsPolicyDirectory(t *testing.T) {
	t.Setenv("OMNISWITCH_CONFIG", "")
	t.Setenv("OMNISWITCH_POLICY_DIR", "")
	settings, err := loadRuntimeSettings()
	if err != nil || settings.policyDir != "policies" {
		t.Fatalf("default policy directory = %q/%v, want policies", settings.policyDir, err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("apiVersion: omniswitch.dev/v1\nkind: GatewayConfig\ngateway:\n  policy_dir: /etc/omniswitch/policies\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMNISWITCH_CONFIG", path)
	settings, err = loadRuntimeSettings()
	if err != nil || settings.policyDir != "/etc/omniswitch/policies" {
		t.Fatalf("configured policy directory = %q/%v", settings.policyDir, err)
	}
	t.Setenv("OMNISWITCH_POLICY_DIR", "/operator/override")
	settings, err = loadRuntimeSettings()
	if err != nil || settings.policyDir != "/operator/override" {
		t.Fatalf("environment policy directory = %q/%v", settings.policyDir, err)
	}
	if summary := runtimeConfigSummary(settings, nil); summary["gateway"].(map[string]any)["policy_dir"] != settings.policyDir {
		t.Fatal("config summary does not report effective policy directory")
	}
}

func TestRouteSummaryStrategy(t *testing.T) {
	summary := routeSummaries(map[string]router.Route{"chat": {Provider: "openai", Strategy: router.StrategyRoundRobin}})
	if len(summary) != 1 || summary[0]["strategy"] != router.StrategyRoundRobin {
		t.Fatalf("route summary lost strategy: %#v", summary)
	}
}

func TestRuntimeConfigSummaryA2A(t *testing.T) {
	summary := runtimeConfigSummary(runtimeSettings{}, nil)
	want := map[string]any{
		"enabled": true, "agent_card_path": "/.well-known/agent-card.json", "jsonrpc_path": "/a2a",
		"streaming": true, "streaming_mode": "sse_task_events", "task_lifecycle": true,
		"task_store": "in_memory", "cancel": "best_effort", "push_notifications": false,
		"methods": []string{"SendMessage", "SendMessageStream", "GetTask", "CancelTask", "GetExtendedAgentCard"},
	}
	if !reflect.DeepEqual(summary["a2a"], want) {
		t.Fatalf("A2A summary = %#v, want %#v", summary["a2a"], want)
	}
}
