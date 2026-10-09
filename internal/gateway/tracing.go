package gateway

import (
	"sort"

	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/omniswitch-dev/omniswitch/internal/provider"
)

func setChatRequestSpan(span oteltrace.Span, req provider.ChatRequest) {
	span.SetName("chat " + firstNonEmpty(req.Model, "unknown"))
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.String("gen_ai.request.model", req.Model),
		attribute.String("llm.request.model", req.Model),
		attribute.Bool("llm.request.stream", req.Stream),
	}
	maxTokens := req.MaxTokens
	if req.MaxCompletionTokens != nil {
		maxTokens = req.MaxCompletionTokens
	}
	if maxTokens != nil {
		attrs = append(attrs, attribute.Int("gen_ai.request.max_tokens", *maxTokens))
	}
	if req.Temperature != nil {
		attrs = append(attrs, attribute.Float64("gen_ai.request.temperature", *req.Temperature))
	}
	if req.TopP != nil {
		attrs = append(attrs, attribute.Float64("gen_ai.request.top_p", *req.TopP))
	}
	span.SetAttributes(attrs...)
}

func setChatProviderSpan(span oteltrace.Span, meta provider.ProviderMeta) {
	span.SetAttributes(
		attribute.String("llm.response.provider", meta.Provider),
		attribute.Float64("omniswitch.usage.cost_usd", meta.Cost),
	)
	if name := firstNonEmpty(meta.ProviderType, meta.Provider); name != "" {
		span.SetAttributes(
			attribute.String("gen_ai.provider.name", name),
			// Retained for one release for older observability backends.
			attribute.String("gen_ai.system", name),
		)
	}
}

func setChatResponseSpan(span oteltrace.Span, resp provider.ChatResponse, meta provider.ProviderMeta) {
	setChatProviderSpan(span, meta)
	model := firstNonEmpty(resp.Model, meta.Model)
	finishReasons := make([]string, 0, len(resp.Choices))
	for _, choice := range resp.Choices {
		if choice.FinishReason != "" {
			finishReasons = append(finishReasons, choice.FinishReason)
		}
	}
	span.SetAttributes(
		attribute.String("gen_ai.response.model", model),
		attribute.String("gen_ai.response.id", resp.ID),
		attribute.StringSlice("gen_ai.response.finish_reasons", finishReasons),
		attribute.Int("gen_ai.usage.input_tokens", resp.Usage.PromptTokens),
		attribute.Int("gen_ai.usage.output_tokens", resp.Usage.CompletionTokens),
		attribute.String("llm.response.model", model),
		attribute.Int("llm.usage.prompt_tokens", resp.Usage.PromptTokens),
		attribute.Int("llm.usage.completion_tokens", resp.Usage.CompletionTokens),
		attribute.Int("llm.usage.total_tokens", resp.Usage.TotalTokens),
	)
}

func setStreamFinishReasons(span oteltrace.Span, reasons map[int]string) {
	indices := make([]int, 0, len(reasons))
	for index := range reasons {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	ordered := make([]string, 0, len(indices))
	for _, index := range indices {
		ordered = append(ordered, reasons[index])
	}
	span.SetAttributes(attribute.StringSlice("gen_ai.response.finish_reasons", ordered))
}
