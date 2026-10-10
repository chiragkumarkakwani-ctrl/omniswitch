package audit

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/model"
	"github.com/omniswitch-dev/omniswitch/internal/store"
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
			logs, _, err := st.ListLogs(ctx, 10, 0, "", "")
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
	logs, total, err := st.ListLogs(context.Background(), 10, 0, "mcp", "")
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
