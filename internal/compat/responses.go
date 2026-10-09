package compat

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/omniswitch-dev/omniswitch/internal/provider"
)

// ResponsesRequest models the stateless, function-tool subset of Responses.
type ResponsesRequest struct {
	Model              string                     `json:"model"`
	Input              json.RawMessage            `json:"input"`
	Instructions       string                     `json:"instructions,omitempty"`
	Tools              []ResponseTool             `json:"tools,omitempty"`
	ToolChoice         json.RawMessage            `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool                      `json:"parallel_tool_calls,omitempty"`
	MaxOutputTokens    *int                       `json:"max_output_tokens,omitempty"`
	Temperature        *float64                   `json:"temperature,omitempty"`
	TopP               *float64                   `json:"top_p,omitempty"`
	Text               *ResponseText              `json:"text,omitempty"`
	Reasoning          *ResponseReasoning         `json:"reasoning,omitempty"`
	Metadata           map[string]string          `json:"metadata,omitempty"`
	User               string                     `json:"user,omitempty"`
	Stream             bool                       `json:"stream,omitempty"`
	Store              *bool                      `json:"store,omitempty"`
	PreviousResponseID json.RawMessage            `json:"previous_response_id,omitempty"`
	Extra              map[string]json.RawMessage `json:"-"`
}

type ResponseTool struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
	Strict      *bool  `json:"strict,omitempty"`
}

type ResponseText struct {
	Format ResponseTextFormat `json:"format"`
}

type ResponseTextFormat struct {
	Type        string `json:"type"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Schema      any    `json:"schema,omitempty"`
	Strict      *bool  `json:"strict,omitempty"`
}

type ResponseReasoning struct {
	Effort string `json:"effort,omitempty"`
}

func (r *ResponsesRequest) UnmarshalJSON(data []byte) error {
	type wire ResponsesRequest
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*r = ResponsesRequest(decoded)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, key := range []string{"model", "input", "instructions", "tools", "tool_choice", "parallel_tool_calls", "max_output_tokens", "temperature", "top_p", "text", "reasoning", "metadata", "user", "stream", "store", "previous_response_id"} {
		delete(fields, key)
	}
	if len(fields) > 0 {
		r.Extra = fields
	}
	return nil
}

// ResponsesToChat converts requests without resolving a provider or making I/O.
func ResponsesToChat(r ResponsesRequest) (provider.ChatRequest, error) {
	if len(r.PreviousResponseID) > 0 && string(r.PreviousResponseID) != "null" {
		return provider.ChatRequest{}, unsupported("previous_response_id", "previous_response_id is not supported: OmniSwitch is stateless; resend the conversation in input")
	}
	if strings.TrimSpace(r.Model) == "" {
		return provider.ChatRequest{}, invalid("model", "model is required")
	}
	messages, err := responseInput(r.Input)
	if err != nil {
		return provider.ChatRequest{}, err
	}
	if r.Instructions != "" {
		messages = append([]provider.Message{{Role: "system", Content: r.Instructions}}, messages...)
	}
	if r.MaxOutputTokens != nil && *r.MaxOutputTokens <= 0 {
		return provider.ChatRequest{}, invalid("max_output_tokens", "max_output_tokens must be positive")
	}
	store := false
	chat := provider.ChatRequest{
		Model: r.Model, Messages: messages, Temperature: r.Temperature,
		MaxCompletionTokens: r.MaxOutputTokens, TopP: r.TopP, Stream: r.Stream,
		ParallelToolCalls: r.ParallelToolCalls, Metadata: r.Metadata, User: r.User,
		Store: &store, Extra: r.Extra,
	}
	for _, tool := range r.Tools {
		if tool.Type != "function" {
			return provider.ChatRequest{}, unsupported("tools", "only function tools are supported")
		}
		if tool.Name == "" {
			return provider.ChatRequest{}, invalid("tools", "function tool name is required")
		}
		chat.Tools = append(chat.Tools, provider.Tool{Type: "function", Function: provider.ToolFunction{
			Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters, Strict: tool.Strict,
		}})
	}
	if len(r.ToolChoice) > 0 && string(r.ToolChoice) != "null" {
		if choice, ok := rawString(r.ToolChoice); ok {
			switch choice {
			case "auto", "none", "required":
				chat.ToolChoice = choice
			default:
				return provider.ChatRequest{}, invalid("tool_choice", "tool_choice must be auto, none, required, or a named function")
			}
		} else {
			var choice struct {
				Type string `json:"type"`
				Name string `json:"name"`
			}
			if err := json.Unmarshal(r.ToolChoice, &choice); err != nil || choice.Type != "function" || choice.Name == "" {
				return provider.ChatRequest{}, invalid("tool_choice", "tool_choice must name a function")
			}
			chat.ToolChoice = functionChoice(choice.Name)
		}
	}
	if r.Text != nil {
		format := r.Text.Format
		chat.ResponseFormat = &provider.ResponseFormat{Type: format.Type}
		switch format.Type {
		case "text", "json_object":
		case "json_schema":
			if format.Name == "" || format.Schema == nil {
				return provider.ChatRequest{}, invalid("text.format", "json_schema requires name and schema")
			}
			chat.ResponseFormat.JSONSchema = &provider.JSONSchemaSpec{Name: format.Name, Description: format.Description, Schema: format.Schema, Strict: format.Strict}
		default:
			return provider.ChatRequest{}, unsupported("text.format", "text.format must be text, json_object, or json_schema")
		}
	}
	if r.Reasoning != nil {
		chat.ReasoningEffort = r.Reasoning.Effort
	}
	if r.Stream {
		chat.StreamOptions = &provider.StreamOptions{IncludeUsage: true}
	}
	return chat, nil
}

func responseInput(raw json.RawMessage) ([]provider.Message, error) {
	if text, ok := rawString(raw); ok {
		return []provider.Message{{Role: "user", Content: text}}, nil
	}
	var items []struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		CallID    string          `json:"call_id"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
		Output    json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(raw, &items); err != nil || len(items) == 0 {
		return nil, invalid("input", "input must be a string or a nonempty item array")
	}
	var messages []provider.Message
	for _, item := range items {
		switch item.Type {
		case "", "message":
			if item.Role != "user" && item.Role != "assistant" && item.Role != "system" && item.Role != "developer" {
				return nil, invalid("input", "message role must be user, assistant, system, or developer")
			}
			message := provider.Message{Role: item.Role}
			parts, err := responseParts(item.Content)
			if err != nil {
				return nil, err
			}
			setParts(&message, parts)
			messages = append(messages, message)
		case "function_call":
			if item.CallID == "" || item.Name == "" {
				return nil, invalid("input", "function_call requires call_id and name")
			}
			call := provider.ToolCall{ID: item.CallID, Type: "function", Function: provider.FunctionCall{Name: item.Name, Arguments: item.Arguments}}
			if len(messages) == 0 || messages[len(messages)-1].Role != "assistant" {
				messages = append(messages, provider.Message{Role: "assistant"})
			}
			messages[len(messages)-1].ToolCalls = append(messages[len(messages)-1].ToolCalls, call)
		case "function_call_output":
			if item.CallID == "" {
				return nil, invalid("input", "function_call_output requires call_id")
			}
			parts, err := responseParts(item.Output)
			if err != nil {
				return nil, err
			}
			message := provider.Message{Role: "tool", ToolCallID: item.CallID}
			setParts(&message, parts)
			messages = append(messages, message)
		default:
			return nil, unsupported("input", fmt.Sprintf("input item type %q is not supported", item.Type))
		}
	}
	return messages, nil
}

func responseParts(raw json.RawMessage) ([]provider.ContentPart, error) {
	if text, ok := rawString(raw); ok {
		return []provider.ContentPart{{Type: "text", Text: text}}, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL string `json:"image_url"`
		Detail   string `json:"detail"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil || parts == nil {
		return nil, invalid("input", "message content and tool output must be strings or content arrays")
	}
	var result []provider.ContentPart
	for _, part := range parts {
		switch part.Type {
		case "input_text", "output_text":
			result = append(result, provider.ContentPart{Type: "text", Text: part.Text})
		case "input_image":
			if part.ImageURL == "" {
				return nil, unsupported("input_image", "input_image requires image_url; file_id is not supported")
			}
			result = append(result, provider.ContentPart{Type: "image_url", ImageURL: &provider.ImageURL{URL: part.ImageURL, Detail: part.Detail}})
		default:
			return nil, unsupported("input", fmt.Sprintf("content part type %q is not supported", part.Type))
		}
	}
	return result, nil
}

// Response is the complete stateless Responses result (including request options).
type Response struct {
	ID                 string               `json:"id"`
	Object             string               `json:"object"`
	CreatedAt          int64                `json:"created_at"`
	Status             string               `json:"status"`
	Error              any                  `json:"error"`
	IncompleteDetails  any                  `json:"incomplete_details"`
	Instructions       any                  `json:"instructions"`
	Model              string               `json:"model"`
	Output             []ResponseOutputItem `json:"output"`
	Usage              *ResponseUsage       `json:"usage"`
	Tools              []ResponseTool       `json:"tools"`
	ToolChoice         any                  `json:"tool_choice"`
	ParallelToolCalls  bool                 `json:"parallel_tool_calls"`
	Store              bool                 `json:"store"`
	Metadata           map[string]string    `json:"metadata"`
	MaxOutputTokens    *int                 `json:"max_output_tokens"`
	Temperature        *float64             `json:"temperature"`
	TopP               *float64             `json:"top_p"`
	Text               ResponseText         `json:"text"`
	Reasoning          ResponseReasoning    `json:"reasoning"`
	PreviousResponseID any                  `json:"previous_response_id"`
	Truncation         string               `json:"truncation"`
	User               string               `json:"user"`
}

type ResponseOutputItem struct {
	Type      string            `json:"type"`
	ID        string            `json:"id"`
	Status    string            `json:"status"`
	Role      string            `json:"role,omitempty"`
	Content   []ResponseContent `json:"content,omitempty"`
	CallID    string            `json:"call_id,omitempty"`
	Name      string            `json:"name,omitempty"`
	Arguments *string           `json:"arguments,omitempty"`
}

// MarshalJSON preserves required empty fields on newly added output items.
func (item ResponseOutputItem) MarshalJSON() ([]byte, error) {
	if item.Type == "message" {
		content := item.Content
		if content == nil {
			content = []ResponseContent{}
		}
		return json.Marshal(struct {
			Type    string            `json:"type"`
			ID      string            `json:"id"`
			Status  string            `json:"status"`
			Role    string            `json:"role"`
			Content []ResponseContent `json:"content"`
		}{item.Type, item.ID, item.Status, item.Role, content})
	}
	return json.Marshal(struct {
		Type      string  `json:"type"`
		ID        string  `json:"id"`
		Status    string  `json:"status"`
		CallID    string  `json:"call_id"`
		Name      string  `json:"name"`
		Arguments *string `json:"arguments"`
	}{item.Type, item.ID, item.Status, item.CallID, item.Name, item.Arguments})
}

type ResponseContent struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

type ResponseUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func responseUsage(usage provider.Usage) *ResponseUsage {
	// Integration: P2b owns provider.Usage cached/reasoning extensions. Wire
	// them here once merged; current Usage only exposes the three totals.
	return &ResponseUsage{InputTokens: usage.PromptTokens, OutputTokens: usage.CompletionTokens, TotalTokens: usage.TotalTokens}
}

func responseBase(request ResponsesRequest, id, model string, created int64) Response {
	tools := request.Tools
	if tools == nil {
		tools = []ResponseTool{}
	}
	metadata := request.Metadata
	if metadata == nil {
		metadata = map[string]string{}
	}
	choice := any("auto")
	if len(request.ToolChoice) > 0 && string(request.ToolChoice) != "null" {
		_ = json.Unmarshal(request.ToolChoice, &choice)
	}
	parallel := true
	if request.ParallelToolCalls != nil {
		parallel = *request.ParallelToolCalls
	}
	response := Response{
		ID: prefixedID("resp", id), Object: "response", CreatedAt: created, Status: "completed", Model: model,
		Output: []ResponseOutputItem{}, Tools: tools, Metadata: metadata, ToolChoice: choice, ParallelToolCalls: parallel,
		MaxOutputTokens: request.MaxOutputTokens, Temperature: request.Temperature, TopP: request.TopP,
		Text: ResponseText{Format: ResponseTextFormat{Type: "text"}}, Truncation: "disabled", User: request.User,
	}
	if request.Instructions != "" {
		response.Instructions = request.Instructions
	}
	if request.Text != nil {
		response.Text = *request.Text
	}
	if request.Reasoning != nil {
		response.Reasoning = *request.Reasoning
	}
	return response
}

func responseStatus(reason string) (string, any) {
	switch reason {
	case "length":
		return "incomplete", map[string]string{"reason": "max_output_tokens"}
	case "content_filter":
		return "incomplete", map[string]string{"reason": "content_filter"}
	default:
		return "completed", nil
	}
}

func outputText(text string) ResponseContent {
	return ResponseContent{Type: "output_text", Text: text, Annotations: []any{}}
}

// ResponsesFromChat translates the first (only) completion choice.
func ResponsesFromChat(request ResponsesRequest, chat provider.ChatResponse) Response {
	response := responseBase(request, chat.ID, chat.Model, chat.Created)
	response.Usage = responseUsage(chat.Usage)
	if len(chat.Choices) == 0 {
		return response
	}
	choice := chat.Choices[0]
	response.Status, response.IncompleteDetails = responseStatus(choice.FinishReason)
	if choice.Message.Content != "" {
		response.Output = append(response.Output, ResponseOutputItem{Type: "message", ID: prefixedID("msg", response.ID), Role: "assistant", Status: response.Status, Content: []ResponseContent{outputText(choice.Message.Content)}})
	}
	for i, call := range choice.Message.ToolCalls {
		arguments := call.Function.Arguments
		response.Output = append(response.Output, ResponseOutputItem{Type: "function_call", ID: fmt.Sprintf("%s_%d", prefixedID("fc", response.ID), i), CallID: call.ID, Name: call.Function.Name, Arguments: &arguments, Status: "completed"})
	}
	return response
}
