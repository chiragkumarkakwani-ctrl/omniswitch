package audit

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/omniswitch-dev/omniswitch/internal/model"
	"github.com/omniswitch-dev/omniswitch/internal/store"
)

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
