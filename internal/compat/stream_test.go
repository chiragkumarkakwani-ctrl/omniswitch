package compat

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStreamTerminalAndInvalidPayloads(t *testing.T) {
	for _, api := range []string{"responses", "messages"} {
		t.Run(api, func(t *testing.T) {
			var stream interface{ Translate(string) ([]Event, error) }
			if api == "responses" {
				stream = NewResponsesStream(ResponsesRequest{Model: "fallback"})
			} else {
				stream = NewMessagesStream("fallback")
			}
			if _, err := stream.Translate("bad json"); err == nil {
				t.Fatal("expected decode error")
			}
			if _, err := stream.Translate(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":-1}]}}]}`); err == nil {
				t.Fatal("expected index error")
			}
		})
		t.Run(api+"empty", func(t *testing.T) {
			var stream interface{ Translate(string) ([]Event, error) }
			if api == "responses" {
				stream = NewResponsesStream(ResponsesRequest{Model: "fallback"})
			} else {
				stream = NewMessagesStream("fallback")
			}
			events, err := stream.Translate("[DONE]")
			if err != nil || len(events) == 0 {
				t.Fatalf("empty stream = %v %v", events, err)
			}
			data, _ := json.Marshal(events)
			if !strings.Contains(string(data), "fallback") {
				t.Fatal("missing fallback model")
			}
			for _, payload := range []string{"[DONE]", `{"choices":[]}`} {
				events, err = stream.Translate(payload)
				if err != nil || len(events) != 0 {
					t.Fatal("stream completed twice")
				}
			}
		})
	}
}

func TestStreamIgnoresOtherChoicesAndMergesMetadata(t *testing.T) {
	for _, api := range []string{"responses", "messages"} {
		t.Run(api, func(t *testing.T) {
			var stream interface{ Translate(string) ([]Event, error) }
			if api == "responses" {
				stream = NewResponsesStream(ResponsesRequest{})
			} else {
				stream = NewMessagesStream("")
			}
			payloads := []string{
				`{"id":"chatcmpl-a","model":"m","choices":[{"index":1,"delta":{"content":"ignored"}},{"index":0,"delta":{"tool_calls":[{"function":{"name":"get_"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"call","function":{"name":"weather","arguments":"{"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"}"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`[DONE]`,
			}
			var all []Event
			for _, payload := range payloads {
				events, err := stream.Translate(payload)
				if err != nil {
					t.Fatal(err)
				}
				all = append(all, events...)
			}
			data, _ := json.Marshal(all)
			if strings.Contains(string(data), "ignored") || !strings.Contains(string(data), "get_weather") {
				t.Fatalf("events = %s", data)
			}
		})
	}
}

func TestMessagesStreamRejectsMalformedArguments(t *testing.T) {
	for _, args := range []string{"{", "null", "[]"} {
		stream := NewMessagesStream("m")
		payload := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"f","arguments":` + string(mustJSON(t, args)) + `}}]}}]}`
		if _, err := stream.Translate(payload); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Translate("[DONE]"); err == nil || err.Error() != "upstream returned malformed tool arguments" {
			t.Fatalf("error = %v", err)
		}
		if events, err := stream.Translate("[DONE]"); err != nil || len(events) != 0 {
			t.Fatal("must stay terminal after error")
		}
	}
	stream := NewMessagesStream("m")
	if _, err := stream.Translate(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Translate("[DONE]"); err == nil {
		t.Fatal("expected incomplete metadata error")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
