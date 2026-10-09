package gateway

import (
	"net/http"
	"strings"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/provider"
)

// ImageGeneration serves OpenAI-compatible JSON image requests. Images remain
// provider-generated; guardrails inspect the prompt, not the image pixels.
func (h *Handler) ImageGeneration(w http.ResponseWriter, r *http.Request) {
	m := h.newMediaRequest(w, r, "image_generation")
	if r.Method != http.MethodPost {
		m.reject(w, http.StatusMethodNotAllowed, "POST is required", "invalid_request_error", "method_not_allowed")
		return
	}
	var request provider.ImageRequest
	if err := decodeMediaJSON(w, r, h.maxRequestBytes, &request); err != nil {
		m.bodyError(w, err)
		return
	}
	request.Model = strings.TrimSpace(request.Model)
	if request.Model == "" {
		request.Model = "dall-e-2"
	}
	if strings.TrimSpace(request.Prompt) == "" || request.N < 0 {
		m.reject(w, http.StatusBadRequest, "prompt is required and n must be positive", "invalid_request_error", "invalid_parameter")
		return
	}
	if request.N == 0 {
		request.N = 1
	}
	m.usage.Images, m.usage.Quality, m.usage.Size = request.N, request.Quality, request.Size
	prompt, allowed := m.authorize(w, request.Model, request.Prompt)
	if !allowed {
		return
	}
	defer m.reservation.Release()
	request.Prompt = prompt
	response, meta, err := h.router.ImageGeneration(m.ctx, request, r.Header.Get("x-omniswitch-provider"))
	if err != nil {
		m.providerError(w, meta, err)
		return
	}
	m.usage.Images = len(response.Data)
	m.complete(meta)
	m.log.Response = &provider.ChatResponse{Object: "image_generation", Model: meta.Model, Created: time.Now().Unix()}
	m.writeLog("success", "")
	writeJSON(w, http.StatusOK, response)
}
