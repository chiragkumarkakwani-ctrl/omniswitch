package audit

import (
	"context"
	"testing"
	"time"

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
