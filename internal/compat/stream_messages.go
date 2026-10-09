package compat

import (
	"encoding/json"
	"fmt"

	"github.com/omniswitch-dev/omniswitch/internal/provider"
)

type messageStreamBlock struct {
	index     int
	id        string
	name      string
	arguments string
	pending   string
	started   bool
	tool      bool
}

// MessagesStream tracks stable block indices, including interleaved tool calls.
// It validates complete arguments before closing a block. HTTP glue can turn
// an error into HTTP 502 before headers, or an SSE error after headers.
type MessagesStream struct {
	model        string
	message      MessageResponse
	started      bool
	completed    bool
	textIndex    int
	blocks       []*messageStreamBlock
	calls        map[int]*messageStreamBlock
	finishReason string
}

func NewMessagesStream(model string) *MessagesStream {
	return &MessagesStream{model: model, textIndex: -1, calls: make(map[int]*messageStreamBlock)}
}

func (t *MessagesStream) start(chunk provider.ChatResponseChunk) []Event {
	if t.started {
		return nil
	}
	t.started = true
	model := chunk.Model
	if model == "" {
		model = t.model
	}
	t.message = MessageResponse{ID: prefixedID("msg", chunk.ID), Type: "message", Role: "assistant", Model: model, Content: []MessageBlock{}}
	if chunk.Usage != nil {
		t.message.Usage = messageUsage(*chunk.Usage)
		t.message.Usage.OutputTokens = 0
	}
	return []Event{namedEvent("message_start", map[string]any{"message": t.message})}
}

func (t *MessagesStream) Translate(payload string) ([]Event, error) {
	if t.completed {
		return nil, nil
	}
	if payload == "[DONE]" {
		return t.finish()
	}
	var chunk provider.ChatResponseChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return nil, fmt.Errorf("invalid upstream chat stream: %w", err)
	}
	events := t.start(chunk)
	if chunk.Usage != nil {
		t.message.Usage = messageUsage(*chunk.Usage)
	}
	for _, choice := range chunk.Choices {
		if choice.Index != 0 {
			continue
		}
		if choice.FinishReason != "" {
			t.finishReason = choice.FinishReason
		}
		if choice.Delta.Content != "" {
			if t.textIndex < 0 {
				t.textIndex = len(t.blocks)
				t.blocks = append(t.blocks, &messageStreamBlock{index: t.textIndex, started: true})
				events = append(events, namedEvent("content_block_start", map[string]any{"index": t.textIndex, "content_block": map[string]string{"type": "text", "text": ""}}))
			}
			events = append(events, namedEvent("content_block_delta", map[string]any{"index": t.textIndex, "delta": map[string]string{"type": "text_delta", "text": choice.Delta.Content}}))
		}
		for position, delta := range choice.Delta.ToolCalls {
			callIndex := position
			if delta.Index != nil {
				callIndex = *delta.Index
			}
			if callIndex < 0 {
				return nil, fmt.Errorf("invalid upstream tool call index")
			}
			block, ok := t.calls[callIndex]
			if !ok {
				block = &messageStreamBlock{index: len(t.blocks), tool: true}
				t.calls[callIndex] = block
				t.blocks = append(t.blocks, block)
			}
			if delta.ID != "" {
				block.id = delta.ID
			}
			block.name += delta.Function.Name
			block.arguments += delta.Function.Arguments
			block.pending += delta.Function.Arguments
			// Wait for arguments to start so fragmented function names can be
			// assembled before the immutable content_block_start is sent.
			if !block.started && block.pending != "" && block.id != "" && block.name != "" {
				block.started = true
				events = append(events, namedEvent("content_block_start", map[string]any{"index": block.index, "content_block": MessageBlock{Type: "tool_use", ID: block.id, Name: block.name, Input: map[string]any{}}}))
			}
			if block.started && block.pending != "" {
				events = append(events, namedEvent("content_block_delta", map[string]any{"index": block.index, "delta": map[string]string{"type": "input_json_delta", "partial_json": block.pending}}))
				block.pending = ""
			}
		}
	}
	return events, nil
}

func (t *MessagesStream) finish() ([]Event, error) {
	if t.completed {
		return nil, nil
	}
	for _, block := range t.blocks {
		if block.tool {
			if _, err := validToolInput(block.arguments); err != nil {
				t.completed = true
				return nil, err
			}
			if block.id == "" || block.name == "" {
				t.completed = true
				return nil, fmt.Errorf("upstream returned incomplete tool call")
			}
		}
	}
	events := t.start(provider.ChatResponseChunk{})
	t.completed = true
	for _, block := range t.blocks {
		events = append(events, namedEvent("content_block_stop", map[string]any{"index": block.index}))
	}
	reason := messageStopReason(t.finishReason, len(t.calls) > 0)
	events = append(events,
		namedEvent("message_delta", map[string]any{"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil}, "usage": t.message.Usage}),
		namedEvent("message_stop", map[string]any{}),
	)
	return events, nil
}

// MessagePing is a transport-independent Anthropic keepalive event.
func MessagePing() Event { return namedEvent("ping", map[string]any{}) }
