package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/omniswitch-dev/omniswitch/internal/compat"
	"github.com/omniswitch-dev/omniswitch/internal/guardrail"
	"github.com/omniswitch-dev/omniswitch/internal/provider"
	"github.com/omniswitch-dev/omniswitch/internal/router"
)

func TestCompatNativeAnthropicToolStreams(t *testing.T) {
	for _, api := range []string{"responses", "messages"} {
		for _, buffered := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/buffered=%t", api, buffered), func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var request struct {
						Model     string                  `json:"model"`
						Stream    bool                    `json:"stream"`
						MaxTokens int                     `json:"max_tokens"`
						Tools     []struct{ Name string } `json:"tools"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil || r.URL.Path != "/v1/messages" || !request.Stream || request.MaxTokens != 64 || len(request.Tools) != 2 || request.Tools[0].Name != "get_weather" || request.Tools[1].Name != "get_time" {
						t.Errorf("native request = %+v, path = %s, error = %v", request, r.URL.Path, err)
						w.WriteHeader(400)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					payloads := []string{
						fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_native","model":%q,"usage":{"input_tokens":11,"output_tokens":0}}}`, request.Model),
						`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
						`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Checking."}}`,
						`{"type":"content_block_stop","index":0}`,
						`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_weather","name":"get_weather","input":{}}}`,
						`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
						`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Boston\"}"}}`,
						`{"type":"content_block_stop","index":1}`,
						`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call_time","name":"get_time","input":{}}}`,
						`{"type":"content_block_stop","index":2}`,
						`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":7}}`,
						`{"type":"message_stop"}`,
					}
					for _, payload := range payloads {
						fmt.Fprintf(w, "data: %s\n\n", payload)
						w.(http.Flusher).Flush()
					}
				}))
				defer upstream.Close()
				native := provider.NewAnthropic("test", provider.WithAnthropicBaseURL(upstream.URL+"/"))
				registry := provider.NewRegistry()
				registry.Register(native)
				handler := New(registry, router.New(registry), newGatewayTestStore(t), guardrail.NewEngine())
				handler.SetStreamGuardrailBuffer(buffered)
				request := map[string]any{"model": native.Models()[0].ID, "stream": true}
				if api == "responses" {
					request["input"], request["max_output_tokens"] = "Weather?", 64
					request["tools"] = []any{
						map[string]any{"type": "function", "name": "get_weather", "parameters": map[string]any{"type": "object"}},
						map[string]any{"type": "function", "name": "get_time", "parameters": map[string]any{"type": "object"}},
					}
				} else {
					request["max_tokens"] = 64
					request["messages"] = []any{map[string]any{"role": "user", "content": "Weather?"}}
					request["tools"] = []any{
						map[string]any{"name": "get_weather", "input_schema": map[string]any{"type": "object"}},
						map[string]any{"name": "get_time", "input_schema": map[string]any{"type": "object"}},
					}
				}
				rec := serveCompat(handler, api, compatJSON(t, request))
				if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
					t.Fatalf("status/headers/body = %d %v %s", rec.Code, rec.Header(), rec.Body.String())
				}
				events := readCompatEvents(t, rec.Body.String())
				if api == "responses" {
					var terminal struct {
						Response compat.Response `json:"response"`
					}
					if events[len(events)-1].Event != "response.completed" {
						t.Fatalf("terminal event = %+v", events[len(events)-1])
					}
					if err := json.Unmarshal(events[len(events)-1].Data.(json.RawMessage), &terminal); err != nil {
						t.Fatal(err)
					}
					result := terminal.Response
					if len(result.Output) != 3 || result.Output[1].CallID != "call_weather" || result.Output[1].Name != "get_weather" || result.Output[1].Arguments == nil || *result.Output[1].Arguments != `{"city":"Boston"}` || result.Output[2].CallID != "call_time" || result.Output[2].Name != "get_time" || result.Output[2].Arguments == nil || *result.Output[2].Arguments != `{}` || result.Usage == nil || result.Usage.InputTokens != 11 || result.Usage.OutputTokens != 7 || result.Usage.TotalTokens != 18 {
						t.Fatalf("native response = %+v", result)
					}
					return
				}
				if events[len(events)-1].Event != "message_stop" {
					t.Fatalf("terminal event = %+v", events[len(events)-1])
				}
				calls := make(map[int]compat.MessageBlock)
				args := make(map[int]string)
				var usage compat.MessageUsage
				for _, event := range events {
					var data struct {
						Index        int                 `json:"index"`
						ContentBlock compat.MessageBlock `json:"content_block"`
						Usage        compat.MessageUsage `json:"usage"`
						Delta        struct {
							Type        string `json:"type"`
							PartialJSON string `json:"partial_json"`
						} `json:"delta"`
					}
					if err := json.Unmarshal(event.Data.(json.RawMessage), &data); err != nil {
						t.Fatal(err)
					}
					if event.Event == "content_block_start" && data.ContentBlock.Type == "tool_use" {
						calls[data.Index] = data.ContentBlock
					}
					if data.Delta.Type == "input_json_delta" {
						args[data.Index] += data.Delta.PartialJSON
					}
					if event.Event == "message_delta" {
						usage = data.Usage
					}
				}
				if len(calls) != 2 || calls[1].ID != "call_weather" || calls[1].Name != "get_weather" || args[1] != `{"city":"Boston"}` || calls[2].ID != "call_time" || calls[2].Name != "get_time" || args[2] != `{}` || usage.InputTokens != 11 || usage.OutputTokens != 7 {
					t.Fatalf("native message calls/arguments/usage = %+v %+v %+v", calls, args, usage)
				}
			})
		}
	}
}
