package store

import (
	"context"
	"testing"
)

func TestEndpointForPath(t *testing.T) {
	cases := map[string]string{
		"/v1/chat/completions":         "chat_completions",
		"/v1/responses":                "responses",
		"/v1/messages":                 "messages",
		"/v1/embeddings":               "embeddings",
		"/v1/moderations":              "moderations",
		"/v1/rerank":                   "rerank",
		"/v1/images/generations":       "images_generations",
		"/v1/audio/transcriptions":     "audio_transcriptions",
		"/v1/audio/speech":             "audio_speech",
		"/mcp":                         "mcp",
		"/v1/mcp/tools/call":           "mcp",
		"/a2a":                         "a2a",
		"/v1/models":                   "",
		"/.well-known/agent-card.json": "",
		"/unknown":                     "",
		"/v1/audio/speech/unknown":     "",
	}
	for path, want := range cases {
		t.Run(path, func(t *testing.T) {
			if got := EndpointForPath(path); got != want {
				t.Fatalf("EndpointForPath(%q) = %q, want %q", path, got, want)
			}
		})
	}
}

func TestWithRequestEndpointPreservesOriginalPath(t *testing.T) {
	ctx := WithRequestEndpoint(context.Background(), "/v1/messages")
	ctx = WithRequestEndpoint(ctx, "/v1/chat/completions")
	if got := RequestEndpoint(ctx); got != "messages" {
		t.Fatalf("translated request endpoint = %q, want messages", got)
	}
}
