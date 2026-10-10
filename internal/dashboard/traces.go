package dashboard

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/omniswitch-dev/omniswitch/internal/store"
)

type traceLog struct {
	store.RequestLog
	KeyName string `json:"key_name"`
}

func (h *Handler) getTrace(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	traceID := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/api/traces/"))
	if traceID == "" || validateFilterValue("trace_id", traceID) != nil || strings.Contains(traceID, "/") {
		writeError(w, http.StatusBadRequest, "a valid trace id is required")
		return
	}
	query, err := requestQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	window := query.Get("window")
	if window == "" {
		window = "30d"
	}
	_, since, until, err := parseTimeWindow(window)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	scope := scopedAPIKeyID(r)
	detail, err := h.store.GetTraceInRange(r.Context(), traceID, scope, since, until)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "trace not found")
		} else {
			writeError(w, http.StatusInternalServerError, "could not read trace")
		}
		return
	}
	keyIDs := make([]string, 0, len(detail.Logs))
	requestIDs := make([]string, 0, len(detail.Logs))
	for _, entry := range detail.Logs {
		keyIDs = append(keyIDs, entry.APIKeyID)
		requestIDs = append(requestIDs, entry.ID)
	}
	keyNames, err := h.store.APIKeyNames(r.Context(), keyIDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read trace key names")
		return
	}
	logs := make([]traceLog, 0, len(detail.Logs))
	spans := []store.TraceSpan{}
	for _, entry := range detail.Logs {
		var metadata struct {
			Spans []store.TraceSpan `json:"_spans"`
		}
		if json.Unmarshal([]byte(entry.Metadata), &metadata) == nil {
			spans = append(spans, metadata.Spans...)
		}
		logs = append(logs, traceLog{RequestLog: entry, KeyName: keyNames[entry.APIKeyID]})
	}
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].Start.Before(spans[j].Start) })
	events, _, err := h.store.ListGuardrailEvents(r.Context(), store.GuardrailFilter{
		Since: since, Until: until, APIKeyID: scope, TraceID: traceID, RequestIDs: requestIDs, Limit: 100,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read trace guardrails")
		return
	}
	for i := range events {
		events[i].Details = redactDetails(events[i].Details)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"trace_id": traceID, "logs": logs, "guardrail_events": events,
		"shadow_logs": detail.ShadowLogs, "feedback": detail.Feedback, "spans": spans,
	})
}
