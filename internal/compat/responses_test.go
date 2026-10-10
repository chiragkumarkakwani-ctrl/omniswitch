package compat

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/omniswitch-dev/omniswitch/internal/provider"
)

func TestResponsesRequestValidation(t *testing.T) {
	for _, test := range []struct{ name, body, param, code string }{
		{"model", `{"input":"hi"}`, "model", "invalid_parameter"},
		{"input_missing", `{"model":"m"}`, "input", "invalid_parameter"},
		{"input_null", `{"model":"m","input":null}`, "input", "invalid_parameter"},
		{"input_empty", `{"model":"m","input":[]}`, "input", "invalid_parameter"},
		{"input_object", `{"model":"m","input":{}}`, "input", "invalid_parameter"},
		{"previous", `{"model":"m","input":"hi","previous_response_id":"resp_old"}`, "previous_response_id", "unsupported_parameter"},
		{"bad_role", `{"model":"m","input":[{"role":"tool","content":"hi"}]}`, "input", "invalid_parameter"},
		{"content_null", `{"model":"m","input":[{"role":"user","content":null}]}`, "input", "invalid_parameter"},
		{"content_type", `{"model":"m","input":[{"role":"user","content":[{"type":"input_file"}]}]}`, "input", "unsupported_parameter"},
		{"file_image", `{"model":"m","input":[{"role":"user","content":[{"type":"input_image","file_id":"f"}]}]}`, "input_image", "unsupported_parameter"},
		{"item_type", `{"model":"m","input":[{"type":"item_reference"}]}`, "input", "unsupported_parameter"},
		{"call_id", `{"model":"m","input":[{"type":"function_call","name":"f"}]}`, "input", "invalid_parameter"},
		{"call_name", `{"model":"m","input":[{"type":"function_call","call_id":"c"}]}`, "input", "invalid_parameter"},
		{"output_call_id", `{"model":"m","input":[{"type":"function_call_output","output":"ok"}]}`, "input", "invalid_parameter"},
		{"bad_output", `{"model":"m","input":[{"type":"function_call_output","call_id":"c","output":42}]}`, "input", "invalid_parameter"},
		{"tokens", `{"model":"m","input":"hi","max_output_tokens":0}`, "max_output_tokens", "invalid_parameter"},
		{"tool_type", `{"model":"m","input":"hi","tools":[{"type":"web_search"}]}`, "tools", "unsupported_parameter"},
		{"tool_name", `{"model":"m","input":"hi","tools":[{"type":"function"}]}`, "tools", "invalid_parameter"},
		{"tool_choice_string", `{"model":"m","input":"hi","tool_choice":"any"}`, "tool_choice", "invalid_parameter"},
		{"tool_choice_type", `{"model":"m","input":"hi","tool_choice":42}`, "tool_choice", "invalid_parameter"},
		{"tool_choice_name", `{"model":"m","input":"hi","tool_choice":{"type":"function"}}`, "tool_choice", "invalid_parameter"},
		{"text_format", `{"model":"m","input":"hi","text":{"format":{"type":"other"}}}`, "text.format", "unsupported_parameter"},
		{"text_schema", `{"model":"m","input":"hi","text":{"format":{"type":"json_schema","name":"n"}}}`, "text.format", "invalid_parameter"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var request ResponsesRequest
			if err := json.Unmarshal([]byte(test.body), &request); err != nil {
				t.Fatal(err)
			}
			_, err := ResponsesToChat(request)
			var parameterError *RequestError
			if !errors.As(err, &parameterError) || parameterError.Param != test.param || parameterError.Code != test.code || parameterError.Error() == "" {
				t.Fatalf("error = %#v", err)
			}
		})
	}
}

func TestResponsesOptionsAndDefaults(t *testing.T) {
	for _, choice := range []string{"auto", "none", "required", "null"} {
		t.Run(choice, func(t *testing.T) {
			body := `{"model":"m","input":"hi","previous_response_id":null,"tool_choice":` + choice + `}`
			if choice != "null" {
				body = `{"model":"m","input":"hi","previous_response_id":null,"tool_choice":"` + choice + `"}`
			}
			var request ResponsesRequest
			if err := json.Unmarshal([]byte(body), &request); err != nil {
				t.Fatal(err)
			}
			chat, err := ResponsesToChat(request)
			if err != nil {
				t.Fatal(err)
			}
			if choice != "null" && chat.ToolChoice != choice {
				t.Fatalf("choice = %v", chat.ToolChoice)
			}
			response := ResponsesFromChat(request, provider.ChatResponse{ID: "resp_kept", Model: "m"})
			if response.ID != "resp_kept" || len(response.Output) != 0 || response.Store {
				t.Fatalf("defaults: %+v", response)
			}
		})
	}
	for _, format := range []string{"text", "json_object"} {
		request := ResponsesRequest{Model: "m", Input: json.RawMessage(`"hi"`), Text: &ResponseText{Format: ResponseTextFormat{Type: format}}}
		chat, err := ResponsesToChat(request)
		if err != nil || chat.ResponseFormat.Type != format {
			t.Fatalf("format %s: %+v %v", format, chat, err)
		}
	}
	var request ResponsesRequest
	if err := json.Unmarshal([]byte(`{"model":"m","input":"hi","instructions":"System","tools":[{"type":"function","name":"f"}],"tool_choice":"none","parallel_tool_calls":false,"text":{"format":{"type":"json_object"}},"reasoning":{"effort":"low"},"metadata":{"k":"v"},"max_output_tokens":12,"temperature":0.5,"top_p":0.6}`), &request); err != nil {
		t.Fatal(err)
	}
	response := ResponsesFromChat(request, provider.ChatResponse{ID: "chatcmpl-abc", Model: "m", Choices: []provider.Choice{{Message: provider.Message{Content: "ok"}, FinishReason: "content_filter"}}})
	if response.ID != "resp_abc" || response.Status != "incomplete" || response.Instructions != "System" || response.ParallelToolCalls || response.Reasoning.Effort != "low" || response.Text.Format.Type != "json_object" || response.Metadata["k"] != "v" || len(response.Tools) != 1 {
		t.Fatalf("response = %+v", response)
	}
	data, err := json.Marshal(response)
	if err != nil || !strings.Contains(string(data), `"reason":"content_filter"`) {
		t.Fatalf("response json = %s %v", data, err)
	}
	if err := json.Unmarshal([]byte(`{"model":3}`), &request); err == nil {
		t.Fatal("expected decode error")
	}
	if err := json.Unmarshal([]byte(`[]`), &request); err == nil {
		t.Fatal("expected object error")
	}
}

func TestResponsesHistoryWithoutAssistantText(t *testing.T) {
	request := ResponsesRequest{Model: "m", Input: json.RawMessage(`[{"type":"function_call","call_id":"call","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"call","output":[{"type":"input_image","image_url":"https://example.com/image.png"}]}]`)}
	chat, err := ResponsesToChat(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 2 || len(chat.Messages[0].ToolCalls) != 1 || len(chat.Messages[1].ContentParts) != 1 {
		t.Fatalf("messages = %+v", chat.Messages)
	}
}
