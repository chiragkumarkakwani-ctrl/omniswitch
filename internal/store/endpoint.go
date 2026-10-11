package store

import "context"

type requestEndpointKey struct{}

// EndpointForPath gives request logs a stable endpoint name. Keep this mapping
// shared by gateway requests, compatibility APIs, and MCP audit writers.
func EndpointForPath(path string) string {
	switch path {
	case "/v1/chat/completions":
		return "chat_completions"
	case "/v1/responses":
		return "responses"
	case "/v1/messages":
		return "messages"
	case "/v1/embeddings":
		return "embeddings"
	case "/v1/moderations":
		return "moderations"
	case "/v1/rerank":
		return "rerank"
	case "/v1/images/generations":
		return "images_generations"
	case "/v1/audio/transcriptions":
		return "audio_transcriptions"
	case "/v1/audio/speech":
		return "audio_speech"
	case "/mcp", "/v1/mcp/tools/call":
		return "mcp"
	case "/a2a":
		return "a2a"
	default:
		return ""
	}
}

// WithRequestEndpoint preserves the original endpoint through internal calls
// that reuse the chat pipeline. The input is the external request's URL.Path.
func WithRequestEndpoint(ctx context.Context, path string) context.Context {
	if RequestEndpoint(ctx) != "" {
		return ctx
	}
	return context.WithValue(ctx, requestEndpointKey{}, EndpointForPath(path))
}

// RequestEndpoint returns the endpoint attached to a request context.
func RequestEndpoint(ctx context.Context) string {
	endpoint, _ := ctx.Value(requestEndpointKey{}).(string)
	return endpoint
}
