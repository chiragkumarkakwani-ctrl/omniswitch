package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestAnthropicStreamDenseToolIndicesAndInitialInput(t *testing.T) {
	for _, test := range []struct {
		name   string
		input  string
		deltas []string
		want   string
	}{
		{"empty_object", `{}`, nil, `{}`},
		{"initial_object", `{"count":9007199254740993,"value":1e400}`, nil, `{"count":9007199254740993,"value":1e400}`},
		{"fragmented_object", `{}`, []string{`{"city":`, `"Boston"}`}, `{"city":"Boston"}`},
		{"missing_input_is_not_fabricated", "", nil, ""},
		{"null_is_not_replaced", `null`, nil, `null`},
		{"array_is_not_replaced", `[]`, nil, `[]`},
		{"empty_delta_is_not_replaced", `{}`, []string{""}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var body strings.Builder
			writeEvent := func(event any) {
				data, err := json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				fmt.Fprintf(&body, "data: %s\n\n", data)
			}
			writeEvent(map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_tools", "model": "test-model", "usage": map[string]int{"input_tokens": 11}}})
			writeEvent(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "Checking."}})
			block := map[string]any{"type": "tool_use", "id": "call_first", "name": "first"}
			if test.input != "" {
				block["input"] = json.RawMessage(test.input)
			}
			writeEvent(map[string]any{"type": "content_block_start", "index": 1, "content_block": block})
			for _, delta := range test.deltas {
				writeEvent(map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": delta}})
			}
			writeEvent(map[string]any{"type": "content_block_stop", "index": 1})
			writeEvent(map[string]any{"type": "content_block_delta", "index": 2, "delta": map[string]any{"type": "text_delta", "text": "Waiting."}})
			writeEvent(map[string]any{"type": "content_block_start", "index": 3, "content_block": map[string]any{"type": "tool_use", "id": "call_second", "name": "second", "input": map[string]any{}}})
			writeEvent(map[string]any{"type": "content_block_stop", "index": 3})
			writeEvent(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]int{"output_tokens": 7}})
			writeEvent(map[string]any{"type": "message_stop"})

			calls := make([]ToolCall, 2)
			var text strings.Builder
			var usage *Usage
			for _, chunk := range collectChunks(streamAnthropic(context.Background(), io.NopCloser(strings.NewReader(body.String())), "fallback", true)) {
				if chunk.Usage != nil {
					usage = chunk.Usage
				}
				for _, choice := range chunk.Choices {
					text.WriteString(choice.Delta.Content)
					for _, call := range choice.Delta.ToolCalls {
						if call.Index == nil || *call.Index < 0 || *call.Index >= len(calls) {
							t.Fatalf("tool index must be dense: %+v", call)
						}
						current := &calls[*call.Index]
						current.ID += call.ID
						current.Function.Name += call.Function.Name
						current.Function.Arguments += call.Function.Arguments
					}
				}
			}
			if calls[0].ID != "call_first" || calls[0].Function.Name != "first" || calls[0].Function.Arguments != test.want || calls[1].ID != "call_second" || calls[1].Function.Name != "second" || calls[1].Function.Arguments != `{}` {
				t.Fatalf("tool calls = %+v", calls)
			}
			if text.String() != "Checking.Waiting." || usage == nil || usage.PromptTokens != 11 || usage.CompletionTokens != 7 || usage.TotalTokens != 18 {
				t.Fatalf("text/usage = %q %+v", text.String(), usage)
			}
		})
	}
}
