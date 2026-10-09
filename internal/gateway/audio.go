package gateway

import (
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/provider"
)

// Transcription accepts bounded multipart audio and returns a guarded JSON
// transcript. Uploaded binary data is never copied into request logs.
func (h *Handler) Transcription(w http.ResponseWriter, r *http.Request) {
	m := h.newMediaRequest(w, r, "transcription")
	if r.Method != http.MethodPost {
		m.reject(w, http.StatusMethodNotAllowed, "POST is required", "invalid_request_error", "method_not_allowed")
		return
	}
	limit := h.maxUploadBytes.Load()
	if r.ContentLength > limit {
		m.reject(w, http.StatusRequestEntityTooLarge, "multipart body exceeds gateway.max_upload_bytes", "invalid_request_error", "payload_too_large")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		m.reject(w, http.StatusBadRequest, "multipart/form-data is required", "invalid_request_error", "invalid_request_body")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	if err := r.ParseMultipartForm(min(limit, 1<<20)); err != nil {
		m.bodyError(w, err)
		return
	}
	request := provider.TranscriptionRequest{
		Model: strings.TrimSpace(r.FormValue("model")), Language: r.FormValue("language"),
		Prompt: r.FormValue("prompt"), ResponseFormat: r.FormValue("response_format"),
	}
	if request.Model == "" {
		m.reject(w, http.StatusBadRequest, "model is required", "invalid_request_error", "invalid_parameter")
		return
	}
	if request.ResponseFormat != "" && request.ResponseFormat != "json" {
		m.reject(w, http.StatusBadRequest, "transcription response_format must be json", "invalid_request_error", "unsupported_parameter")
		return
	}
	if raw := r.FormValue("temperature"); raw != "" {
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || !(value >= 0 && value <= 1) {
			m.reject(w, http.StatusBadRequest, "temperature must be between 0 and 1", "invalid_request_error", "invalid_parameter")
			return
		}
		request.Temperature = &value
	}
	file, header, err := r.FormFile("file")
	if err != nil || header.Size == 0 {
		if file != nil {
			_ = file.Close()
		}
		m.reject(w, http.StatusBadRequest, "a nonempty audio file is required", "invalid_request_error", "invalid_parameter")
		return
	}
	defer file.Close()
	request.File, request.Filename, request.ContentType = file, header.Filename, header.Header.Get("Content-Type")
	m.usage.UploadBytes = header.Size
	prompt, allowed := m.authorize(w, request.Model, request.Prompt)
	if !allowed {
		return
	}
	defer m.reservation.Release()
	request.Prompt = prompt
	response, meta, err := h.router.Transcription(m.ctx, request, r.Header.Get("x-omniswitch-provider"))
	if err != nil {
		m.providerError(w, meta, err)
		return
	}
	m.complete(meta)
	logResponse := provider.ChatResponse{
		Object: "audio.transcription", Model: meta.Model, Created: time.Now().Unix(),
		Choices: []provider.Choice{{Message: provider.Message{Role: "assistant", Content: response.Text}}},
	}
	disposition := h.outputGuardrailDisposition(m.ctx, m.log.ID, &logResponse)
	m.log.Response = &logResponse
	if disposition.Denied || disposition.Retry || disposition.RerouteProvider != "" || disposition.Fallback != "" {
		reason := disposition.Reason
		if reason == "" {
			reason = "transcript blocked by output guardrails"
		}
		m.reject(w, http.StatusForbidden, reason, "guardrail", "guardrail_triggered")
		return
	}
	response.Text = logResponse.Choices[0].Message.Content
	m.writeLog("success", "")
	writeJSON(w, http.StatusOK, response)
}

// Speech relays the upstream binary body incrementally with its content type.
// Input text is guarded before any provider request; audio is not text-scanned.
func (h *Handler) Speech(w http.ResponseWriter, r *http.Request) {
	m := h.newMediaRequest(w, r, "speech")
	if r.Method != http.MethodPost {
		m.reject(w, http.StatusMethodNotAllowed, "POST is required", "invalid_request_error", "method_not_allowed")
		return
	}
	var request provider.SpeechRequest
	if err := decodeMediaJSON(w, r, h.maxRequestBytes, &request); err != nil {
		m.bodyError(w, err)
		return
	}
	request.Model = strings.TrimSpace(request.Model)
	if request.Model == "" || strings.TrimSpace(request.Input) == "" || strings.TrimSpace(request.Voice) == "" {
		m.reject(w, http.StatusBadRequest, "model, input, and voice are required", "invalid_request_error", "invalid_parameter")
		return
	}
	if request.Speed != 0 && !(request.Speed >= 0.25 && request.Speed <= 4) {
		m.reject(w, http.StatusBadRequest, "speed must be between 0.25 and 4", "invalid_request_error", "invalid_parameter")
		return
	}
	input, allowed := m.authorize(w, request.Model, request.Input)
	if !allowed {
		return
	}
	defer m.reservation.Release()
	request.Input = input
	body, contentType, meta, err := h.router.Speech(m.ctx, request, r.Header.Get("x-omniswitch-provider"))
	if err != nil {
		m.providerError(w, meta, err)
		return
	}
	defer body.Close()
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, copyErr := io.Copy(mediaFlushWriter{w}, body)
	m.complete(meta)
	if copyErr != nil {
		// Headers/body may already be sent; never append a JSON error to audio.
		m.writeLog("error", "speech stream failed: "+copyErr.Error())
		return
	}
	m.log.Response = &provider.ChatResponse{Object: "audio.speech", Model: meta.Model, Created: time.Now().Unix()}
	m.writeLog("success", "")
}

type mediaFlushWriter struct{ http.ResponseWriter }

func (w mediaFlushWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok && n > 0 {
		flusher.Flush()
	}
	return n, err
}
