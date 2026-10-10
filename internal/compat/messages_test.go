package compat

import (
	"encoding/json"
	"testing"

	"github.com/omniswitch-dev/omniswitch/internal/provider"
)

func TestMessagesRequestValidation(t *testing.T) {
	for _, test := range []struct{ name, fields string }{
		{"missing_model", `"max_tokens":1,"messages":[{"role":"user","content":"hi"}]`},
		{"missing_max", `"model":"m","messages":[{"role":"user","content":"hi"}]`},
		{"zero_max", `"model":"m","max_tokens":0,"messages":[{"role":"user","content":"hi"}]`},
		{"no_messages", `"model":"m","max_tokens":1`},
		{"bad_role", `"messages":[{"role":"system","content":"hi"}]`},
		{"system_type", `"system":4`},
		{"system_block", `"system":[{"type":"image"}]`},
		{"content_null", `"messages":[{"role":"user","content":null}]`},
		{"content_type", `"messages":[{"role":"user","content":4}]`},
		{"bad_block", `"messages":[{"role":"user","content":[{"type":"thinking"}]}]`},
		{"image_source", `"messages":[{"role":"user","content":[{"type":"image"}]}]`},
		{"image_base64", `"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"x"}}]}]`},
		{"image_url", `"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url"}}]}]`},
		{"image_type", `"messages":[{"role":"user","content":[{"type":"image","source":{"type":"file"}}]}]`},
		{"tool_use_role", `"messages":[{"role":"user","content":[{"type":"tool_use","id":"c","name":"f","input":{}}]}]`},
		{"tool_use_id", `"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"f","input":{}}]}]`},
		{"tool_use_input", `"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"f","input":[]}]}]`},
		{"tool_result_role", `"messages":[{"role":"assistant","content":[{"type":"tool_result","tool_use_id":"c","content":"ok"}]}]`},
		{"tool_result_id", `"messages":[{"role":"user","content":[{"type":"tool_result","content":"ok"}]}]`},
		{"tool_result_content", `"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":[{"type":"image"}]}]}]`},
		{"tool_schema", `"tools":[{"name":"f"}]`},
		{"tool_name", `"tools":[{"input_schema":{}}]`},
		{"choice_name", `"tool_choice":{"type":"tool"}`},
		{"choice_type", `"tool_choice":{"type":"bad"}`},
		{"top_k", `"top_k":-1`},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := `"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}],`
			if test.name == "missing_model" || test.name == "missing_max" || test.name == "zero_max" || test.name == "no_messages" {
				base = ""
			}
			var request MessagesRequest
			if err := json.Unmarshal([]byte("{"+base+test.fields+"}"), &request); err != nil {
				t.Fatal(err)
			}
			if _, err := MessagesToChat(request); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestMessagesOptionalBlocksAndChoices(t *testing.T) {
	for _, system := range []string{`null`, `""`, `[]`} {
		var request MessagesRequest
		if err := json.Unmarshal([]byte(`{"model":"m","max_tokens":1,"system":`+system+`,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c"}]}],"tool_choice":{"type":"auto"}}`), &request); err != nil {
			t.Fatal(err)
		}
		chat, err := MessagesToChat(request)
		if err != nil || chat.ToolChoice != "auto" || len(chat.Messages) != 1 || chat.Messages[0].Content != "" {
			t.Fatalf("chat = %+v %v", chat, err)
		}
	}
	var request MessagesRequest
	if err := json.Unmarshal([]byte(`{"model":"m","max_tokens":1,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"f","input":{}}]}]}`), &request); err != nil {
		t.Fatal(err)
	}
	chat, err := MessagesToChat(request)
	if err != nil || chat.Messages[0].ToolCalls[0].Function.Arguments != "{}" {
		t.Fatalf("empty tool input: %+v %v", chat, err)
	}
}

func TestMessagesFromChatErrorsAndStopReasons(t *testing.T) {
	for _, arguments := range []string{"", "not json", "{", "null", "[]", "1", `"text"`} {
		chat := provider.ChatResponse{Choices: []provider.Choice{{Message: provider.Message{ToolCalls: []provider.ToolCall{{Function: provider.FunctionCall{Arguments: arguments}}}}}}}
		if _, err := MessagesFromChat(chat); err == nil || err.Error() != "upstream returned malformed tool arguments" {
			t.Fatalf("arguments %q: %v", arguments, err)
		}
	}
	for _, test := range []struct {
		finish string
		tools  bool
		want   string
	}{
		{"stop", false, "end_turn"}, {"", true, "tool_use"}, {"tool_calls", true, "tool_use"}, {"tool_use", true, "tool_use"}, {"length", false, "max_tokens"}, {"max_tokens", false, "max_tokens"}, {"stop_sequence", false, "stop_sequence"},
	} {
		t.Run(test.finish+test.want, func(t *testing.T) {
			choice := provider.Choice{FinishReason: test.finish}
			if test.tools {
				choice.Message.ToolCalls = []provider.ToolCall{{ID: "c", Function: provider.FunctionCall{Name: "f", Arguments: "{}"}}}
			}
			response, err := MessagesFromChat(provider.ChatResponse{ID: "msg_kept", Choices: []provider.Choice{choice}})
			if err != nil || *response.StopReason != test.want || response.StopSequence != nil || response.ID != "msg_kept" {
				t.Fatalf("response = %+v %v", response, err)
			}
		})
	}
	response, err := MessagesFromChat(provider.ChatResponse{})
	if err != nil || *response.StopReason != "end_turn" || len(response.Content) != 0 {
		t.Fatalf("empty = %+v %v", response, err)
	}
}

func TestMessagesErrors(t *testing.T) {
	for _, test := range []struct {
		status int
		want   string
	}{
		{400, "invalid_request_error"}, {402, "invalid_request_error"}, {401, "authentication_error"}, {403, "permission_error"}, {429, "rate_limit_error"}, {500, "api_error"}, {502, "api_error"}, {503, "overloaded_error"}, {529, "overloaded_error"},
	} {
		result := MessagesError(test.status, []byte(`{"error":{"message":"cause"}}`))
		if result["type"] != "error" || result["error"].(map[string]string)["type"] != test.want || result["error"].(map[string]string)["message"] != "cause" {
			t.Fatalf("error = %v", result)
		}
	}
	if MessagesError(502, nil)["error"].(map[string]string)["message"] != "request failed" {
		t.Fatal("missing fallback")
	}
	if MessagesError(502, []byte("upstream error"))["error"].(map[string]string)["message"] != "upstream error" {
		t.Fatal("lost raw message")
	}
	assertJSON(t, MessagePing(), json.RawMessage(`{"event":"ping","data":{"type":"ping"}}`))
}

func TestMessagesToolInputRetainsLargeJSONNumbers(t *testing.T) {
	arguments := `{"id":9007199254740993,"amount":1.234567890123456789,"large":1e400}`
	chat := provider.ChatResponse{Choices: []provider.Choice{{Message: provider.Message{ToolCalls: []provider.ToolCall{{ID: "c", Function: provider.FunctionCall{Name: "f", Arguments: arguments}}}}}}}
	response, err := MessagesFromChat(chat)
	if err != nil {
		t.Fatal(err)
	}
	input := response.Content[0].Input
	if input["id"].(json.Number).String() != "9007199254740993" || input["amount"].(json.Number).String() != "1.234567890123456789" || input["large"].(json.Number).String() != "1e400" {
		t.Fatalf("rounded tool input: %v", input)
	}
	data, err := json.Marshal(response)
	if err != nil || len(data) == 0 {
		t.Fatalf("number encoding = %s %v", data, err)
	}
	request := MessagesRequest{Model: "m", MaxTokens: newInt(64), Messages: []InputMessage{{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"c","name":"f","input":` + arguments + `}]`)}}}
	converted, err := MessagesToChat(request)
	if err != nil {
		t.Fatal(err)
	}
	again, err := validToolInput(converted.Messages[0].ToolCalls[0].Function.Arguments)
	if err != nil || again["id"] != input["id"] || again["amount"] != input["amount"] || again["large"] != input["large"] {
		t.Fatalf("input history changed: %v %v", again, err)
	}
}

func newInt(value int) *int { return &value }

func TestMessagesRejectsTrailingArgumentsAndIncompleteToolMetadata(t *testing.T) {
	if _, err := validToolInput(`{} {}`); err == nil {
		t.Fatal("multiple JSON objects are not one input")
	}
	for _, call := range []provider.ToolCall{{Function: provider.FunctionCall{Name: "f", Arguments: "{}"}}, {ID: "c", Function: provider.FunctionCall{Arguments: "{}"}}} {
		if _, err := MessagesFromChat(provider.ChatResponse{Choices: []provider.Choice{{Message: provider.Message{ToolCalls: []provider.ToolCall{call}}}}}); err == nil || err.Error() != "upstream returned incomplete tool call" {
			t.Fatalf("error = %v", err)
		}
	}
}
