package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAnthropicCompatOptionsReachNativeUpstream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			received := make(chan anthropicRequest, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request anthropicRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				received <- request
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"model\":\"test-model\",\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"id":"msg_test","model":"test-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`)
				}
			}))
			defer upstream.Close()
			client := NewAnthropic("test")
			client.baseURL = upstream.URL
			maxTokens, maxCompletion, topK := 30, 40, 10
			temperature, topP, parallel := 0.2, 0.8, false
			request := ChatRequest{Model: "test-model", Messages: []Message{{Role: "user", Content: "hi"}}, MaxTokens: &maxTokens, MaxCompletionTokens: &maxCompletion, Temperature: &temperature, TopP: &topP, Stop: []string{"END"}, User: "user-123", ToolChoice: "required", ParallelToolCalls: &parallel, StreamOptions: &StreamOptions{IncludeUsage: true}}
			ctx := WithAnthropicTopK(context.Background(), topK)
			if stream {
				chunks, _, err := client.ChatCompletionStream(ctx, request)
				if err != nil {
					t.Fatal(err)
				}
				for range chunks {
				}
			} else {
				if _, _, err := client.ChatCompletion(ctx, request); err != nil {
					t.Fatal(err)
				}
			}
			got := <-received
			if got.MaxTokens != maxCompletion || got.TopK == nil || *got.TopK != topK || got.Temperature == nil || *got.Temperature != temperature || got.TopP == nil || *got.TopP != topP || len(got.StopSequences) != 1 || got.StopSequences[0] != "END" || got.Metadata["user_id"] != "user-123" || got.ToolChoice.Type != "any" || got.ToolChoice.DisableParallelToolUse == nil || !*got.ToolChoice.DisableParallelToolUse {
				t.Fatalf("native request = %+v", got)
			}
		})
	}
}

func TestAnthropicParallelChoiceDefaults(t *testing.T) {
	parallel := false
	request, err := toAnthropicRequest(ChatRequest{ParallelToolCalls: &parallel}, false)
	if err != nil || request.ToolChoice.Type != "auto" || !*request.ToolChoice.DisableParallelToolUse {
		t.Fatalf("default = %+v %v", request, err)
	}
	request, err = toAnthropicRequest(ChatRequest{ToolChoice: "none", ParallelToolCalls: &parallel}, false)
	if err != nil || request.ToolChoice.Type != "none" || request.ToolChoice.DisableParallelToolUse != nil {
		t.Fatalf("none = %+v %v", request, err)
	}
}
