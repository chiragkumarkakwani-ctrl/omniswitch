package router

import (
	"context"
	"strconv"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/provider"
)

type spanRecorderKey struct{}

// SpanRecorder records measured request stages without coupling the router to
// the HTTP gateway or to a persistence implementation.
type SpanRecorder func(name, providerName, status string, start time.Time, metadata map[string]any)

func WithSpanRecorder(ctx context.Context, recorder SpanRecorder) context.Context {
	return context.WithValue(ctx, spanRecorderKey{}, recorder)
}

func recordRequestSpan(ctx context.Context, name, providerName, status string, start time.Time, metadata map[string]any) {
	if recorder, ok := ctx.Value(spanRecorderKey{}).(SpanRecorder); ok && recorder != nil {
		recorder(name, providerName, status, start, metadata)
	}
}

func recordRoute(ctx context.Context, start time.Time, logicalModel, model string, route Route, providers []provider.Provider) {
	names := make([]string, 0, len(providers))
	for _, current := range providers {
		names = append(names, current.Name())
	}
	strategy := route.Strategy
	if strategy == "" {
		strategy = StrategyDirect
	}
	status := "ok"
	if len(names) == 0 {
		status = "error"
	}
	recordRequestSpan(ctx, "route", "", status, start, map[string]any{
		"model": logicalModel, "resolved_model": model, "strategy": strategy, "providers": names,
	})
}

func recordProviderAttempt(ctx context.Context, start time.Time, name, model string, attempt int, fallback, stream bool, err error) {
	metadata := map[string]any{"model": model, "attempt": attempt + 1, "fallback": fallback, "stream": stream}
	status := "ok"
	if err != nil {
		status = "error"
		metadata["error"] = err.Error()
		// Adapters currently expose upstream statuses in their error messages,
		// not ProviderMeta. Only retain a reported status; never guess one.
		if match := statusCodePattern.FindStringSubmatch(err.Error()); len(match) == 2 {
			if code, parseErr := strconv.Atoi(match[1]); parseErr == nil {
				metadata["http_status"] = code
			}
		}
	}
	recordRequestSpan(ctx, "provider_attempt", name, status, start, metadata)
}

func recordProviderFallback(ctx context.Context, start time.Time, from, to string, route Route, previousError error) {
	strategy := route.Strategy
	if strategy == "" {
		strategy = StrategyDirect
	}
	metadata := map[string]any{"from": from, "to": to, "strategy": strategy, "reason": "previous provider failed"}
	if previousError != nil {
		metadata["error"] = previousError.Error()
		if match := statusCodePattern.FindStringSubmatch(previousError.Error()); len(match) == 2 {
			if code, err := strconv.Atoi(match[1]); err == nil {
				metadata["http_status"] = code
			}
		}
	}
	recordRequestSpan(ctx, "guardrail_fallback", to, "ok", start, metadata)
}

func waitForRecordedRetry(ctx context.Context, providerName string, attempt int, delay time.Duration) error {
	start := time.Now()
	err := waitForRetry(ctx, delay)
	status := "ok"
	metadata := map[string]any{"attempt": attempt + 1}
	if err != nil {
		status = "error"
		metadata["error"] = err.Error()
	}
	recordRequestSpan(ctx, "retry_backoff", providerName, status, start, metadata)
	return err
}

// A successful streaming attempt ends when its channel closes, not when the
// upstream merely returns the channel. Record before closing the wrapper so
// the gateway cannot persist a log before the measured span is available.
func recordStreamAttempt(ctx context.Context, chunks <-chan provider.ChatResponseChunk, cancel context.CancelFunc, start time.Time, name, model string, attempt int, fallback bool) <-chan provider.ChatResponseChunk {
	out := make(chan provider.ChatResponseChunk)
	go func() {
		var streamErr error
		defer close(out)
		defer cancel()
		defer func() { recordProviderAttempt(ctx, start, name, model, attempt, fallback, true, streamErr) }()
		for {
			select {
			case <-ctx.Done():
				streamErr = ctx.Err()
				return
			case chunk, ok := <-chunks:
				if !ok {
					return
				}
				select {
				case <-ctx.Done():
					streamErr = ctx.Err()
					return
				case out <- chunk:
				}
			}
		}
	}()
	return out
}
