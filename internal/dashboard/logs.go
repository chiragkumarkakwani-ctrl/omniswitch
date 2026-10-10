package dashboard

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/omniswitch-dev/omniswitch/internal/gateway"
	"github.com/omniswitch-dev/omniswitch/internal/store"
)

func timeWindow(r *http.Request) (string, time.Time, time.Time, error) {
	query, err := requestQuery(r)
	if err != nil {
		return "", time.Time{}, time.Time{}, err
	}
	return parseTimeWindow(strings.TrimSpace(query.Get("window")))
}

func requestQuery(r *http.Request) (url.Values, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("query parameters must be correctly URL-encoded")
	}
	return query, nil
}

func parseTimeWindow(window string) (string, time.Time, time.Time, error) {
	if window == "" {
		window = "24h"
	}
	durations := map[string]time.Duration{"1h": time.Hour, "6h": 6 * time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour}
	duration, ok := durations[window]
	if !ok {
		return "", time.Time{}, time.Time{}, fmt.Errorf("window must be 1h, 6h, 24h, 7d, or 30d")
	}
	until := time.Now().UTC()
	return window, until.Add(-duration), until, nil
}

func pagination(r *http.Request, maximum int) (int, int, error) {
	query, err := requestQuery(r)
	if err != nil {
		return 0, 0, err
	}
	limit, offset := 50, 0
	for _, field := range []struct {
		name  string
		value *int
	}{{"limit", &limit}, {"offset", &offset}} {
		if value := query.Get(field.name); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 0 || (field.name == "limit" && parsed == 0) {
				return 0, 0, fmt.Errorf("%s must be a non-negative integer (limit must be positive)", field.name)
			}
			*field.value = parsed
		}
	}
	if limit > maximum {
		limit = maximum
	}
	return limit, offset, nil
}

func logFilter(r *http.Request, maximum int) (store.LogFilter, error) {
	_, since, until, err := timeWindow(r)
	if err != nil {
		return store.LogFilter{}, err
	}
	query, err := requestQuery(r)
	if err != nil {
		return store.LogFilter{}, err
	}
	for _, bound := range []struct {
		name  string
		value *time.Time
	}{{"since", &since}, {"until", &until}} {
		if value := query.Get(bound.name); value != "" {
			parsed, err := time.Parse(time.RFC3339Nano, value)
			if err != nil || parsed.Year() < 1 {
				return store.LogFilter{}, fmt.Errorf("%s must be an RFC3339 timestamp", bound.name)
			}
			*bound.value = parsed.UTC()
		}
	}
	if !since.Before(until) || until.Sub(since) > 366*24*time.Hour {
		return store.LogFilter{}, fmt.Errorf("time range must be positive and no longer than 366 days")
	}
	limit, offset, err := pagination(r, maximum)
	if err != nil {
		return store.LogFilter{}, err
	}
	filter := store.LogFilter{
		Since: since, Until: until, Limit: limit, Offset: offset,
		Provider: query.Get("provider"), Model: query.Get("model"), Endpoint: query.Get("endpoint"),
		TraceID: query.Get("trace_id"), SessionID: query.Get("session_id"),
		Decision: strings.ToUpper(query.Get("decision")), Query: query.Get("q"), Sort: query.Get("sort"),
		APIKeyID: gateway.ScopedAPIKeyID(r),
	}
	if gateway.IsAdmin(r) {
		filter.APIKeyID = query.Get("api_key_id")
	} else if query.Has("api_key_id") {
		return store.LogFilter{}, fmt.Errorf("api_key_id filter requires admin role")
	}
	if filter.Decision != "" && filter.Decision != "ALLOW" && filter.Decision != "DENY" {
		return store.LogFilter{}, fmt.Errorf("decision must be ALLOW or DENY")
	}
	if filter.Sort != "" && filter.Sort != "time_desc" && filter.Sort != "time_asc" && filter.Sort != "latency_desc" {
		return store.LogFilter{}, fmt.Errorf("sort must be time_desc, time_asc, or latency_desc")
	}
	if query.Has("cached") {
		value, err := strconv.ParseBool(query.Get("cached"))
		if err != nil {
			return store.LogFilter{}, fmt.Errorf("cached must be true or false")
		}
		filter.Cached = &value
	}
	for _, value := range []struct{ name, text string }{
		{"provider", filter.Provider}, {"model", filter.Model}, {"api_key_id", filter.APIKeyID},
		{"trace_id", filter.TraceID}, {"session_id", filter.SessionID}, {"endpoint", filter.Endpoint}, {"q", filter.Query},
	} {
		if err := validateFilterValue(value.name, value.text); err != nil {
			return store.LogFilter{}, err
		}
	}
	seenStatuses := map[string]bool{}
	for _, values := range query["status"] {
		for _, status := range strings.Split(values, ",") {
			status = strings.TrimSpace(status)
			switch status {
			case "success", "error", "denied", "limited":
				if !seenStatuses[status] {
					filter.Statuses = append(filter.Statuses, status)
					seenStatuses[status] = true
				}
			default:
				return store.LogFilter{}, fmt.Errorf("status must contain success, error, denied, or limited")
			}
		}
	}
	return filter, nil
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	return false
}

func (h *Handler) getLogs(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if !gateway.IsAdmin(r) && r.URL.Query().Has("api_key_id") {
		writeError(w, http.StatusForbidden, "api_key_id filter requires admin role")
		return
	}
	filter, err := logFilter(r, 100)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	logs, total, err := h.store.ListLogs(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read request logs")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs, "total": total, "limit": filter.Limit, "offset": filter.Offset})
}

func (h *Handler) getLogFacets(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	_, since, until, err := timeWindow(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	facets, err := h.store.LogFacets(r.Context(), store.LogFilter{Since: since, Until: until, APIKeyID: scopedAPIKeyID(r)})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read log facets")
		return
	}
	writeJSON(w, http.StatusOK, facets)
}

var exportColumns = []string{"id", "timestamp", "trace_id", "session_id", "provider", "model", "api_key_id", "endpoint", "status", "decision", "input_tokens", "output_tokens", "total_tokens", "cached_tokens", "reasoning_tokens", "latency_ms", "cost", "cost_source", "cached"}

// csvText prevents a spreadsheet from interpreting untrusted model, trace, or
// key names as formulas. CSV quoting alone does not prevent formula injection.
func csvText(value string) string {
	trimmed := strings.TrimLeft(value, " \t\r\n")
	if trimmed != "" && strings.ContainsAny(trimmed[:1], "=+-@") {
		return "'" + value
	}
	return value
}

func (h *Handler) exportLogs(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if !gateway.IsAdmin(r) {
		writeError(w, http.StatusForbidden, "export requires admin role")
		return
	}
	if r.URL.Query().Get("format") != "csv" {
		writeError(w, http.StatusBadRequest, "format must be csv")
		return
	}
	filter, err := logFilter(r, store.MaxExportRows)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	filter.Limit = store.MaxExportRows
	if r.URL.Query().Has("limit") {
		filter.Limit, _, _ = pagination(r, store.MaxExportRows)
	}
	filter.OmitPayloads = true
	logs, _, err := h.store.ListLogs(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not export request logs")
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="omniswitch-requests-`+time.Now().UTC().Format("20060102T150405Z")+`.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	writer := csv.NewWriter(w)
	if err := writer.Write(exportColumns); err != nil {
		return
	}
	for _, log := range logs {
		row := []string{log.ID, log.Timestamp.UTC().Format(time.RFC3339Nano), log.TraceID, log.SessionID, log.Provider, log.Model, log.APIKeyID, log.Endpoint, log.Status, log.Decision,
			strconv.Itoa(log.InputTokens), strconv.Itoa(log.OutputTokens), strconv.Itoa(log.TotalTokens), strconv.Itoa(log.CachedTokens), strconv.Itoa(log.ReasoningTokens),
			strconv.FormatFloat(log.LatencyMs, 'f', -1, 64), strconv.FormatFloat(log.Cost, 'f', -1, 64), log.CostSource, strconv.FormatBool(log.Cached)}
		for i := range row {
			row[i] = csvText(row[i])
		}
		if err := writer.Write(row); err != nil {
			return
		}
	}
	writer.Flush()
}

func validateFilterValue(name, value string) error {
	if len(value) > 256 {
		return fmt.Errorf("%s must not exceed 256 bytes", name)
	}
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return fmt.Errorf("%s must be valid UTF-8 text without NUL characters", name)
	}
	return nil
}
