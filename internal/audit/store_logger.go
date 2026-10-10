package audit

import (
	"context"
	"encoding/json"

	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/omniswitch-dev/omniswitch/internal/store"
)

type MultiLogger struct {
	loggers []Logger
}

func NewMultiLogger(loggers ...Logger) *MultiLogger {
	return &MultiLogger{loggers: loggers}
}

func (l *MultiLogger) Log(ctx context.Context, event Event) error {
	for _, logger := range l.loggers {
		if logger == nil {
			continue
		}
		if err := logger.Log(ctx, event); err != nil {
			return err
		}
	}
	return nil
}

type StoreLogger struct {
	store *store.Store
}

func NewStoreLogger(st *store.Store) *StoreLogger {
	return &StoreLogger{store: st}
}

func (l *StoreLogger) Log(ctx context.Context, event Event) error {
	if l == nil || l.store == nil {
		return nil
	}
	ctx = store.WithRequestEndpoint(ctx, "/mcp")
	status := "success"
	if event.Decision == "DENY" {
		status = "denied"
	}
	latencyMs := float64(event.Latency.Microseconds()) / 1000
	fields := make(map[string]any, len(event.Metadata)+1)
	for key, value := range event.Metadata {
		fields[key] = value
	}
	fields["mcp"] = map[string]any{
		"agent_id": event.AgentID, "action": event.Action, "resource": event.Resource,
		"rule": event.Rule, "target": event.Target, "latency_ms": latencyMs,
	}
	metadata, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	traceID, _ := ctx.Value(traceContextKey{}).(string)
	if traceID == "" {
		if span := oteltrace.SpanContextFromContext(ctx); span.IsValid() {
			traceID = span.TraceID().String()
		}
	}
	return l.store.InsertLog(ctx, store.RequestLog{
		ID: event.DecisionID, Timestamp: event.Timestamp,
		TraceID: traceID, SessionID: event.SessionID, Endpoint: store.RequestEndpoint(ctx),
		APIKeyID: event.AgentID, Provider: "mcp", Model: event.Tool,
		Status: status, Decision: event.Decision, DecisionReason: event.Reason,
		LatencyMs: latencyMs, Metadata: string(metadata),
	})
}
