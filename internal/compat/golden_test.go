package compat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/omniswitch-dev/omniswitch/internal/provider"
)

type fixture struct {
	Source           string                `json:"source"`
	VerifiedOn       string                `json:"verified_on"`
	Kind             string                `json:"kind"`
	Request          json.RawMessage       `json:"request"`
	ExpectedChat     json.RawMessage       `json:"expected_chat"`
	ChatResponse     provider.ChatResponse `json:"chat_response"`
	ExpectedResponse json.RawMessage       `json:"expected_response"`
	Chunks           []json.RawMessage     `json:"chunks"`
	ExpectedEvents   json.RawMessage       `json:"expected_events"`
}

func TestGoldenFixtures(t *testing.T) {
	for _, api := range []string{"responses", "messages"} {
		files, err := filepath.Glob(filepath.Join("../gateway/testdata/compat", api, "*.json"))
		if err != nil || len(files) == 0 {
			t.Fatalf("fixtures for %s: %v", api, err)
		}
		for _, file := range files {
			t.Run(api+"/"+filepath.Base(file), func(t *testing.T) {
				data, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				var f fixture
				if err := json.Unmarshal(data, &f); err != nil {
					t.Fatal(err)
				}
				if f.Source == "" || f.VerifiedOn == "" {
					t.Fatal("fixture needs public source and verification date")
				}
				var responses ResponsesRequest
				var messages MessagesRequest
				if len(f.Request) > 0 {
					if api == "responses" {
						err = json.Unmarshal(f.Request, &responses)
					} else {
						err = json.Unmarshal(f.Request, &messages)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				switch f.Kind {
				case "request":
					var chat provider.ChatRequest
					if api == "responses" {
						chat, err = ResponsesToChat(responses)
					} else {
						chat, err = MessagesToChat(messages)
					}
					if err != nil {
						t.Fatal(err)
					}
					assertJSON(t, chat, f.ExpectedChat)
				case "response":
					if api == "responses" {
						assertJSON(t, ResponsesFromChat(responses, f.ChatResponse), f.ExpectedResponse)
					} else {
						result, err := MessagesFromChat(f.ChatResponse)
						if err != nil {
							t.Fatal(err)
						}
						assertJSON(t, result, f.ExpectedResponse)
					}
				case "stream":
					var translator interface{ Translate(string) ([]Event, error) }
					if api == "responses" {
						translator = NewResponsesStream(responses)
					} else {
						translator = NewMessagesStream(messages.Model)
					}
					events := []Event{}
					for _, raw := range f.Chunks {
						payload := string(raw)
						if text, ok := rawString(raw); ok {
							payload = text
						}
						translated, err := translator.Translate(payload)
						if err != nil {
							t.Fatal(err)
						}
						events = append(events, translated...)
					}
					assertJSON(t, events, f.ExpectedEvents)
				default:
					t.Fatalf("unknown fixture kind %q", f.Kind)
				}
			})
		}
	}
}

func assertJSON(t *testing.T, value any, want json.RawMessage) {
	t.Helper()
	got, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON mismatch\ngot: %s\nwant: %s", got, want)
	}
}
