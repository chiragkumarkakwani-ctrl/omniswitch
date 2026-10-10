package gateway

import (
	"context"
	"net/http"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/provider"
	"github.com/omniswitch-dev/omniswitch/internal/telemetry"
)

type requestMetricsKey struct{}

type requestMetrics struct {
	registry *telemetry.Metrics
	endpoint string
	provider string
	model    string
}

// MetricsMiddleware includes authentication and rate-limit failures in request
// counters when mounted outside the middleware chain. Endpoint handlers also
// collect metrics themselves so embedders need not install this middleware.
func (h *Handler) MetricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w, r, finish := h.beginRequestMetrics(w, r)
		defer finish()
		next.ServeHTTP(w, r)
	})
}

// beginRequestMetrics wraps only the public inference APIs. The context marker
// lets Responses/Messages include translation failures without double-counting
// the ChatCompletions handler they call. SetMetrics is a startup-only setter.
func (h *Handler) beginRequestMetrics(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, *http.Request, func()) {
	endpoint := metricsEndpoint(r.URL.Path)
	if h.metrics == nil || endpoint == "" || metricsObservation(r.Context()) != nil {
		return w, r, func() {}
	}
	observation := &requestMetrics{registry: h.metrics, endpoint: endpoint}
	r = r.WithContext(context.WithValue(r.Context(), requestMetricsKey{}, observation))
	writer := &metricsResponseWriter{ResponseWriter: w}
	var wrapped http.ResponseWriter = writer
	if flusher, ok := w.(http.Flusher); ok {
		wrapped = &metricsFlushingResponseWriter{metricsResponseWriter: writer, flusher: flusher}
	}
	started := time.Now()
	return wrapped, r, func() {
		status := writer.status
		if status == 0 {
			status = http.StatusOK
		}
		observation.registry.RecordRequest(observation.endpoint, observation.provider, observation.model, metricsStatus(status), time.Since(started))
	}
}

func metricsEndpoint(path string) string {
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
	default:
		return ""
	}
}

func metricsStatus(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "limited"
	case status == http.StatusForbidden || status == http.StatusPaymentRequired:
		return "denied"
	case status >= 200 && status < 400:
		return "success"
	default:
		return "error"
	}
}

func metricsObservation(ctx context.Context) *requestMetrics {
	observation, _ := ctx.Value(requestMetricsKey{}).(*requestMetrics)
	return observation
}

func setRequestMetricModel(ctx context.Context, model string) {
	if observation := metricsObservation(ctx); observation != nil {
		observation.model = model
	}
}

func setRequestMetricResult(ctx context.Context, provider, model string) {
	if observation := metricsObservation(ctx); observation != nil {
		observation.provider = provider
		if model != "" {
			observation.model = model
		}
	}
}

// recordLogMetrics runs before any store checks: telemetry must work when an
// embedder disables persistence or a log write fails.
func recordLogMetrics(ctx context.Context, logCtx logContext) {
	observation := metricsObservation(ctx)
	if observation == nil {
		return
	}
	model := logCtx.Request.Model
	if logCtx.Meta != nil {
		if logCtx.Meta.Provider != "" {
			observation.provider = logCtx.Meta.Provider
		}
		model = firstNonEmpty(logCtx.Meta.Model, model)
	}
	if logCtx.Response != nil {
		model = firstNonEmpty(logCtx.Response.Model, model)
	}
	observation.model = firstNonEmpty(model, observation.model)
}

func recordProviderUsage(ctx context.Context, model string, usage provider.Usage, meta provider.ProviderMeta) {
	if observation := metricsObservation(ctx); observation != nil && !meta.Cached {
		observation.registry.RecordUsage(meta.Provider, firstNonEmpty(model, meta.Model, observation.model), usage.PromptTokens, usage.CompletionTokens, meta.Cost)
	}
}

type metricsResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *metricsResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *metricsResponseWriter) WriteHeader(status int) {
	if w.status == 0 && status >= 200 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *metricsResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(body)
}

type metricsFlushingResponseWriter struct {
	*metricsResponseWriter
	flusher http.Flusher
}

func (w *metricsFlushingResponseWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	w.flusher.Flush()
}
