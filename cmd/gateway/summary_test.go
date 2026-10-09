package main

import (
	"reflect"
	"testing"
)

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
