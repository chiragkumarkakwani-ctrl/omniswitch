package audit

import "context"

type traceContextKey struct{}

// WithTraceID carries the gateway's explicit trace identifier into MCP audit
// records without introducing an audit-to-gateway import cycle.
func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceContextKey{}, id)
}
