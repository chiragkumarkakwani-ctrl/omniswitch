package audit

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/model"
	"github.com/omniswitch-dev/omniswitch/internal/store"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func TestStoreLoggerRecordsMCPEndpoint(t *testing.T) {
	for _, path := range []string{"", "/mcp", "/v1/mcp/tools/call"} {
		t.Run(path, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			ctx := context.Background()
			if path != "" {
				ctx = store.WithRequestEndpoint(ctx, path)
			}
			if err := NewStoreLogger(st).Log(ctx, Event{DecisionID: "mcp-event", Timestamp: time.Now().UTC(), Tool: "test_tool", Decision: "ALLOW"}); err != nil {
				t.Fatal(err)
			}
			logs, _, err := st.ListLogs(ctx, store.LogFilter{Limit: 10, Offset: 0, Provider: ""})
			if err != nil || len(logs) != 1 || logs[0].Endpoint != "mcp" {
				t.Fatalf("MCP endpoint logs = %+v, err=%v", logs, err)
			}
		})
	}
}

func TestStoreLoggerPersistsDiscoveryMetadata(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	event := NewEvent(model.ToolRequest{Tool: model.Tool{Name: "ops"}, Action: model.Action{Name: "list"}}, model.Decision{ID: "listing-1", Allowed: true})
	event.Metadata = map[string]any{"hidden_tools": 2, "mcp.target": "ops"}
	if err := NewStoreLogger(st).Log(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	logs, total, err := st.ListLogs(context.Background(), store.LogFilter{Limit: 10, Provider: "mcp"})
	if err != nil || total != 1 {
		t.Fatalf("ListLogs() total/error = %d/%v, want one audit", total, err)
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(logs[0].Metadata), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["hidden_tools"] != float64(2) || metadata["mcp.target"] != "ops" {
		t.Fatalf("metadata = %v, want discovery hidden count and target", metadata)
	}
}

func TestStoreLoggerPreservesMCPIdentityMetadataAndTrace(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	event := NewEvent(model.ToolRequest{
		Agent: model.Agent{ID: "verified-key"}, Tool: model.Tool{Name: "delete_table"}, Action: model.Action{Name: "call"}, Resource: model.Resource{Name: "payments"},
		Session: model.Session{ID: "session"}, Metadata: map[string]string{"mcp.target": "ops", "private": "PRIVATE_ARGUMENT"},
	}, model.Decision{ID: "decision", Allowed: false, Rule: "deny-delete", Reason: "denied", EvaluationTime: 1500 * time.Microsecond})
	traceID, err := oteltrace.TraceIDFromHex("00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	spanID, err := oteltrace.SpanIDFromHex("0011223344556677")
	if err != nil {
		t.Fatal(err)
	}
	span := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{TraceID: traceID, SpanID: spanID})
	for i, expected := range []string{"", traceID.String(), "explicit-trace"} {
		ctx := context.Background()
		if i > 0 {
			ctx = oteltrace.ContextWithSpanContext(ctx, span)
		}
		if i == 2 {
			ctx = WithTraceID(ctx, expected)
		}
		event.DecisionID = "decision-" + expected
		if err := NewStoreLogger(st).Log(ctx, event); err != nil {
			t.Fatal(err)
		}
		logs, _, err := st.ListLogs(context.Background(), store.LogFilter{APIKeyID: "verified-key", Provider: "mcp", Sort: "time_desc"})
		if err != nil || len(logs) != i+1 {
			t.Fatalf("audit logs: %+v/%v", logs, err)
		}
		var row store.RequestLog
		for _, entry := range logs {
			if entry.ID == event.DecisionID {
				row = entry
			}
		}
		if row.TraceID != expected || row.SessionID != "session" || row.Model != "delete_table" || row.Status != "denied" || row.Endpoint != "mcp" || row.LatencyMs != 1.5 || row.RequestBody != "" || row.ResponseBody != "" {
			t.Fatalf("audit context: %+v", row)
		}
		var metadata map[string]map[string]any
		if err := json.Unmarshal([]byte(row.Metadata), &metadata); err != nil {
			t.Fatal(err)
		}
		mcp := metadata["mcp"]
		if mcp["agent_id"] != "verified-key" || mcp["target"] != "ops" || mcp["action"] != "call" || mcp["resource"] != "payments" || mcp["rule"] != "deny-delete" || mcp["latency_ms"] != 1.5 || len(mcp) != 6 {
			t.Fatalf("MCP metadata: %s", row.Metadata)
		}
	}
}
