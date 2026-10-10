package router

import (
	"context"

	"github.com/omniswitch-dev/omniswitch/internal/provider"
)

type preflightKey struct{}

// Preflight runs after variant/provider/model resolution but before upstream
// I/O. An error aborts the route, rather than trying a fallback that could
// bypass a gateway policy. Callers use this to reserve token and spend limits.
type Preflight func(context.Context, provider.Provider, string) error

func WithPreflight(ctx context.Context, preflight Preflight) context.Context {
	return context.WithValue(ctx, preflightKey{}, preflight)
}

func runPreflight(ctx context.Context, current provider.Provider, model string) error {
	if preflight, ok := ctx.Value(preflightKey{}).(Preflight); ok {
		return preflight(ctx, current, model)
	}
	return nil
}
