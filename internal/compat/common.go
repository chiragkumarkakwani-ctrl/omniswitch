// Package compat translates agent API wire formats to and from the common chat
// model. It performs no I/O; stream translators only retain conversation state.
package compat

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/omniswitch-dev/omniswitch/internal/provider"
)

// RequestError identifies a bad or unsupported client parameter.
type RequestError struct {
	Param   string
	Code    string
	Message string
}

func (e *RequestError) Error() string { return e.Message }

func invalid(param, message string) error {
	return &RequestError{Param: param, Code: "invalid_parameter", Message: message}
}

func unsupported(param, message string) error {
	return &RequestError{Param: param, Code: "unsupported_parameter", Message: message}
}

// Event is one named SSE event, without transport framing.
type Event struct {
	Event string `json:"event"`
	Data  any    `json:"data"`
}

func namedEvent(name string, fields map[string]any) Event {
	fields["type"] = name
	return Event{Event: name, Data: fields}
}

func prefixedID(prefix, id string) string {
	if strings.HasPrefix(id, prefix+"_") {
		return id
	}
	for _, old := range []string{"chatcmpl-", "chatcmpl_", "chat_", "resp_", "msg_", "chunk_"} {
		id = strings.TrimPrefix(id, old)
	}
	if id == "" {
		id = "stream"
	}
	return prefix + "_" + id
}

func rawString(raw json.RawMessage) (string, bool) {
	var text string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &text) != nil {
		return "", false
	}
	return text, true
}

func setParts(message *provider.Message, parts []provider.ContentPart) {
	var texts []string
	multimodal := false
	for _, part := range parts {
		if part.Type == "text" {
			texts = append(texts, part.Text)
		} else {
			multimodal = true
		}
	}
	message.Content = strings.Join(texts, "\n")
	if multimodal {
		message.ContentParts = parts
	}
}

func functionChoice(name string) any {
	return map[string]any{"type": "function", "function": map[string]any{"name": name}}
}

func validToolInput(arguments string) (map[string]any, error) {
	var input map[string]any
	if err := json.Unmarshal([]byte(arguments), &input); err != nil || input == nil {
		return nil, fmt.Errorf("upstream returned malformed tool arguments")
	}
	return input, nil
}

// MessagesError preserves the chat HTTP status while using Anthropic's envelope.
func MessagesError(status int, body []byte) map[string]any {
	var original struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &original)
	message := original.Error.Message
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = "request failed"
	}
	errorType := "invalid_request_error"
	switch {
	case status == 401:
		errorType = "authentication_error"
	case status == 403:
		errorType = "permission_error"
	case status == 429:
		errorType = "rate_limit_error"
	case status == 503 || status == 529:
		errorType = "overloaded_error"
	case status >= 500:
		errorType = "api_error"
	}
	return map[string]any{"type": "error", "error": map[string]string{"type": errorType, "message": message}}
}
