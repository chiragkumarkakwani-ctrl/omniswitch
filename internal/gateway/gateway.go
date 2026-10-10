package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/omniswitch-dev/omniswitch/internal/cache"
	"github.com/omniswitch-dev/omniswitch/internal/gatewayconfig"
	"github.com/omniswitch-dev/omniswitch/internal/guardrail"
	"github.com/omniswitch-dev/omniswitch/internal/provider"
	"github.com/omniswitch-dev/omniswitch/internal/router"
	"github.com/omniswitch-dev/omniswitch/internal/store"
	"github.com/omniswitch-dev/omniswitch/internal/telemetry"
)

// Handler serves the OpenAI-compatible /v1/chat/completions API.
type Handler struct {
	registry              *provider.Registry
	router                *router.Router
	store                 *store.Store
	metrics               *telemetry.Metrics
	guardrails            atomic.Pointer[guardrail.Engine]
	cacheThreshold        float64
	cacheTTL              time.Duration
	cacheScope            string
	logPayloads           atomic.Bool
	streamGuardrailBuffer bool
	maxRequestBytes       int64
	maxUploadBytes        atomic.Int64
	shadowProvider        string
	configRouters         sync.Map
	alerts                *AlertManager
}

type cachedConfigRouter struct {
	router    *router.Router
	updatedAt time.Time
}

// New creates a new gateway handler.
func New(registry *provider.Registry, rtr *router.Router, st *store.Store, gr *guardrail.Engine) *Handler {
	h := &Handler{
		registry:              registry,
		router:                rtr,
		store:                 st,
		metrics:               telemetry.DefaultMetrics,
		cacheThreshold:        0.95,
		cacheTTL:              24 * time.Hour,
		cacheScope:            "api_key",
		streamGuardrailBuffer: true,
		maxRequestBytes:       10 << 20,
	}
	// Preserve the previous package-level behavior for embedders. The gateway
	// binary explicitly disables this unless configured otherwise.
	h.SetLogPayloads(true)
	h.maxUploadBytes.Store(defaultMaxUploadBytes)
	if gr != nil {
		h.guardrails.Store(gr)
	}
	return h
}

// SetGuardrails atomically swaps the guardrail engine; in-flight requests keep
// using their loaded engine until completion. Used by config hot-reload.
func (h *Handler) SetGuardrails(engine *guardrail.Engine) {
	if engine == nil {
		h.guardrails.Store(&guardrail.Engine{})
		return
	}
	h.guardrails.Store(engine)
}

func (h *Handler) SetSemanticCache(threshold float64) {
	h.cacheThreshold = threshold
}

func (h *Handler) SetCacheTTL(ttl time.Duration) {
	h.cacheTTL = ttl
}

func (h *Handler) SetCacheScope(scope string) {
	switch strings.TrimSpace(scope) {
	case "workspace", "organization", "global", "api_key":
		h.cacheScope = scope
	default:
		h.cacheScope = "api_key"
	}
}

func (h *Handler) SetLogPayloads(enabled bool) {
	h.logPayloads.Store(enabled)
}

// LogPayloads reports the live payload-logging setting, including hot reloads.
func (h *Handler) LogPayloads() bool {
	return h.logPayloads.Load()
}

// SetStreamGuardrailBuffer prevents output from being emitted before output
// guardrails have inspected it. Disabling it trades protection for lower
// first-token latency and is therefore only appropriate for trusted traffic.
func (h *Handler) SetStreamGuardrailBuffer(enabled bool) {
	h.streamGuardrailBuffer = enabled
}

func (h *Handler) SetMaxRequestBytes(limit int64) {
	if limit > 0 {
		h.maxRequestBytes = limit
	}
}

// SetMaxUploadBytes sets the multipart transcription body limit. Atomic storage
// lets config reloads safely update it while uploads are in flight.
func (h *Handler) SetMaxUploadBytes(limit int64) {
	if limit > 0 {
		h.maxUploadBytes.Store(limit)
	}
}

func (h *Handler) SetShadowProvider(providerName string) {
	h.shadowProvider = providerName
}

func (h *Handler) SetAlerts(config AlertConfig) {
	if len(config.Thresholds) == 0 && config.WebhookURL == "" && config.SlackURL == "" {
		h.alerts = nil
		return
	}
	h.alerts = NewAlertManager(config)
}

// SetMetrics replaces the process-wide registry for this handler. Call it
// before serving requests. A nil registry disables collection on this handler.
func (h *Handler) SetMetrics(metrics *telemetry.Metrics) {
	h.metrics = metrics
}

// ChatCompletions handles POST /v1/chat/completions.
func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	w, r, finishMetrics := h.beginRequestMetrics(w, r)
	defer finishMetrics()
	ctx, span := otel.Tracer("omniswitch/gateway").Start(
		store.WithRequestEndpoint(r.Context(), r.URL.Path),
		"chat unknown",
		oteltrace.WithSpanKind(oteltrace.SpanKindServer),
		oteltrace.WithAttributes(
			attribute.String("http.request.method", r.Method),
			attribute.String("http.route", r.URL.Path),
			attribute.String("gen_ai.operation.name", "chat"),
		),
	)
	defer span.End()
	r = r.WithContext(ctx)

	if r.Method != http.MethodPost {
		span.SetStatus(codes.Error, "method not allowed")
		writeError(w, http.StatusMethodNotAllowed, "only POST is supported")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.maxRequestBytes)
	var req provider.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	setRequestMetricModel(ctx, req.Model)
	span.SetName("chat " + firstNonEmpty(req.Model, "unknown"))
	span.SetAttributes(attribute.String("gen_ai.request.model", req.Model))
	activeRouter := h.router
	configRef := strings.TrimSpace(r.Header.Get("x-omniswitch-config"))
	if configRef != "" {
		configRouter, err := h.routerForConfig(ctx, configRef)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "invalid dynamic config")
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		activeRouter = configRouter
		span.SetAttributes(attribute.String("omniswitch.config", configRef))
	}
	transformed, err := activeRouter.TransformRequest(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid route transformation")
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req = transformed
	setChatRequestSpan(span, req)
	setRequestMetricModel(ctx, req.Model)

	reqID := newRequestID()
	traceID := requestHeaderOrNew(r, "x-omniswitch-trace-id", "trace")
	sessionID := r.Header.Get("x-omniswitch-session-id")
	userID := r.Header.Get("x-omniswitch-user-id")
	providerHint := r.Header.Get("x-omniswitch-provider")
	shadowHint := firstNonEmpty(r.Header.Get("x-omniswitch-shadow-provider"), activeRouter.ShadowProviderForModel(req.Model), h.shadowProvider)
	apiKeyID := r.Header.Get("x-omniswitch-key-id")
	tenantCacheScope := h.tenantCacheScope(r)
	traceRecorder := newTraceRecorder()
	ctx = WithRequestID(r.Context(), reqID)
	ctx = WithTraceID(ctx, traceID)
	ctx = WithSessionID(ctx, sessionID)
	ctx = WithUserID(ctx, userID)
	ctx = WithMetadataJSON(ctx, r.Header.Get("x-omniswitch-metadata"))
	ctx = WithTags(ctx, tagsHeader(r.Header.Get("x-omniswitch-tags")))
	ctx = WithTraceRecorder(ctx, traceRecorder)
	span.SetAttributes(
		attribute.String("omniswitch.request_id", reqID),
		attribute.String("omniswitch.trace_id", traceID),
		attribute.String("omniswitch.session_id", sessionID),
		attribute.String("omniswitch.user_id", userID),
	)

	stageStart := time.Now()
	if denied, reason := h.budgetExceeded(ctx, apiKeyID); denied {
		recordTraceSpan(ctx, "budget", "", "denied", stageStart, map[string]any{"reason": reason})
		span.SetAttributes(
			attribute.String("omniswitch.decision", "DENY"),
			attribute.String("omniswitch.decision_reason", reason),
		)
		h.logRequest(ctx, logContext{
			ID: reqID, TraceID: traceID, SessionID: sessionID, Request: req, APIKeyID: apiKeyID,
			Status: "denied", ErrorMessage: reason,
		})
		w.Header().Set("x-omniswitch-trace-id", traceID)
		w.Header().Set("x-omniswitch-session-id", sessionID)
		writeJSON(w, http.StatusPaymentRequired, map[string]any{
			"error": map[string]string{"message": reason, "type": "budget_exceeded", "code": "budget_exceeded"},
		})
		return
	}
	recordTraceSpan(ctx, "budget", "", "ok", stageStart, nil)

	// Input guardrails.
	if gr := h.guardrails.Load(); gr != nil {
		stageStart = time.Now()
		results := gr.EvaluateInputContext(ctx, req.Messages)
		h.recordGuardrailResults(ctx, reqID, results)
		var rerouteProvider string
		var fallbackTarget string
		for _, gr := range results {
			if gr.Action == "deny" {
				recordTraceSpan(ctx, "guardrail_input", "", "denied", stageStart, map[string]any{"type": gr.Type, "message": gr.Message})
				span.SetAttributes(
					attribute.String("omniswitch.decision", "DENY"),
					attribute.String("omniswitch.guardrail.type", gr.Type),
					attribute.String("omniswitch.decision_reason", gr.Message),
				)
				h.logRequest(ctx, logContext{
					ID: reqID, TraceID: traceID, SessionID: sessionID, Request: req, APIKeyID: apiKeyID,
					Status: "denied", ErrorMessage: gr.Message,
				})
				w.Header().Set("x-omniswitch-trace-id", traceID)
				w.Header().Set("x-omniswitch-session-id", sessionID)
				writeJSON(w, http.StatusForbidden, map[string]any{
					"error": map[string]string{"message": gr.Message, "type": gr.Type, "code": "guardrail_triggered"},
				})
				return
			}
			if gr.Action == "reroute" && rerouteProvider == "" && gr.RerouteProvider != "" {
				rerouteProvider = gr.RerouteProvider
			}
			if gr.Action == "fallback" && fallbackTarget == "" && gr.Fallback != "" {
				fallbackTarget = gr.Fallback
			}
		}
		if rerouteProvider != "" {
			providerHint = rerouteProvider
			recordTraceSpan(ctx, "guardrail_reroute", providerHint, "rerouted", stageStart, map[string]any{"target": rerouteProvider})
		}
		if fallbackTarget != "" {
			providerName, modelID := parseFallbackTarget(fallbackTarget)
			if providerName != "" {
				providerHint = providerName
			}
			if modelID != "" {
				req.Model = modelID
			}
			recordTraceSpan(ctx, "guardrail_fallback", providerHint, "rerouted", stageStart,
				map[string]any{"target": fallbackTarget})
			span.SetAttributes(
				attribute.String("omniswitch.guardrail.fallback_target", fallbackTarget),
			)
		}
		recordTraceSpan(ctx, "guardrail_input", "", "ok", stageStart, nil)
	}

	backendReq := req
	backendReq.Stream = false
	cacheProvider := firstNonEmpty(providerHint, "auto")
	if configRef != "" {
		cacheProvider = "config:" + configRef + "|" + cacheProvider
	}
	cacheProvider = compatibleCacheProvider(ctx, cacheProvider)
	stageStart = time.Now()
	cachedResp, cacheHit := h.cachedResponse(ctx, tenantCacheScope, cacheProvider, backendReq)
	recordTraceSpan(ctx, "cache_lookup", cacheProvider, map[bool]string{true: "hit", false: "miss"}[cacheHit], stageStart, nil)
	if cacheHit {
		meta := provider.ProviderMeta{Provider: "semantic-cache", Model: req.Model, Cached: true, Timestamp: time.Now().UTC()}
		setChatResponseSpan(span, cachedResp, meta)
		setRequestMetricResult(ctx, meta.Provider, cachedResp.Model)
		span.SetAttributes(attribute.Bool("omniswitch.cache_hit", true))
		disp := h.outputGuardrailDisposition(ctx, reqID, &cachedResp)
		if disp.Denied {
			span.SetAttributes(attribute.String("omniswitch.decision", "DENY"), attribute.String("omniswitch.decision_reason", disp.Reason))
			h.logRequest(ctx, logContext{
				ID: reqID, TraceID: traceID, SessionID: sessionID, Request: req, Response: &cachedResp,
				APIKeyID: apiKeyID, Status: "denied", ErrorMessage: disp.Reason, Cached: true,
			})
			w.Header().Set("x-omniswitch-trace-id", traceID)
			w.Header().Set("x-omniswitch-session-id", sessionID)
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": map[string]string{"message": disp.Reason, "type": "guardrail", "code": "guardrail_triggered"},
			})
			return
		}

		if !h.enforceCompatibleResponse(w, ctx, logContext{
			ID: reqID, TraceID: traceID, SessionID: sessionID, Request: req, Response: &cachedResp, APIKeyID: apiKeyID, Cached: true,
		}) {
			return
		}
		span.SetStatus(codes.Ok, "")
		span.SetAttributes(
			attribute.String("omniswitch.decision", "ALLOW"),
			attribute.Bool("omniswitch.cache_hit", true),
			attribute.String("llm.response.provider", meta.Provider),
		)
		h.logRequest(ctx, logContext{
			ID: reqID, TraceID: traceID, SessionID: sessionID, Request: req, Response: &cachedResp, Meta: &meta,
			APIKeyID: apiKeyID, Status: "success", Cached: true,
		})
		w.Header().Set("x-omniswitch-trace-id", traceID)
		w.Header().Set("x-omniswitch-session-id", sessionID)
		w.Header().Set("x-omniswitch-cache", "HIT")
		if req.Stream {
			writeStream(w, cachedResp)
			return
		}
		writeJSON(w, http.StatusOK, cachedResp)
		return
	}

	if req.Stream {
		h.streamProviderResponse(w, ctx, streamContext{
			ID: reqID, TraceID: traceID, SessionID: sessionID, Request: req, BackendRequest: backendReq,
			Router: activeRouter, ProviderHint: providerHint, ShadowHint: shadowHint, APIKeyID: apiKeyID, CacheProvider: cacheProvider, CacheScope: tenantCacheScope,
		})
		return
	}

	var resp provider.ChatResponse
	var meta provider.ProviderMeta

	maxAttempts := 1
	for attempt := 0; attempt < maxAttempts; attempt++ {
		stageStart = time.Now()
		resp, meta, err = activeRouter.Execute(ctx, backendReq, providerHint)
		if err != nil {
			recordTraceSpan(ctx, "provider_call", meta.Provider, "error", stageStart, map[string]any{"error": err.Error()})
			span.RecordError(err)
			span.SetStatus(codes.Error, "provider error")
			setChatProviderSpan(span, meta)
			h.logRequest(ctx, logContext{
				ID: reqID, TraceID: traceID, SessionID: sessionID, Request: req, Meta: &meta,
				APIKeyID: apiKeyID, Status: "error", ErrorMessage: err.Error(),
			})
			w.Header().Set("x-omniswitch-trace-id", traceID)
			w.Header().Set("x-omniswitch-session-id", sessionID)
			if upErr, ok := asUnsupportedParameterError(err); ok {
				writeUnsupportedParameterError(w, upErr)
				return
			}
			writeError(w, http.StatusBadGateway, "provider error: "+err.Error())
			return
		}
		recordTraceSpan(ctx, "provider_call", meta.Provider, "ok", stageStart, nil)
		setChatResponseSpan(span, resp, meta)
		recordProviderUsage(ctx, resp.Model, resp.Usage, meta)

		// Output guardrails inspect every completion choice.
		disp := h.outputGuardrailDisposition(ctx, reqID, &resp)
		if disp.Denied {
			span.SetAttributes(
				attribute.String("omniswitch.decision", "DENY"),
				attribute.String("omniswitch.decision_reason", disp.Reason),
				attribute.String("llm.response.provider", meta.Provider),
			)
			h.logRequest(ctx, logContext{
				ID: reqID, TraceID: traceID, SessionID: sessionID, Request: req, Response: &resp, Meta: &meta,
				APIKeyID: apiKeyID, Status: "denied", ErrorMessage: disp.Reason,
			})
			w.Header().Set("x-omniswitch-trace-id", traceID)
			w.Header().Set("x-omniswitch-session-id", sessionID)
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": map[string]string{"message": disp.Reason, "type": "guardrail", "code": "guardrail_triggered"},
			})
			return
		}

		if disp.Retry || disp.RerouteProvider != "" || disp.Fallback != "" {
			if disp.MaxRetries > 0 && maxAttempts == 1 {
				maxAttempts = 1 + disp.MaxRetries
			} else if maxAttempts == 1 {
				maxAttempts = 2 // default 1 retry
			}

			if attempt+1 < maxAttempts {
				if disp.RerouteProvider != "" {
					providerHint = disp.RerouteProvider
				}
				if disp.Fallback != "" {
					if pn, mod := parseFallbackTarget(disp.Fallback); pn != "" || mod != "" {
						if pn != "" {
							providerHint = pn
						}
						if mod != "" {
							backendReq.Model = mod
						}
					}
				}
				continue
			}
		}

		break
	}

	if !h.enforceCompatibleResponse(w, ctx, logContext{
		ID: reqID, TraceID: traceID, SessionID: sessionID, Request: req, Response: &resp, Meta: &meta, APIKeyID: apiKeyID,
	}) {
		return
	}

	h.storeCache(ctx, tenantCacheScope, cacheProvider, backendReq, resp)
	h.logRequest(ctx, logContext{
		ID: reqID, TraceID: traceID, SessionID: sessionID, Request: req, Response: &resp, Meta: &meta,
		APIKeyID: apiKeyID, Status: "success", Cached: meta.Cached,
	})
	span.SetStatus(codes.Ok, "")
	span.SetAttributes(
		attribute.String("omniswitch.decision", "ALLOW"),
		attribute.Bool("omniswitch.cache_hit", meta.Cached),
		attribute.String("llm.response.provider", meta.Provider),
	)
	h.shadow(ctx, activeRouter, reqID, traceID, meta.Provider, backendReq, shadowHint)

	w.Header().Set("x-omniswitch-trace-id", traceID)
	w.Header().Set("x-omniswitch-session-id", sessionID)
	w.Header().Set("x-omniswitch-cache", "MISS")
	if req.Stream {
		writeStream(w, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

type streamContext struct {
	ID             string
	TraceID        string
	SessionID      string
	Request        provider.ChatRequest
	BackendRequest provider.ChatRequest
	Router         *router.Router
	ProviderHint   string
	ShadowHint     string
	APIKeyID       string
	CacheProvider  string
	CacheScope     string
}

// ListModels handles GET /v1/models.
func (h *Handler) ListModels(w http.ResponseWriter, r *http.Request) {
	models := h.registry.AllModels()
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": models})
}

// parseFallbackTarget splits a "provider/model" reroute target. Either half
// may be empty; the caller applies only the halves present.
func parseFallbackTarget(target string) (providerName, modelID string) {
	name, model, ok := strings.Cut(target, "/")
	if !ok {
		return strings.TrimPrefix(target, "@"), ""
	}
	return strings.TrimPrefix(name, "@"), model
}

type logContext struct {
	ID           string
	TraceID      string
	SessionID    string
	Request      provider.ChatRequest
	Response     *provider.ChatResponse
	Meta         *provider.ProviderMeta
	APIKeyID     string
	Status       string
	ErrorMessage string
	Cached       bool
}

func (h *Handler) logRequest(ctx context.Context, logCtx logContext) {
	recordLogMetrics(ctx, logCtx)
	if h.store == nil {
		return
	}

	entry := store.RequestLog{
		ID:         logCtx.ID,
		Timestamp:  time.Now().UTC(),
		TraceID:    logCtx.TraceID,
		SessionID:  logCtx.SessionID,
		Endpoint:   store.RequestEndpoint(ctx),
		CostSource: "none",
		Model:      logCtx.Request.Model,
		APIKeyID:   logCtx.APIKeyID,
		Status:     logCtx.Status,
		Decision:   "ALLOW",
		Cached:     logCtx.Cached,
	}
	if h.LogPayloads() {
		entry.RequestBody = marshalLogBody(logCtx.Request)
	}

	if logCtx.Status == "denied" {
		entry.Decision = "DENY"
		entry.DecisionReason = logCtx.ErrorMessage
	}
	if logCtx.Status == "error" {
		entry.ErrorMessage = logCtx.ErrorMessage
	}

	if logCtx.Meta != nil {
		entry.Provider = logCtx.Meta.Provider
		if logCtx.Meta.Model != "" {
			entry.Model = logCtx.Meta.Model
		}
		entry.LatencyMs = float64(logCtx.Meta.Latency.Microseconds()) / 1000
		entry.Cost = logCtx.Meta.Cost
	}

	if logCtx.Response != nil {
		entry.InputTokens = logCtx.Response.Usage.PromptTokens
		entry.OutputTokens = logCtx.Response.Usage.CompletionTokens
		entry.TotalTokens = logCtx.Response.Usage.TotalTokens
		if h.LogPayloads() {
			entry.ResponseBody = marshalLogBody(logCtx.Response)
		}
	}

	if spans := spansFromContext(ctx); len(spans) > 0 {
		entry.Metadata = metadataWithSpans(MetadataJSONFromContext(ctx), spans)
	} else if meta := MetadataJSONFromContext(ctx); meta != "{}" {
		entry.Metadata = meta
	}

	if err := h.store.InsertLog(ctx, entry); err != nil {
		log.Printf("failed to log request: %v", err)
	}
	if entry.APIKeyID != "" && entry.Status == "success" && entry.Cost > 0 && !entry.Cached {
		if err := h.store.IncrementAPIKeySpend(ctx, entry.APIKeyID, entry.Cost); err != nil {
			log.Printf("failed to increment API key spend: %v", err)
		}
	}
}

func (h *Handler) routerForConfig(ctx context.Context, ref string) (*router.Router, error) {
	if h.store == nil {
		return nil, fmt.Errorf("dynamic config %q requested but no store is configured", ref)
	}
	record, err := h.store.GetGatewayConfig(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("dynamic config %q not found", ref)
	}
	if cached, ok := h.configRouters.Load(record.Name); ok {
		value := cached.(cachedConfigRouter)
		if value.updatedAt.Equal(record.UpdatedAt) {
			return value.router, nil
		}
	}
	cfg, err := gatewayconfig.Parse([]byte(record.Body), "."+record.Format)
	if err != nil {
		return nil, fmt.Errorf("dynamic config %q is invalid: %w", record.Name, err)
	}
	configRouter := h.router.CloneWithRoutes(cfg.Routes)
	h.configRouters.Store(record.Name, cachedConfigRouter{router: configRouter, updatedAt: record.UpdatedAt})
	if record.ID != record.Name {
		h.configRouters.Store(record.ID, cachedConfigRouter{router: configRouter, updatedAt: record.UpdatedAt})
	}
	return configRouter, nil
}

func (h *Handler) streamProviderResponse(w http.ResponseWriter, ctx context.Context, streamCtx streamContext) {
	span := oteltrace.SpanFromContext(ctx)
	activeRouter := streamCtx.Router
	if activeRouter == nil {
		activeRouter = h.router
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported by this server")
		return
	}
	bufferOutput := h.streamGuardrailBuffer && h.guardrails.Load() != nil

	maxAttempts := 1
	var aggregated provider.ChatResponse
	var meta provider.ProviderMeta

	for attempt := 0; attempt < maxAttempts; attempt++ {
		var err error
		var chunks <-chan provider.ChatResponseChunk
		chunks, meta, err = activeRouter.ExecuteStream(ctx, streamCtx.BackendRequest, streamCtx.ProviderHint)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "provider stream error")
			setChatProviderSpan(span, meta)
			h.logRequest(ctx, logContext{
				ID: streamCtx.ID, TraceID: streamCtx.TraceID, SessionID: streamCtx.SessionID, Request: streamCtx.Request, Meta: &meta,
				APIKeyID: streamCtx.APIKeyID, Status: "error", ErrorMessage: err.Error(),
			})
			w.Header().Set("x-omniswitch-trace-id", streamCtx.TraceID)
			w.Header().Set("x-omniswitch-session-id", streamCtx.SessionID)
			if upErr, ok := asUnsupportedParameterError(err); ok {
				writeUnsupportedParameterError(w, upErr)
				return
			}
			writeError(w, http.StatusBadGateway, "provider stream error: "+err.Error())
			return
		}

		if !bufferOutput {
			setStreamHeaders(w, streamCtx.TraceID, streamCtx.SessionID)
			w.WriteHeader(http.StatusOK)
		}

		aggregated = provider.ChatResponse{
			ID:      newID("chat"),
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   firstNonEmpty(meta.Model, streamCtx.BackendRequest.Model),
			Choices: []provider.Choice{{
				Index:        0,
				Message:      provider.Message{Role: "assistant"},
				FinishReason: "stop",
			}},
		}
		var buffered []provider.ChatResponseChunk
		finishReasons := make(map[int]string)
		for chunk := range chunks {
			if chunk.ID != "" {
				aggregated.ID = chunk.ID
			}
			if chunk.Model != "" {
				aggregated.Model = chunk.Model
			}
			for _, choice := range chunk.Choices {
				if choice.FinishReason != "" {
					finishReasons[choice.Index] = choice.FinishReason
				}
				if choice.Index == 0 {
					if choice.Delta.Role != "" {
						aggregated.Choices[0].Message.Role = choice.Delta.Role
					}
					aggregated.Choices[0].Message.Content += choice.Delta.Content
					if len(choice.Delta.ToolCalls) > 0 {
						mergeToolCallDeltas(&aggregated.Choices[0].Message.ToolCalls, choice.Delta.ToolCalls)
					}
					if choice.FinishReason != "" {
						aggregated.Choices[0].FinishReason = choice.FinishReason
					}
				}
			}
			if chunk.Usage != nil {
				aggregated.Usage = *chunk.Usage
				meta.Cost = provider.EstimateCost(firstNonEmpty(meta.ProviderType, meta.Provider), aggregated.Model, aggregated.Usage)
			}
			if bufferOutput {
				buffered = append(buffered, chunk)
			} else {
				writeSSE(w, chunk)
				flusher.Flush()
			}
		}

		setChatResponseSpan(span, aggregated, meta)
		recordProviderUsage(ctx, aggregated.Model, aggregated.Usage, meta)
		setStreamFinishReasons(span, finishReasons)
		if bufferOutput {
			disp := h.outputGuardrailDisposition(ctx, streamCtx.ID, &aggregated)
			if disp.Denied {
				span.SetStatus(codes.Error, "stream output guardrail denied")
				span.SetAttributes(attribute.String("omniswitch.decision", "DENY"), attribute.String("omniswitch.decision_reason", disp.Reason))
				h.logRequest(ctx, logContext{
					ID: streamCtx.ID, TraceID: streamCtx.TraceID, SessionID: streamCtx.SessionID, Request: streamCtx.Request, Response: &aggregated, Meta: &meta,
					APIKeyID: streamCtx.APIKeyID, Status: "denied", ErrorMessage: disp.Reason,
				})
				w.Header().Set("x-omniswitch-trace-id", streamCtx.TraceID)
				w.Header().Set("x-omniswitch-session-id", streamCtx.SessionID)
				writeJSON(w, http.StatusForbidden, map[string]any{
					"error": map[string]string{"message": disp.Reason, "type": "guardrail", "code": "guardrail_triggered"},
				})
				return
			}

			if disp.Retry || disp.RerouteProvider != "" || disp.Fallback != "" {
				if disp.MaxRetries > 0 && maxAttempts == 1 {
					maxAttempts = 1 + disp.MaxRetries
				} else if maxAttempts == 1 {
					maxAttempts = 2 // default 1 retry
				}

				if attempt+1 < maxAttempts {
					if disp.RerouteProvider != "" {
						streamCtx.ProviderHint = disp.RerouteProvider
					}
					if disp.Fallback != "" {
						if pn, mod := parseFallbackTarget(disp.Fallback); pn != "" || mod != "" {
							if pn != "" {
								streamCtx.ProviderHint = pn
							}
							if mod != "" {
								streamCtx.BackendRequest.Model = mod
							}
						}
					}
					continue
				}
			}

			if !h.enforceCompatibleResponse(w, ctx, logContext{
				ID: streamCtx.ID, TraceID: streamCtx.TraceID, SessionID: streamCtx.SessionID, Request: streamCtx.Request, Response: &aggregated, Meta: &meta, APIKeyID: streamCtx.APIKeyID,
			}) {
				return
			}

			setStreamHeaders(w, streamCtx.TraceID, streamCtx.SessionID)
			if disp.Redacted {
				writeStream(w, aggregated)
			} else {
				w.WriteHeader(http.StatusOK)
				for _, chunk := range buffered {
					writeSSE(w, chunk)
					flusher.Flush()
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
				flusher.Flush()
			}
		} else {
			if err := validateCompatibleResponse(ctx, aggregated); err != nil {
				h.logRequest(ctx, logContext{
					ID: streamCtx.ID, TraceID: streamCtx.TraceID, SessionID: streamCtx.SessionID, Request: streamCtx.Request, Response: &aggregated, Meta: &meta, APIKeyID: streamCtx.APIKeyID,
					Status: "error", ErrorMessage: err.Error(),
				})
				span.RecordError(err)
				span.SetStatus(codes.Error, "compatibility stream error")
				// The translator turns completion into api_error on an already-open
				// stream. HTTP status is immutable here; never cache bad arguments.
				_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
		}

		break
	}

	h.storeCache(ctx, streamCtx.CacheScope, streamCtx.CacheProvider, streamCtx.BackendRequest, aggregated)
	h.logRequest(ctx, logContext{
		ID: streamCtx.ID, TraceID: streamCtx.TraceID, SessionID: streamCtx.SessionID, Request: streamCtx.Request, Response: &aggregated, Meta: &meta,
		APIKeyID: streamCtx.APIKeyID, Status: "success",
	})
	span.SetStatus(codes.Ok, "")
	span.SetAttributes(
		attribute.String("omniswitch.decision", "ALLOW"),
		attribute.String("llm.response.provider", meta.Provider),
		attribute.Bool("omniswitch.cache_hit", meta.Cached),
	)
	h.shadow(ctx, activeRouter, streamCtx.ID, streamCtx.TraceID, meta.Provider, streamCtx.BackendRequest, streamCtx.ShadowHint)
}

func setStreamHeaders(w http.ResponseWriter, traceID, sessionID string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("x-omniswitch-trace-id", traceID)
	w.Header().Set("x-omniswitch-session-id", sessionID)
	w.Header().Set("x-omniswitch-cache", "MISS")
}

type guardrailDisposition struct {
	Denied          bool
	Redacted        bool
	Retry           bool
	MaxRetries      int
	RerouteProvider string
	Fallback        string
	Reason          string
}

// outputGuardrailDisposition evaluates every generated choice. A redaction
// replaces the affected output before it is logged, cached, or returned.
func (h *Handler) outputGuardrailDisposition(ctx context.Context, requestID string, resp *provider.ChatResponse) guardrailDisposition {
	gr := h.guardrails.Load()
	if gr == nil {
		return guardrailDisposition{}
	}
	var disp guardrailDisposition
	for index := range resp.Choices {
		results := gr.EvaluateOutputMessageContext(ctx, resp.Choices[index].Message)
		h.recordGuardrailResults(ctx, requestID, results)
		for _, result := range results {
			if !result.Triggered {
				continue
			}
			switch result.Action {
			case "deny":
				disp.Denied = true
				if disp.Reason == "" {
					disp.Reason = result.Message
				}
			case "redact":
				resp.Choices[index].Message.Content = "[REDACTED BY OMNISWITCH]"
				disp.Redacted = true
			case "retry":
				disp.Retry = true
				if result.MaxRetries > disp.MaxRetries {
					disp.MaxRetries = result.MaxRetries
				}
			case "reroute":
				if disp.RerouteProvider == "" && result.RerouteProvider != "" {
					disp.RerouteProvider = result.RerouteProvider
				}
			case "fallback":
				if disp.Fallback == "" && result.Fallback != "" {
					disp.Fallback = result.Fallback
				}
			}
		}
	}
	return disp
}

// recordGuardrailResults keeps a structured audit trail for both enforced and
// non-blocking results without persisting the prompt or completion itself.
func (h *Handler) recordGuardrailResults(ctx context.Context, requestID string, results []guardrail.Result) {
	for _, result := range results {
		if !result.Triggered {
			continue
		}
		h.metrics.RecordGuardrailEvent(result.Type, result.Action)
		if h.store == nil || requestID == "" {
			continue
		}
		if err := h.store.InsertGuardrailEvent(ctx, store.GuardrailEvent{
			ID:        newID("gr"),
			RequestID: requestID,
			Timestamp: time.Now().UTC(),
			Type:      result.Type,
			Action:    result.Action,
			Matched:   true,
			Details:   result.Details,
		}); err != nil {
			log.Printf("failed to store guardrail event: %v", err)
		}
	}
}

func (h *Handler) budgetExceeded(ctx context.Context, apiKeyID string) (bool, string) {
	if h.store == nil || apiKeyID == "" {
		return false, ""
	}
	apiKey, err := h.store.GetAPIKeyByID(ctx, apiKeyID)
	if err != nil {
		return false, ""
	}
	costBudget := apiKey.BudgetUSD
	if costBudget <= 0 {
		costBudget = apiKey.MonthlyCostBudget
	}
	if costBudget <= 0 && apiKey.MonthlyTokenBudget <= 0 {
		return false, ""
	}
	now := time.Now().UTC()
	since := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	usage, err := h.store.GetBudgetUsage(ctx, apiKeyID, since)
	if err != nil {
		log.Printf("failed to check budget: %v", err)
		return false, ""
	}
	spendUSD := apiKey.SpendUSD
	if usage.Cost > spendUSD {
		spendUSD = usage.Cost
	}
	if costBudget > 0 && spendUSD >= costBudget {
		return true, fmt.Sprintf("monthly cost budget exceeded: %.4f of %.4f used", spendUSD, costBudget)
	}
	if apiKey.MonthlyTokenBudget > 0 && usage.Tokens >= apiKey.MonthlyTokenBudget {
		return true, fmt.Sprintf("monthly token budget exceeded: %d of %d used", usage.Tokens, apiKey.MonthlyTokenBudget)
	}
	return false, ""
}

func (h *Handler) cachedResponse(ctx context.Context, tenantScope, providerName string, req provider.ChatRequest) (provider.ChatResponse, bool) {
	if h.store == nil || h.cacheThreshold <= 0 || len(req.Messages) == 0 {
		return provider.ChatResponse{}, false
	}
	key, err := cache.KeyWithScope(tenantScope, providerName, req)
	if err != nil {
		log.Printf("exact cache key failed: %v", err)
	} else if entry, ok, err := h.store.GetExactCache(ctx, key); err != nil {
		log.Printf("exact cache lookup failed: %v", err)
	} else if ok {
		return entry.Response, true
	}

	vector := cache.Vectorize(cache.PromptText(req.Messages))
	entry, ok, err := h.store.FindSemanticCache(ctx, semanticCacheScope(tenantScope, providerName, req), vector, h.cacheThreshold)
	if err != nil {
		log.Printf("semantic cache lookup failed: %v", err)
		return provider.ChatResponse{}, false
	}
	if !ok {
		return provider.ChatResponse{}, false
	}
	return entry.Response, true
}

func (h *Handler) storeCache(ctx context.Context, tenantScope, providerName string, req provider.ChatRequest, resp provider.ChatResponse) {
	if h.store == nil || h.cacheThreshold <= 0 || len(req.Messages) == 0 || len(resp.Choices) == 0 {
		return
	}
	key, err := cache.KeyWithScope(tenantScope, providerName, req)
	if err != nil {
		log.Printf("exact cache key failed: %v", err)
	}
	promptText := cache.PromptText(req.Messages)
	var expiresAt *time.Time
	if h.cacheTTL > 0 {
		value := time.Now().UTC().Add(h.cacheTTL)
		expiresAt = &value
	}
	if err := h.store.InsertSemanticCache(ctx, store.CacheEntry{
		ID:        newID("cache"),
		Key:       key,
		CreatedAt: time.Now().UTC(),
		ExpiresAt: expiresAt,
		Model:     semanticCacheScope(tenantScope, providerName, req),
		Prompt:    promptText,
		Vector:    cache.Vectorize(promptText),
		Response:  resp,
	}); err != nil {
		log.Printf("semantic cache insert failed: %v", err)
	}
}

func semanticCacheScope(tenantScope, providerName string, req provider.ChatRequest) string {
	parts := []string{tenantScope, providerName}
	parts = append(parts, requestCacheScope(req))
	return strings.Join(parts, "|")
}

func requestCacheScope(req provider.ChatRequest) string {
	parts := []string{req.Model}
	if req.Temperature != nil {
		parts = append(parts, fmt.Sprintf("temperature=%.4f", *req.Temperature))
	}
	if req.TopP != nil {
		parts = append(parts, fmt.Sprintf("top_p=%.4f", *req.TopP))
	}
	if req.MaxTokens != nil {
		parts = append(parts, fmt.Sprintf("max_tokens=%d", *req.MaxTokens))
	}
	if len(req.Stop) > 0 {
		parts = append(parts, "stop="+strings.Join(req.Stop, ","))
	}
	return strings.Join(parts, "|")
}

func (h *Handler) tenantCacheScope(r *http.Request) string {
	switch h.cacheScope {
	case "global":
		return "global"
	case "organization":
		if id := strings.TrimSpace(r.Header.Get("x-omniswitch-organization-id")); id != "" {
			return "organization:" + id
		}
	case "workspace":
		if id := strings.TrimSpace(r.Header.Get("x-omniswitch-workspace-id")); id != "" {
			return "workspace:" + id
		}
	}
	if id := strings.TrimSpace(r.Header.Get("x-omniswitch-key-id")); id != "" {
		return "api_key:" + id
	}
	return "anonymous"
}

func (h *Handler) shadow(ctx context.Context, activeRouter *router.Router, requestID, traceID, primaryProvider string, req provider.ChatRequest, shadowProvider string) {
	if h.store == nil || strings.TrimSpace(shadowProvider) == "" || shadowProvider == primaryProvider {
		return
	}
	if activeRouter == nil {
		activeRouter = h.router
	}
	// Background completions report upstream usage without sharing mutable
	// request labels or adding a second logical request.
	var metrics *telemetry.Metrics
	if observation := metricsObservation(ctx); observation != nil {
		metrics = observation.registry
	}
	go func() {
		shadowCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		resp, meta, err := activeRouter.Execute(shadowCtx, req, shadowProvider)
		if meta.Provider == "" {
			meta.Provider = shadowProvider
		}
		status := "success"
		errMsg := ""
		if err != nil {
			status = "error"
			errMsg = err.Error()
		} else if metrics != nil && !meta.Cached {
			metrics.RecordUsage(meta.Provider, firstNonEmpty(resp.Model, meta.Model, req.Model), resp.Usage.PromptTokens, resp.Usage.CompletionTokens, meta.Cost)
		}
		if err := h.store.InsertShadowLog(context.Background(), store.ShadowLog{
			ID:              newID("shadow"),
			RequestID:       requestID,
			TraceID:         traceID,
			Timestamp:       time.Now().UTC(),
			PrimaryProvider: primaryProvider,
			ShadowProvider:  meta.Provider,
			Model:           req.Model,
			LatencyMs:       float64(meta.Latency.Microseconds()) / 1000,
			Cost:            meta.Cost,
			Status:          status,
			ErrorMessage:    errMsg,
		}); err != nil {
			log.Printf("failed to log shadow request: %v", err)
		}
	}()
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": message, "type": "invalid_request_error"},
	})
}

// writeUnsupportedParameterError surfaces a provider adapter's refusal to
// silently drop a request field it cannot honour, matching OpenAI's error
// body shape so SDKs decode it the same way they decode a real OpenAI 400.
func writeUnsupportedParameterError(w http.ResponseWriter, err *provider.UnsupportedParameterError) {
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error": map[string]any{
			"message": err.Error(),
			"type":    "invalid_request_error",
			"param":   err.Param,
			"code":    "unsupported_parameter",
		},
	})
}

// asUnsupportedParameterError reports whether err (or a wrapped cause) is an
// UnsupportedParameterError from a provider adapter.
func asUnsupportedParameterError(err error) (*provider.UnsupportedParameterError, bool) {
	var upErr *provider.UnsupportedParameterError
	if errors.As(err, &upErr) {
		return upErr, true
	}
	return nil, false
}

func writeStream(w http.ResponseWriter, resp provider.ChatResponse) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported by this server")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	content := ""
	finishReason := "stop"
	var toolCalls []provider.ToolCall
	if len(resp.Choices) > 0 {
		content = resp.Choices[0].Message.Content
		toolCalls = resp.Choices[0].Message.ToolCalls
		finishReason = resp.Choices[0].FinishReason
	}
	if finishReason == "" {
		finishReason = "stop"
	}

	// Cached/redacted responses have no live token boundaries. Preserve
	// their exact whitespace instead of synthesizing words with strings.Fields.
	var tokens []string
	if content != "" {
		tokens = []string{content}
	}
	for _, token := range tokens {
		chunk := map[string]any{
			"id":      resp.ID,
			"object":  "chat.completion.chunk",
			"created": resp.Created,
			"model":   resp.Model,
			"choices": []map[string]any{
				{
					"index": 0,
					"delta": map[string]string{"content": token},
				},
			},
		}
		writeSSE(w, chunk)
		flusher.Flush()
	}
	if len(toolCalls) > 0 {
		deltas := make([]provider.ToolCall, len(toolCalls))
		for index, call := range toolCalls {
			i := index
			call.Index = &i
			deltas[index] = call
		}
		chunk := provider.ChatResponseChunk{
			ID:      resp.ID,
			Object:  "chat.completion.chunk",
			Created: resp.Created,
			Model:   resp.Model,
			Choices: []provider.ChunkChoice{{Index: 0, Delta: provider.Message{ToolCalls: deltas}}},
		}
		writeSSE(w, chunk)
		flusher.Flush()
	}

	finalChunk := map[string]any{
		"id":      resp.ID,
		"object":  "chat.completion.chunk",
		"created": resp.Created,
		"model":   resp.Model,
		"usage":   resp.Usage,
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]string{},
				"finish_reason": finishReason,
			},
		},
	}
	writeSSE(w, finalChunk)
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func mergeToolCallDeltas(target *[]provider.ToolCall, deltas []provider.ToolCall) {
	for _, delta := range deltas {
		index := len(*target)
		if delta.Index != nil && *delta.Index >= 0 {
			index = *delta.Index
		}
		for len(*target) <= index {
			*target = append(*target, provider.ToolCall{Type: "function"})
		}
		current := &(*target)[index]
		if delta.ID != "" {
			current.ID = delta.ID
		}
		if delta.Type != "" {
			current.Type = delta.Type
		}
		if current.Type == "" {
			current.Type = "function"
		}
		if delta.Function.Name != "" {
			current.Function.Name += delta.Function.Name
		}
		if delta.Function.Arguments != "" {
			current.Function.Arguments += delta.Function.Arguments
		}
	}
}

func writeSSE(w http.ResponseWriter, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func newRequestID() string {
	return newID("req")
}

func newID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return fmt.Sprintf("%s_%d_%s", prefix, time.Now().UnixNano(), hex.EncodeToString(b[:]))
}

func requestHeaderOrNew(r *http.Request, header string, prefix string) string {
	if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
		return value
	}
	return newID(prefix)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func marshalLogBody(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	const maxLoggedBodyBytes = 64 * 1024
	if len(data) <= maxLoggedBodyBytes {
		return string(data)
	}
	return string(data[:maxLoggedBodyBytes]) + "...[truncated]"
}
