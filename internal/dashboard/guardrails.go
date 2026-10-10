package dashboard

import (
	"net/http"
	"regexp"

	"github.com/omniswitch-dev/omniswitch/internal/store"
)

var detailRedactors = []struct {
	pattern *regexp.Regexp
	replace string
}{
	{regexp.MustCompile(`(?i)sk-[^\s"',;]+`), "[REDACTED]"},
	{regexp.MustCompile(`(?i)bearer\s+[^\s"',;]+`), "Bearer [REDACTED]"},
	{regexp.MustCompile(`(?i)(?:api[_-]?key|client[_-]?secret|password|token)["']?\s*[:=]\s*["']?[^\s"',;]+`), "[REDACTED]"},
	{regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+`), "[REDACTED]"},
}

func redactDetails(details string) string {
	for _, redactor := range detailRedactors {
		details = redactor.pattern.ReplaceAllString(details, redactor.replace)
	}
	runes := []rune(details)
	if len(runes) > 256 {
		details = string(runes[:256])
	}
	return details
}

func (h *Handler) getGuardrailEvents(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	_, since, until, err := timeWindow(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, offset, err := pagination(r, 100)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	filter := store.GuardrailFilter{
		Since: since, Until: until, APIKeyID: scopedAPIKeyID(r),
		Type: r.URL.Query().Get("type"), Action: r.URL.Query().Get("action"),
		TraceID: r.URL.Query().Get("trace_id"), Limit: limit, Offset: offset,
	}
	for _, value := range []struct{ name, text string }{
		{"type", filter.Type}, {"action", filter.Action}, {"trace_id", filter.TraceID},
	} {
		if err := validateFilterValue(value.name, value.text); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	events, total, err := h.store.ListGuardrailEvents(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read guardrail events")
		return
	}
	for i := range events {
		events[i].Details = redactDetails(events[i].Details)
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "total": total, "limit": limit, "offset": offset})
}

func (h *Handler) getGuardrailSummary(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	_, since, until, err := timeWindow(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	summary, err := h.store.GuardrailSummary(r.Context(), store.GuardrailFilter{Since: since, Until: until, APIKeyID: scopedAPIKeyID(r)})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read guardrail summary")
		return
	}
	writeJSON(w, http.StatusOK, summary)
}
