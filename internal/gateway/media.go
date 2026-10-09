package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/omniswitch-dev/omniswitch/internal/provider"
)

const defaultMaxUploadBytes int64 = 25 << 20

type mediaUsage struct {
	Operation    string  `json:"operation"`
	Images       int     `json:"images,omitempty"`
	Quality      string  `json:"quality,omitempty"`
	Size         string  `json:"size,omitempty"`
	AudioSeconds float64 `json:"audio_seconds,omitempty"`
	Characters   int     `json:"characters,omitempty"`
	UploadBytes  int64   `json:"upload_bytes,omitempty"`
}

type mediaReservation struct {
	Complete func(mediaUsage, float64)
	Release  func()
}

// priceMediaUsage is the integration point for the catalog's image, audio-minute
// and character prices. Do not retain a provider's legacy image-price estimate.
func (h *Handler) priceMediaUsage(_ mediaUsage, _ provider.ProviderMeta) (float64, string) {
	return 0, "unpriced"
}

// reserveMediaLimits is the integration point for token/spend reservations.
// Complete reconciles actual usage; Release must be safe after Complete.
func (h *Handler) reserveMediaLimits(_ context.Context, _ string, _ mediaUsage) (mediaReservation, error) {
	return mediaReservation{Complete: func(mediaUsage, float64) {}, Release: func() {}}, nil
}

type mediaCostSourceKey struct{}

// MediaCostSourceFromContext exposes the media pricing decision to the central
// request logger when its cost_source persistence is integrated.
func MediaCostSourceFromContext(ctx context.Context) string {
	source, _ := ctx.Value(mediaCostSourceKey{}).(string)
	return source
}

type mediaRequest struct {
	handler     *Handler
	ctx         context.Context
	log         logContext
	usage       mediaUsage
	started     time.Time
	reservation mediaReservation
}

func (h *Handler) newMediaRequest(w http.ResponseWriter, r *http.Request, operation string) *mediaRequest {
	id, traceID := newRequestID(), requestHeaderOrNew(r, "x-omniswitch-trace-id", "trace")
	sessionID := r.Header.Get("x-omniswitch-session-id")
	ctx := WithRequestID(r.Context(), id)
	ctx = WithTraceID(ctx, traceID)
	ctx = WithSessionID(ctx, sessionID)
	ctx = WithUserID(ctx, r.Header.Get("x-omniswitch-user-id"))
	ctx = WithTags(ctx, tagsHeader(r.Header.Get("x-omniswitch-tags")))
	ctx = WithMetadataJSON(ctx, r.Header.Get("x-omniswitch-metadata"))
	ctx = WithTraceRecorder(ctx, newTraceRecorder())
	w.Header().Set("x-omniswitch-trace-id", traceID)
	w.Header().Set("x-omniswitch-session-id", sessionID)
	m := &mediaRequest{
		handler: h, ctx: ctx, started: time.Now(), usage: mediaUsage{Operation: operation},
		log: logContext{ID: id, TraceID: traceID, SessionID: sessionID, APIKeyID: r.Header.Get("x-omniswitch-key-id")},
	}
	m.setCostSource("unpriced")
	return m
}

func (m *mediaRequest) setCostSource(source string) {
	metadata := map[string]any{}
	_ = json.Unmarshal([]byte(MetadataJSONFromContext(m.ctx)), &metadata)
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["cost_source"] = source
	metadata["media"] = m.usage
	payload, _ := json.Marshal(metadata)
	m.ctx = WithMetadataJSON(context.WithValue(m.ctx, mediaCostSourceKey{}, source), string(payload))
}

func (m *mediaRequest) authorize(w http.ResponseWriter, model, text string) (string, bool) {
	m.log.Request = provider.ChatRequest{Model: model, Messages: []provider.Message{{Role: "user", Content: text}}}
	if denied, reason := m.handler.budgetExceeded(m.ctx, m.log.APIKeyID); denied {
		m.reject(w, http.StatusPaymentRequired, reason, "budget_exceeded", "budget_exceeded")
		return "", false
	}
	if gr := m.handler.guardrails.Load(); gr != nil {
		results := gr.EvaluateInputContext(m.ctx, m.log.Request.Messages)
		m.handler.recordGuardrailResults(m.ctx, m.log.ID, results)
		for _, result := range results {
			if !result.Triggered {
				continue
			}
			switch result.Action {
			case "deny", "retry", "reroute", "fallback":
				m.reject(w, http.StatusForbidden, result.Message, result.Type, "guardrail_triggered")
				return "", false
			case "redact":
				text = "[REDACTED BY OMNISWITCH]"
			}
		}
	}
	m.log.Request.Messages[0].Content = text
	if m.usage.Operation == "speech" {
		m.usage.Characters = utf8.RuneCountInString(text)
	}
	m.setCostSource("unpriced")
	reservation, err := m.handler.reserveMediaLimits(m.ctx, m.log.APIKeyID, m.usage)
	if err != nil {
		m.reject(w, http.StatusTooManyRequests, err.Error(), "rate_limit_error", "rate_limit_exceeded")
		return "", false
	}
	m.reservation = reservation
	return text, true
}

func (m *mediaRequest) complete(meta provider.ProviderMeta) {
	cost, source := m.handler.priceMediaUsage(m.usage, meta)
	meta.Cost = cost
	m.log.Meta = &meta
	m.setCostSource(source)
	m.reservation.Complete(m.usage, cost)
}

func (m *mediaRequest) writeLog(status, message string) {
	m.log.Status, m.log.ErrorMessage = status, message
	if m.log.Meta == nil {
		m.log.Meta = &provider.ProviderMeta{}
	}
	m.log.Meta.Latency = time.Since(m.started)
	m.handler.logRequest(m.ctx, m.log)
}

func (m *mediaRequest) reject(w http.ResponseWriter, status int, message, errorType, code string) {
	logStatus := "error"
	if status == http.StatusForbidden || status == http.StatusPaymentRequired {
		logStatus = "denied"
	}
	m.writeLog(logStatus, message)
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": message, "type": errorType, "code": code}})
}

func (m *mediaRequest) providerError(w http.ResponseWriter, meta provider.ProviderMeta, err error) {
	meta.Cost = 0
	m.log.Meta = &meta
	var unsupported *provider.UnsupportedEndpointError
	if errors.As(err, &unsupported) {
		m.log.Meta.Provider = unsupported.Provider
		m.reject(w, http.StatusBadRequest, unsupported.Error(), "invalid_request_error", "unsupported_endpoint")
		return
	}
	m.reject(w, http.StatusBadGateway, "media provider error: "+err.Error(), "api_error", "provider_error")
}

func (m *mediaRequest) bodyError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		m.reject(w, http.StatusRequestEntityTooLarge, "request body exceeds the configured byte limit", "invalid_request_error", "payload_too_large")
		return
	}
	m.reject(w, http.StatusBadRequest, "invalid request body", "invalid_request_error", "invalid_request_body")
}

func decodeMediaJSON(w http.ResponseWriter, r *http.Request, limit int64, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
