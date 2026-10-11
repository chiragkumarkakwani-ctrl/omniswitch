package compat

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/omniswitch-dev/omniswitch/internal/provider"
)

// MessagesRequest models client tools and text/image Messages requests.
type MessagesRequest struct {
	Model         string             `json:"model"`
	MaxTokens     *int               `json:"max_tokens"`
	System        json.RawMessage    `json:"system,omitempty"`
	Messages      []InputMessage     `json:"messages"`
	Tools         []MessageTool      `json:"tools,omitempty"`
	ToolChoice    *MessageToolChoice `json:"tool_choice,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	TopK          *int               `json:"top_k,omitempty"`
	Metadata      struct {
		UserID string `json:"user_id,omitempty"`
	} `json:"metadata,omitempty"`
	Stream bool `json:"stream,omitempty"`
}

type InputMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type MessageTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema"`
}

type MessageToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
}

type inputBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Source    *imageSource    `json:"source,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// MessagesToChat does not forward top_k to non-Anthropic providers. HTTP glue
// attaches it to the request context for the native Anthropic adapter only.
func MessagesToChat(r MessagesRequest) (provider.ChatRequest, error) {
	if strings.TrimSpace(r.Model) == "" {
		return provider.ChatRequest{}, invalid("model", "model is required")
	}
	if r.MaxTokens == nil || *r.MaxTokens <= 0 {
		return provider.ChatRequest{}, invalid("max_tokens", "max_tokens is required and must be positive")
	}
	if len(r.Messages) == 0 {
		return provider.ChatRequest{}, invalid("messages", "messages must be a nonempty array")
	}
	chat := provider.ChatRequest{Model: r.Model, MaxTokens: r.MaxTokens, Temperature: r.Temperature, TopP: r.TopP, Stop: r.StopSequences, User: r.Metadata.UserID, Stream: r.Stream}
	if len(r.System) > 0 && string(r.System) != "null" {
		system, err := textBlocks(r.System, "system")
		if err != nil {
			return provider.ChatRequest{}, err
		}
		if system != "" {
			chat.Messages = append(chat.Messages, provider.Message{Role: "system", Content: system})
		}
	}
	for _, message := range r.Messages {
		if message.Role != "user" && message.Role != "assistant" {
			return provider.ChatRequest{}, invalid("messages", "message role must be user or assistant")
		}
		converted, err := messageToChat(message)
		if err != nil {
			return provider.ChatRequest{}, err
		}
		chat.Messages = append(chat.Messages, converted...)
	}
	for _, tool := range r.Tools {
		if tool.Name == "" || tool.InputSchema == nil {
			return provider.ChatRequest{}, invalid("tools", "tools require name and input_schema")
		}
		chat.Tools = append(chat.Tools, provider.Tool{Type: "function", Function: provider.ToolFunction{Name: tool.Name, Description: tool.Description, Parameters: tool.InputSchema}})
	}
	if r.ToolChoice != nil {
		switch r.ToolChoice.Type {
		case "auto":
			chat.ToolChoice = "auto"
		case "any":
			chat.ToolChoice = "required"
		case "tool":
			if r.ToolChoice.Name == "" {
				return provider.ChatRequest{}, invalid("tool_choice", "tool_choice tool requires name")
			}
			chat.ToolChoice = functionChoice(r.ToolChoice.Name)
		default:
			return provider.ChatRequest{}, invalid("tool_choice", "tool_choice type must be auto, any, or tool")
		}
		if r.ToolChoice.DisableParallelToolUse != nil {
			parallel := !*r.ToolChoice.DisableParallelToolUse
			chat.ParallelToolCalls = &parallel
		}
	}
	if r.TopK != nil && *r.TopK < 0 {
		return provider.ChatRequest{}, invalid("top_k", "top_k must be nonnegative")
	}
	if r.Stream {
		chat.StreamOptions = &provider.StreamOptions{IncludeUsage: true}
	}
	return chat, nil
}

func textBlocks(raw json.RawMessage, param string) (string, error) {
	if text, ok := rawString(raw); ok {
		return text, nil
	}
	var blocks []inputBlock
	if err := json.Unmarshal(raw, &blocks); err != nil || blocks == nil {
		return "", invalid(param, param+" must be a string or text block array")
	}
	var texts []string
	for _, block := range blocks {
		if block.Type != "text" {
			return "", unsupported(param, param+" only supports text blocks")
		}
		texts = append(texts, block.Text)
	}
	return strings.Join(texts, "\n"), nil
}

func messageToChat(message InputMessage) ([]provider.Message, error) {
	if text, ok := rawString(message.Content); ok {
		return []provider.Message{{Role: message.Role, Content: text}}, nil
	}
	var blocks []inputBlock
	if err := json.Unmarshal(message.Content, &blocks); err != nil || blocks == nil {
		return nil, invalid("messages", "content must be a string or block array")
	}
	converted := provider.Message{Role: message.Role}
	var parts []provider.ContentPart
	var results []provider.Message
	for _, block := range blocks {
		switch block.Type {
		case "text":
			parts = append(parts, provider.ContentPart{Type: "text", Text: block.Text})
		case "image":
			if block.Source == nil {
				return nil, invalid("messages", "image requires source")
			}
			url := block.Source.URL
			switch block.Source.Type {
			case "base64":
				if block.Source.MediaType == "" || block.Source.Data == "" {
					return nil, invalid("messages", "base64 image requires media_type and data")
				}
				url = "data:" + block.Source.MediaType + ";base64," + block.Source.Data
			case "url":
				if url == "" {
					return nil, invalid("messages", "url image requires url")
				}
			default:
				return nil, unsupported("messages", "image source must be base64 or url")
			}
			parts = append(parts, provider.ContentPart{Type: "image_url", ImageURL: &provider.ImageURL{URL: url}})
		case "tool_use":
			if message.Role != "assistant" || block.ID == "" || block.Name == "" {
				return nil, invalid("messages", "tool_use requires an assistant message, id, and name")
			}
			input, err := validToolInput(string(block.Input))
			if err != nil {
				return nil, invalid("messages", "tool_use input must be a JSON object")
			}
			arguments, err := json.Marshal(input)
			if err != nil {
				return nil, err
			}
			converted.ToolCalls = append(converted.ToolCalls, provider.ToolCall{ID: block.ID, Type: "function", Function: provider.FunctionCall{Name: block.Name, Arguments: string(arguments)}})
		case "tool_result":
			if message.Role != "user" || block.ToolUseID == "" {
				return nil, invalid("messages", "tool_result requires a user message and tool_use_id")
			}
			text := ""
			if len(block.Content) > 0 {
				var err error
				text, err = textBlocks(block.Content, "tool_result.content")
				if err != nil {
					return nil, err
				}
			}
			if block.IsError {
				text = "[error] " + text
			}
			results = append(results, provider.Message{Role: "tool", ToolCallID: block.ToolUseID, Content: text})
		default:
			return nil, unsupported("messages", fmt.Sprintf("content block type %q is not supported", block.Type))
		}
	}
	setParts(&converted, parts)
	// Tool results must precede ordinary user content to satisfy pending calls.
	if len(parts) > 0 || len(converted.ToolCalls) > 0 || len(results) == 0 {
		results = append(results, converted)
	}
	return results, nil
}

// MessageResponse is an Anthropic message, not the canonical chat response.
type MessageResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Model        string         `json:"model"`
	Content      []MessageBlock `json:"content"`
	StopReason   *string        `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        MessageUsage   `json:"usage"`
}

type MessageBlock struct {
	Type  string         `json:"type"`
	Text  *string        `json:"text,omitempty"`
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input map[string]any `json:"input,omitempty"`
}

// MarshalJSON must retain input:{} for an empty, valid tool input.
func (b MessageBlock) MarshalJSON() ([]byte, error) {
	if b.Type == "tool_use" {
		return json.Marshal(struct {
			Type  string         `json:"type"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		}{b.Type, b.ID, b.Name, b.Input})
	}
	type wire MessageBlock
	return json.Marshal(wire(b))
}

type MessageUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

func messageUsage(usage provider.Usage) MessageUsage {
	// Integration: map P2b's cached token field here once merged. Current
	// Usage has no cache-read/write detail; do not infer it from totals.
	return MessageUsage{InputTokens: usage.PromptTokens, OutputTokens: usage.CompletionTokens}
}

func messageStopReason(reason string, hasTools bool) string {
	switch reason {
	case "length", "max_tokens":
		return "max_tokens"
	case "tool_calls", "tool_use":
		return "tool_use"
	case "stop_sequence":
		return "stop_sequence"
	default:
		if hasTools {
			return "tool_use"
		}
		return "end_turn"
	}
}

// MessagesFromChat rejects malformed arguments instead of manufacturing input.
func MessagesFromChat(chat provider.ChatResponse) (MessageResponse, error) {
	response := MessageResponse{ID: prefixedID("msg", chat.ID), Type: "message", Role: "assistant", Model: chat.Model, Content: []MessageBlock{}, Usage: messageUsage(chat.Usage)}
	reason := "end_turn"
	if len(chat.Choices) > 0 {
		choice := chat.Choices[0]
		reason = messageStopReason(choice.FinishReason, len(choice.Message.ToolCalls) > 0)
		if choice.Message.Content != "" {
			text := choice.Message.Content
			response.Content = append(response.Content, MessageBlock{Type: "text", Text: &text})
		}
		for _, call := range choice.Message.ToolCalls {
			input, err := validToolInput(call.Function.Arguments)
			if err != nil {
				return MessageResponse{}, err
			}
			if call.ID == "" || call.Function.Name == "" {
				return MessageResponse{}, fmt.Errorf("upstream returned incomplete tool call")
			}
			response.Content = append(response.Content, MessageBlock{Type: "tool_use", ID: call.ID, Name: call.Function.Name, Input: input})
		}
	}
	response.StopReason = &reason
	// ChatResponse does not carry the actual matched stop sequence. Leave
	// stop_sequence null rather than guessing which requested stop matched.
	return response, nil
}
