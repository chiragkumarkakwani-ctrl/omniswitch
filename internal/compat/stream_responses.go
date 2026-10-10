package compat

import (
	"encoding/json"
	"fmt"

	"github.com/omniswitch-dev/omniswitch/internal/provider"
)

// ResponsesStream translates one chat SSE payload at a time. Completion is
// deliberately deferred until [DONE], after the separate usage-only chunk.
type ResponsesStream struct {
	request      ResponsesRequest
	response     Response
	started      bool
	completed    bool
	sequence     int
	textIndex    int
	calls        map[int]int
	finishReason string
}

func NewResponsesStream(request ResponsesRequest) *ResponsesStream {
	return &ResponsesStream{request: request, textIndex: -1, calls: make(map[int]int)}
}

func (t *ResponsesStream) event(name string, fields map[string]any) Event {
	fields["sequence_number"] = t.sequence
	t.sequence++
	return namedEvent(name, fields)
}

func (t *ResponsesStream) start(chunk provider.ChatResponseChunk) []Event {
	if t.started {
		return nil
	}
	t.started = true
	model := chunk.Model
	if model == "" {
		model = t.request.Model
	}
	t.response = responseBase(t.request, chunk.ID, model, chunk.Created)
	t.response.Status = "in_progress"
	return []Event{
		t.event("response.created", map[string]any{"response": t.response}),
		t.event("response.in_progress", map[string]any{"response": t.response}),
	}
}

func (t *ResponsesStream) Translate(payload string) ([]Event, error) {
	if t.completed {
		return nil, nil
	}
	if payload == "[DONE]" {
		return t.finish(), nil
	}
	var chunk provider.ChatResponseChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return nil, fmt.Errorf("invalid upstream chat stream: %w", err)
	}
	events := t.start(chunk)
	if chunk.Usage != nil {
		t.response.Usage = responseUsage(*chunk.Usage)
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
				t.textIndex = len(t.response.Output)
				item := ResponseOutputItem{Type: "message", ID: prefixedID("msg", t.response.ID), Role: "assistant", Status: "in_progress", Content: []ResponseContent{}}
				events = append(events, t.event("response.output_item.added", map[string]any{"output_index": t.textIndex, "item": itemSnapshot(item)}))
				item.Content = []ResponseContent{outputText("")}
				t.response.Output = append(t.response.Output, item)
				events = append(events, t.event("response.content_part.added", map[string]any{"item_id": item.ID, "output_index": t.textIndex, "content_index": 0, "part": outputText("")}))
			}
			item := &t.response.Output[t.textIndex]
			item.Content[0].Text += choice.Delta.Content
			events = append(events, t.event("response.output_text.delta", map[string]any{"item_id": item.ID, "output_index": t.textIndex, "content_index": 0, "delta": choice.Delta.Content, "logprobs": []any{}}))
		}
		for position, delta := range choice.Delta.ToolCalls {
			callIndex := position
			if delta.Index != nil {
				callIndex = *delta.Index
			}
			if callIndex < 0 {
				return nil, fmt.Errorf("invalid upstream tool call index")
			}
			index, ok := t.calls[callIndex]
			if !ok {
				index = len(t.response.Output)
				t.calls[callIndex] = index
				arguments := ""
				item := ResponseOutputItem{Type: "function_call", ID: fmt.Sprintf("%s_%d", prefixedID("fc", t.response.ID), callIndex), CallID: delta.ID, Name: delta.Function.Name, Arguments: &arguments, Status: "in_progress"}
				t.response.Output = append(t.response.Output, item)
				events = append(events, t.event("response.output_item.added", map[string]any{"output_index": index, "item": itemSnapshot(item)}))
			} else {
				item := &t.response.Output[index]
				if delta.ID != "" {
					item.CallID = delta.ID
				}
				item.Name += delta.Function.Name
			}
			item := &t.response.Output[index]
			*item.Arguments += delta.Function.Arguments
			if delta.Function.Arguments != "" {
				events = append(events, t.event("response.function_call_arguments.delta", map[string]any{"item_id": item.ID, "output_index": index, "delta": delta.Function.Arguments}))
			}
		}
	}
	return events, nil
}

func itemSnapshot(item ResponseOutputItem) ResponseOutputItem {
	if item.Arguments != nil {
		arguments := *item.Arguments
		item.Arguments = &arguments
	}
	if item.Content != nil {
		item.Content = append([]ResponseContent{}, item.Content...)
	}
	return item
}

func (t *ResponsesStream) finish() []Event {
	if t.completed {
		return nil
	}
	events := t.start(provider.ChatResponseChunk{})
	t.completed = true
	t.response.Status, t.response.IncompleteDetails = responseStatus(t.finishReason)
	if t.response.Usage == nil {
		t.response.Usage = responseUsage(provider.Usage{})
	}
	for index := range t.response.Output {
		item := &t.response.Output[index]
		if item.Type == "message" {
			item.Status = t.response.Status
			part := item.Content[0]
			events = append(events,
				t.event("response.output_text.done", map[string]any{"item_id": item.ID, "output_index": index, "content_index": 0, "text": part.Text, "logprobs": []any{}}),
				t.event("response.content_part.done", map[string]any{"item_id": item.ID, "output_index": index, "content_index": 0, "part": part}),
			)
		} else {
			item.Status = "completed"
			events = append(events, t.event("response.function_call_arguments.done", map[string]any{"item_id": item.ID, "output_index": index, "name": item.Name, "arguments": *item.Arguments}))
		}
		events = append(events, t.event("response.output_item.done", map[string]any{"output_index": index, "item": itemSnapshot(*item)}))
	}
	events = append(events, t.event("response."+t.response.Status, map[string]any{"response": t.response}))
	return events
}

// Fail closes an open Responses stream with a numbered terminal failure.
func (t *ResponsesStream) Fail(message string) []Event {
	if t.completed {
		return nil
	}
	events := t.start(provider.ChatResponseChunk{})
	t.completed = true
	t.response.Status = "failed"
	t.response.Error = map[string]string{"code": "server_error", "message": message}
	events = append(events, t.event("response.failed", map[string]any{"response": t.response}))
	return events
}
