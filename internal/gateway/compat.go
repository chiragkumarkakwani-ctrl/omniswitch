package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/compat"
	"github.com/omniswitch-dev/omniswitch/internal/provider"
)

// Responses translates the stateless Responses API through the chat pipeline.
func (h *Handler) Responses(w http.ResponseWriter, r *http.Request) {
	w, r, finishMetrics := h.beginRequestMetrics(w, r)
	defer finishMetrics()
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "only POST is supported")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.maxRequestBytes)
	var request compat.ResponsesRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	setRequestMetricModel(r.Context(), request.Model)
	chatRequest, err := compat.ResponsesToChat(request)
	if err != nil {
		var parameterError *compat.RequestError
		if errors.As(err, &parameterError) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{
				"message": parameterError.Message, "type": "invalid_request_error", "param": parameterError.Param, "code": parameterError.Code,
			}})
		} else {
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	if request.Stream {
		h.executeCompatibleStream(w, r, chatRequest, compat.NewResponsesStream(request), false)
		return
	}
	chat, headers, status, body, err := h.executeCompatibleChat(r, chatRequest)
	copyHeaders(w.Header(), headers)
	if err != nil {
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}
	writeJSON(w, http.StatusOK, compat.ResponsesFromChat(request, chat))
}

// Messages uses the same guardrails, tools, routing, cache, and request logging.
func (h *Handler) Messages(w http.ResponseWriter, r *http.Request) {
	w, r, finishMetrics := h.beginRequestMetrics(w, r)
	defer finishMetrics()
	if r.Method != http.MethodPost {
		writeMessagesError(w, http.StatusMethodNotAllowed, "only POST is supported")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.maxRequestBytes)
	var request compat.MessagesRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeMessagesError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	setRequestMetricModel(r.Context(), request.Model)
	chatRequest, err := compat.MessagesToChat(request)
	if err != nil {
		writeMessagesError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.TopK != nil {
		ctx := provider.WithAnthropicTopK(r.Context(), *request.TopK)
		r = r.WithContext(context.WithValue(ctx, messagesTopKCacheKey{}, *request.TopK))
		w.Header().Set("x-omniswitch-warnings", "top_k applies only to native Anthropic upstreams; other upstreams ignore it")
	}
	r = r.WithContext(context.WithValue(r.Context(), messagesResponseValidationKey{}, true))
	if request.Stream {
		h.executeCompatibleStream(w, r, chatRequest, compat.NewMessagesStream(request.Model), true)
		return
	}
	chat, headers, status, body, err := h.executeCompatibleChat(r, chatRequest)
	copyHeaders(w.Header(), headers)
	if err != nil {
		writeJSON(w, status, compat.MessagesError(status, body))
		return
	}
	message, err := compat.MessagesFromChat(chat)
	if err != nil {
		writeMessagesError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, message)
}

type messagesTopKCacheKey struct{}

// top_k is carried in context so OpenAI backends never receive it. Keep cache
// entries distinct across top_k values, including native-provider fallbacks.
func compatibleCacheProvider(ctx context.Context, name string) string {
	if topK, ok := ctx.Value(messagesTopKCacheKey{}).(int); ok {
		return fmt.Sprintf("%s|anthropic-top-k:%d", name, topK)
	}
	return name
}

type messagesResponseValidationKey struct{}

// Called before output is logged/cached or buffered SSE is released, so invalid
// tool arguments retain HTTP 502 and never enter the response cache.
func validateCompatibleResponse(ctx context.Context, response provider.ChatResponse) error {
	if enabled, _ := ctx.Value(messagesResponseValidationKey{}).(bool); enabled {
		_, err := compat.MessagesFromChat(response)
		return err
	}
	return nil
}

// enforceCompatibleResponse is a narrow chat-pipeline hook; validation itself
// remains a pure function in internal/compat.
func (h *Handler) enforceCompatibleResponse(w http.ResponseWriter, ctx context.Context, logCtx logContext) bool {
	if err := validateCompatibleResponse(ctx, *logCtx.Response); err != nil {
		logCtx.Status, logCtx.ErrorMessage = "error", err.Error()
		h.logRequest(ctx, logCtx)
		w.Header().Set("x-omniswitch-trace-id", logCtx.TraceID)
		w.Header().Set("x-omniswitch-session-id", logCtx.SessionID)
		writeError(w, http.StatusBadGateway, err.Error())
		return false
	}
	return true
}

func writeMessagesError(w http.ResponseWriter, status int, message string) {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"message": message}})
	writeJSON(w, status, compat.MessagesError(status, body))
}

// MessagesCompatibilityMiddleware belongs outside the gateway middleware chain:
// auth/authorization/rate-limit failures must also use the Anthropic envelope.
// The official Anthropic SDK sends x-api-key rather than Authorization.
func MessagesCompatibilityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Authorization") == "" && r.Header.Get("x-api-key") != "" {
			r = r.Clone(r.Context())
			r.Header.Set("Authorization", "Bearer "+r.Header.Get("x-api-key"))
		}
		writer := &messagesEnvelopeWriter{target: w}
		next.ServeHTTP(writer, r)
		if writer.status >= 400 {
			w.Header().Del("Content-Length")
			writeJSON(w, writer.status, compat.MessagesError(writer.status, writer.body.Bytes()))
		}
	})
}

type messagesEnvelopeWriter struct {
	target http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *messagesEnvelopeWriter) Unwrap() http.ResponseWriter { return w.target }
func (w *messagesEnvelopeWriter) Header() http.Header         { return w.target.Header() }
func (w *messagesEnvelopeWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	if status < 400 {
		w.target.WriteHeader(status)
	}
}
func (w *messagesEnvelopeWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.status >= 400 {
		return w.body.Write(data)
	}
	return w.target.Write(data)
}
func (w *messagesEnvelopeWriter) Flush() {
	if w.status < 400 {
		if flusher, ok := w.target.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}

func (h *Handler) executeCompatibleChat(r *http.Request, request provider.ChatRequest) (provider.ChatResponse, http.Header, int, []byte, error) {
	clone, err := compatibleChatRequest(r, request)
	if err != nil {
		body, _ := json.Marshal(map[string]any{"error": map[string]string{"message": err.Error(), "type": "api_error"}})
		return provider.ChatResponse{}, nil, http.StatusInternalServerError, body, err
	}
	recorder := &bufferedResponseWriter{header: make(http.Header)}
	h.ChatCompletions(recorder, clone)
	if recorder.status == 0 {
		recorder.status = http.StatusOK
	}
	if recorder.status != http.StatusOK {
		return provider.ChatResponse{}, recorder.header, recorder.status, recorder.body.Bytes(), fmt.Errorf("chat request failed")
	}
	var response provider.ChatResponse
	if err := json.Unmarshal(recorder.body.Bytes(), &response); err != nil {
		body, _ := json.Marshal(map[string]any{"error": map[string]string{"message": "invalid upstream chat response", "type": "api_error"}})
		return provider.ChatResponse{}, recorder.header, http.StatusBadGateway, body, err
	}
	return response, recorder.header, recorder.status, recorder.body.Bytes(), nil
}

func compatibleChatRequest(r *http.Request, request provider.ChatRequest) (*http.Request, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	clone := r.Clone(r.Context())
	clone.Method = http.MethodPost
	clone.Body = io.NopCloser(bytes.NewReader(body))
	clone.ContentLength = int64(len(body))
	// Keep the original path: P2a derives request_logs.endpoint from it.
	return clone, nil
}

type sseTranslator interface {
	Translate(payload string) ([]compat.Event, error)
}

func (h *Handler) executeCompatibleStream(w http.ResponseWriter, r *http.Request, request provider.ChatRequest, translator sseTranslator, messages bool) {
	clone, err := compatibleChatRequest(r, request)
	if err != nil {
		if messages {
			writeMessagesError(w, http.StatusBadRequest, err.Error())
		} else {
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writer := newTransformResponseWriter(w, translator, messages)
	// Only this goroutine and the synchronous chat pipeline touch writer;
	// writer serializes all actual network writes, including keepalives.
	done := make(chan struct{})
	var pings sync.WaitGroup
	if messages {
		pings.Add(1)
		go func() {
			defer pings.Done()
			timer := time.NewTimer(15 * time.Second)
			defer timer.Stop()
			for {
				select {
				case <-done:
					return
				case now := <-timer.C:
					next := writer.ping(now, 15*time.Second)
					timer.Reset(next)
				}
			}
		}()
	}
	h.ChatCompletions(writer, clone)
	close(done)
	pings.Wait()
	writer.close()
}

type transformResponseWriter struct {
	mu         sync.Mutex
	target     http.ResponseWriter
	translator sseTranslator
	header     http.Header
	buffer     string
	body       bytes.Buffer
	status     int
	streaming  bool
	wrote      bool
	failed     bool
	messages   bool
	lastWrite  time.Time
}

func newTransformResponseWriter(target http.ResponseWriter, translator sseTranslator, messages bool) *transformResponseWriter {
	return &transformResponseWriter{target: target, translator: translator, messages: messages, header: make(http.Header)}
}
func (w *transformResponseWriter) Unwrap() http.ResponseWriter { return w.target }
func (w *transformResponseWriter) Header() http.Header         { return w.header }
func (w *transformResponseWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status != 0 {
		return
	}
	w.status = status
	w.streaming = status == http.StatusOK && strings.Contains(strings.ToLower(w.header.Get("Content-Type")), "text/event-stream")
	if w.streaming {
		w.lastWrite = time.Now()
	}
}
func (w *transformResponseWriter) commit(status int) {
	if w.wrote {
		return
	}
	copyHeaders(w.target.Header(), w.header)
	w.target.Header().Del("Content-Length")
	w.target.WriteHeader(status)
	w.wrote = true
}
func (w *transformResponseWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failed {
		return len(data), nil
	}
	if w.status == 0 {
		w.status = http.StatusOK
		w.streaming = strings.Contains(strings.ToLower(w.header.Get("Content-Type")), "text/event-stream")
	}
	if !w.streaming {
		return w.body.Write(data)
	}
	w.buffer += string(data)
	for {
		index := strings.Index(w.buffer, "\n\n")
		if index < 0 {
			break
		}
		raw := w.buffer[:index]
		w.buffer = w.buffer[index+2:]
		var lines []string
		for _, line := range strings.Split(raw, "\n") {
			if strings.HasPrefix(line, "data:") {
				lines = append(lines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if len(lines) == 0 {
			continue
		}
		events, err := w.translator.Translate(strings.Join(lines, "\n"))
		if err != nil {
			w.fail(err)
			return len(data), nil
		}
		for _, event := range events {
			w.commit(http.StatusOK)
			if err := writeNamedSSE(w.target, event); err != nil {
				w.failed = true
				return len(data), err
			}
			w.lastWrite = time.Now()
		}
	}
	return len(data), nil
}
func (w *transformResponseWriter) fail(err error) {
	w.failed = true
	if w.wrote {
		if w.messages {
			body, _ := json.Marshal(map[string]any{"error": map[string]string{"message": err.Error()}})
			_ = writeNamedSSE(w.target, compat.Event{Event: "error", Data: compat.MessagesError(http.StatusBadGateway, body)})
		} else if responses, ok := w.translator.(*compat.ResponsesStream); ok {
			for _, event := range responses.Fail(err.Error()) {
				_ = writeNamedSSE(w.target, event)
			}
		}
		return
	}
	copyHeaders(w.target.Header(), w.header)
	w.target.Header().Del("Content-Length")
	if w.messages {
		writeMessagesError(w.target, http.StatusBadGateway, err.Error())
	} else {
		writeError(w.target, http.StatusBadGateway, err.Error())
	}
	w.wrote = true
}
func (w *transformResponseWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.wrote {
		if flusher, ok := w.target.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}
func (w *transformResponseWriter) ping(now time.Time, idle time.Duration) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.streaming || w.failed {
		return idle
	}
	if !w.lastWrite.IsZero() && now.Sub(w.lastWrite) < idle {
		return idle - now.Sub(w.lastWrite)
	}
	w.commit(http.StatusOK)
	if err := writeNamedSSE(w.target, compat.MessagePing()); err != nil {
		w.failed = true
		return idle
	}
	w.lastWrite = now
	if flusher, ok := w.target.(http.Flusher); ok {
		flusher.Flush()
	}
	return idle
}
func (w *transformResponseWriter) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.streaming || w.failed {
		return
	}
	status := w.status
	if status == 0 {
		status = http.StatusBadGateway
	}
	copyHeaders(w.target.Header(), w.header)
	w.target.Header().Del("Content-Length")
	if w.messages {
		writeJSON(w.target, status, compat.MessagesError(status, w.body.Bytes()))
	} else {
		w.target.WriteHeader(status)
		_, _ = w.target.Write(w.body.Bytes())
	}
	w.wrote = true
}

func writeNamedSSE(w http.ResponseWriter, event compat.Event) error {
	data, err := json.Marshal(event.Data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Event, data)
	return err
}

type bufferedResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *bufferedResponseWriter) Header() http.Header { return w.header }
func (w *bufferedResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *bufferedResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}
func copyHeaders(destination, source http.Header) {
	for key, values := range source {
		if key == "Content-Type" && destination.Get(key) == "text/event-stream" {
			continue
		}
		destination[key] = append([]string(nil), values...)
	}
}
