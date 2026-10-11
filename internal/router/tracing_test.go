package router

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/omniswitch-dev/omniswitch/internal/provider"
)

func TestProviderAttemptSpanSemanticConventions(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	previous := otel.GetTracerProvider()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(previous)
	defer tp.Shutdown(context.Background())

	_, span := startProviderSpan(context.Background(), "@openai-prod", "gpt-test", 2, true, true)
	finishProviderSpan(span, provider.ProviderMeta{Provider: "@openai-prod", ProviderType: "openai", Model: "gpt-resolved", Cost: 0.125}, errors.New("attempt failed"))
	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1", len(spans))
	}
	attempt := spans[0]
	if attempt.Name != "chat gpt-test" || attempt.InstrumentationScope.Name != "omniswitch/router" || attempt.SpanKind != oteltrace.SpanKindClient {
		t.Fatalf("attempt span = %+v", attempt)
	}
	attrs := make(map[attribute.Key]attribute.Value)
	for _, attr := range attempt.Attributes {
		if strings.HasPrefix(string(attr.Key), "sentinel.") || attr.Key == "llm.usage.cost_usd" {
			t.Errorf("legacy attribute %s is still emitted", attr.Key)
		}
		attrs[attr.Key] = attr.Value
	}
	for _, want := range []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.String("gen_ai.provider.name", "openai"),
		attribute.String("gen_ai.system", "openai"),
		attribute.String("gen_ai.request.model", "gpt-test"),
		attribute.String("gen_ai.response.model", "gpt-resolved"),
		attribute.Int("omniswitch.attempt", 2),
		attribute.Bool("omniswitch.fallback", true),
		attribute.Bool("llm.request.stream", true),
		attribute.Float64("omniswitch.usage.cost_usd", 0.125),
	} {
		if got := attrs[want.Key]; !reflect.DeepEqual(got, want.Value) {
			t.Errorf("%s = %v, want %v", want.Key, got, want.Value)
		}
	}
}
