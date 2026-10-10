package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

const MaxExportRows = 10000

// LogFilter is shared by the request list, facets, and CSV export. Until is
// exclusive. Empty time bounds default to the last 24 hours; results are bounded
// even when the caller omits pagination. SQL identifiers never come from input.
type LogFilter struct {
	Since, Until                 time.Time
	Provider, Model, APIKeyID    string
	TraceID, SessionID, Decision string
	Endpoint, Query, Sort        string
	Statuses                     []string
	Cached                       *bool
	Limit, Offset                int
	OmitPayloads                 bool
}

func (f LogFilter) normalized() LogFilter {
	if f.Until.IsZero() {
		f.Until = time.Now().UTC()
	}
	if f.Since.IsZero() {
		f.Since = f.Until.Add(-24 * time.Hour)
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > MaxExportRows {
		f.Limit = MaxExportRows
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	return f
}

// indexedTimeRange preserves the timestamp index while comparing mixed
// RFC3339/RFC3339Nano text correctly ("...00Z" sorts after "...00.1Z"). All
// gateway timestamps are UTC. PostgreSQL uses native timestamp comparisons.
func (s *Store) indexedTimeRange(column string, since, until time.Time) (string, []any) {
	where := column + " >= ? AND " + column + " < ?"
	args := []any{
		since.UTC().Format("2006-01-02T15:04:05"),
		until.UTC().Truncate(time.Second).Add(time.Second).Format("2006-01-02T15:04:05"),
	}
	expr := timestampOrderExpression(column)
	if s.pg {
		expr = column + "::timestamptz"
	}
	where += " AND " + expr + " >= ? AND " + expr + " < ?"
	if s.pg {
		args = append(args, since.UTC(), until.UTC())
	} else {
		args = append(args, since.UTC().Format("2006-01-02T15:04:05.000000000"), until.UTC().Format("2006-01-02T15:04:05.000000000"))
	}
	return where, args
}

func timestampOrderExpression(column string) string {
	return "(substr(" + column + ", 1, 19) || '.' || substr(rtrim(CASE WHEN substr(" + column + ", 20, 1) = '.' THEN substr(" + column + ", 21) ELSE '' END, 'Z') || '000000000', 1, 9))"
}

func (s *Store) logWhere(filter LogFilter) (string, []any) {
	where, args := s.indexedTimeRange("timestamp", filter.Since, filter.Until)
	for _, pair := range [][2]string{
		{"provider", filter.Provider}, {"model", filter.Model}, {"api_key_id", filter.APIKeyID},
		{"trace_id", filter.TraceID}, {"session_id", filter.SessionID}, {"decision", filter.Decision}, {"endpoint", filter.Endpoint},
	} {
		if pair[1] != "" {
			where += " AND " + pair[0] + " = ?"
			args = append(args, pair[1])
		}
	}
	if len(filter.Statuses) > 0 {
		where += " AND status IN (" + strings.TrimSuffix(strings.Repeat("?,", len(filter.Statuses)), ",") + ")"
		for _, status := range filter.Statuses {
			args = append(args, status)
		}
	}
	if filter.Cached != nil {
		where += " AND cached = ?"
		args = append(args, boolToInt(*filter.Cached))
	}
	if filter.Query != "" {
		// Treat the user's text literally, not as SQL LIKE wildcards.
		text := "%" + strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(strings.ToLower(filter.Query)) + "%"
		where += " AND (LOWER(model) LIKE ? ESCAPE '\\' OR LOWER(trace_id) LIKE ? ESCAPE '\\' OR LOWER(session_id) LIKE ? ESCAPE '\\' OR LOWER(error_message) LIKE ? ESCAPE '\\')"
		args = append(args, text, text, text, text)
	}
	return where, args
}

const requestLogColumns = "id, timestamp, COALESCE(trace_id, ''), COALESCE(session_id, ''), COALESCE(provider, ''), COALESCE(model, ''), COALESCE(api_key_id, ''), COALESCE(status, ''), COALESCE(input_tokens, 0), COALESCE(output_tokens, 0), COALESCE(total_tokens, 0), COALESCE(latency_ms, 0), COALESCE(cost, 0), %s, %s, COALESCE(decision, ''), COALESCE(decision_reason, ''), COALESCE(error_message, ''), COALESCE(cached, 0), COALESCE(NULLIF(metadata, ''), '{}'), COALESCE(endpoint, ''), COALESCE(cost_source, 'none'), COALESCE(cached_tokens, 0), COALESCE(reasoning_tokens, 0)"

type rowScanner interface {
	Scan(...any) error
}

func scanRequestLog(row rowScanner) (RequestLog, error) {
	var entry RequestLog
	var timestamp string
	var cached int
	err := row.Scan(&entry.ID, &timestamp, &entry.TraceID, &entry.SessionID, &entry.Provider, &entry.Model, &entry.APIKeyID,
		&entry.Status, &entry.InputTokens, &entry.OutputTokens, &entry.TotalTokens, &entry.LatencyMs, &entry.Cost,
		&entry.RequestBody, &entry.ResponseBody, &entry.Decision, &entry.DecisionReason, &entry.ErrorMessage, &cached,
		&entry.Metadata, &entry.Endpoint, &entry.CostSource, &entry.CachedTokens, &entry.ReasoningTokens)
	if err != nil {
		return entry, err
	}
	entry.Timestamp, err = time.Parse(time.RFC3339Nano, timestamp)
	entry.Timestamp = entry.Timestamp.UTC()
	entry.Cached = cached == 1
	return entry, err
}

// ListLogs returns indexed, filtered request logs and the matching total.
func (s *Store) ListLogs(ctx context.Context, filter LogFilter) ([]RequestLog, int, error) {
	filter = filter.normalized()
	where, args := s.logWhere(filter)
	var total int
	if err := s.queryRow(ctx, "SELECT COUNT(*) FROM request_logs WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	order := timestampOrderExpression("timestamp") + " DESC, id DESC"
	switch filter.Sort {
	case "", "time_desc":
	case "time_asc":
		order = timestampOrderExpression("timestamp") + " ASC, id ASC"
	case "latency_desc":
		order = "latency_ms DESC, " + order
	default:
		return nil, 0, fmt.Errorf("unsupported sort %q", filter.Sort)
	}
	requestBody, responseBody := "COALESCE(request_body, '')", "COALESCE(response_body, '')"
	if filter.OmitPayloads {
		requestBody, responseBody = "''", "''"
	}
	query := "SELECT " + fmt.Sprintf(requestLogColumns, requestBody, responseBody) + " FROM request_logs WHERE " + where + " ORDER BY " + order + " LIMIT ? OFFSET ?"
	rows, err := s.query(ctx, query, append(args, filter.Limit, filter.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	logs := []RequestLog{}
	for rows.Next() {
		entry, err := scanRequestLog(rows)
		if err != nil {
			return nil, 0, err
		}
		logs = append(logs, entry)
	}
	return logs, total, rows.Err()
}

// ListLogsForAPIKey retains the scoped internal convenience API.
func (s *Store) ListLogsForAPIKey(ctx context.Context, apiKeyID string, limit, offset int, provider, status string) ([]RequestLog, int, error) {
	filter := LogFilter{APIKeyID: apiKeyID, Limit: limit, Offset: offset, Provider: provider}
	if status != "" {
		filter.Statuses = []string{status}
	}
	return s.ListLogs(ctx, filter)
}

// LogFacetValues are distinct values present in the caller's time/key scope.
type LogFacetValues struct {
	Providers []string `json:"providers"`
	Models    []string `json:"models"`
	Statuses  []string `json:"statuses"`
	Endpoints []string `json:"endpoints"`
}

func (s *Store) LogFacets(ctx context.Context, filter LogFilter) (LogFacetValues, error) {
	out := LogFacetValues{Providers: []string{}, Models: []string{}, Statuses: []string{}, Endpoints: []string{}}
	filter = filter.normalized()
	where, args := s.logWhere(filter)
	for _, facet := range []struct {
		column string
		values *[]string
	}{{"provider", &out.Providers}, {"model", &out.Models}, {"status", &out.Statuses}, {"endpoint", &out.Endpoints}} {
		rows, err := s.query(ctx, "SELECT DISTINCT "+facet.column+" FROM request_logs WHERE "+where+" AND "+facet.column+" <> '' ORDER BY "+facet.column+" LIMIT 200", args...)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				rows.Close()
				return out, err
			}
			*facet.values = append(*facet.values, value)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// MetricsBucket is an observed traffic bucket. Empty buckets are filled in Go,
// rather than depending on generate_series (which SQLite does not have).
type MetricsBucket struct {
	Start        time.Time `json:"start"`
	Requests     int       `json:"requests"`
	Allowed      int       `json:"allowed"`
	Denied       int       `json:"denied"`
	Limited      int       `json:"limited"`
	Errors       int       `json:"errors"`
	CacheHits    int       `json:"cache_hits"`
	Tokens       int       `json:"tokens"`
	Cost         float64   `json:"cost"`
	AvgLatencyMs float64   `json:"avg_latency_ms"`
}

func (s *Store) MetricsTimeseries(ctx context.Context, since, until time.Time, bucket time.Duration, apiKeyID string) ([]MetricsBucket, error) {
	if bucket < time.Second || bucket%time.Second != 0 || !since.Before(until) {
		return nil, fmt.Errorf("bucket must be whole seconds and time range must be positive")
	}
	step := int64(bucket / time.Second)
	first := since.Unix() / step
	last := until.Add(-time.Nanosecond).Unix() / step
	count := last - first + 1
	if count < 1 || count > 200 {
		return nil, fmt.Errorf("bucket must yield no more than 200 buckets")
	}
	buckets := make([]MetricsBucket, int(count))
	for i := range buckets {
		buckets[i].Start = time.Unix((first+int64(i))*step, 0).UTC()
	}
	where, args := s.indexedTimeRange("timestamp", since, until)
	if apiKeyID != "" {
		where += " AND api_key_id = ?"
		args = append(args, apiKeyID)
	}
	expr := fmt.Sprintf("CAST(CAST(strftime('%%s', timestamp) AS INTEGER) / %d AS INTEGER)", step)
	if s.pg {
		expr = fmt.Sprintf("CAST(FLOOR(EXTRACT(EPOCH FROM timestamp::timestamptz) / %d) AS BIGINT)", step)
	}
	rows, err := s.query(ctx, fmt.Sprintf(`SELECT %s AS bucket, COUNT(*),
		COALESCE(SUM(CASE WHEN status = 'success' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = 'denied' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = 'limited' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = 'error' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN cached = 1 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost), 0), COALESCE(AVG(latency_ms), 0)
		FROM request_logs WHERE %s GROUP BY bucket ORDER BY bucket LIMIT 200`, expr, where), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var number int64
		var row MetricsBucket
		if err := rows.Scan(&number, &row.Requests, &row.Allowed, &row.Denied, &row.Limited, &row.Errors, &row.CacheHits, &row.Tokens, &row.Cost, &row.AvgLatencyMs); err != nil {
			return nil, err
		}
		index := number - first
		if index >= 0 && index < count {
			row.Start = buckets[int(index)].Start
			buckets[int(index)] = row
		}
	}
	return buckets, rows.Err()
}

// LatencyStats uses nearest-rank percentiles over request latency, including
// denied/limited/error requests. An empty window returns numeric zeroes.
type LatencyStats struct {
	Count int     `json:"count"`
	AvgMs float64 `json:"avg_ms"`
	P50Ms float64 `json:"p50_ms"`
	P95Ms float64 `json:"p95_ms"`
	P99Ms float64 `json:"p99_ms"`
}

func (s *Store) LatencyPercentiles(ctx context.Context, since, until time.Time, apiKeyID string) (LatencyStats, error) {
	var out LatencyStats
	where, args := s.indexedTimeRange("timestamp", since, until)
	if apiKeyID != "" {
		where += " AND api_key_id = ?"
		args = append(args, apiKeyID)
	}
	if err := s.queryRow(ctx, "SELECT COUNT(*), COALESCE(AVG(latency_ms), 0) FROM request_logs WHERE "+where, args...).Scan(&out.Count, &out.AvgMs); err != nil || out.Count == 0 {
		return out, err
	}
	for _, percentile := range []struct {
		fraction float64
		value    *float64
	}{{0.50, &out.P50Ms}, {0.95, &out.P95Ms}, {0.99, &out.P99Ms}} {
		offset := int(math.Ceil(float64(out.Count)*percentile.fraction)) - 1
		if err := s.queryRow(ctx, "SELECT COALESCE(latency_ms, 0) FROM request_logs WHERE "+where+" ORDER BY COALESCE(latency_ms, 0) ASC LIMIT 1 OFFSET ?", append(args, offset)...).Scan(percentile.value); err != nil {
			return out, err
		}
	}
	return out, nil
}

// GuardrailFilter selects a bounded, joined decision feed. RequestIDs is used
// by trace detail to avoid exposing events belonging to another key's logs.
type GuardrailFilter struct {
	Since, Until           time.Time
	APIKeyID, Type, Action string
	TraceID                string
	RequestIDs             []string
	Limit, Offset          int
}

type GuardrailEventRow struct {
	GuardrailEvent
	TraceID  string `json:"trace_id"`
	Model    string `json:"model"`
	Provider string `json:"provider"`
	APIKeyID string `json:"api_key_id"`
}

func (s *Store) guardrailWhere(filter GuardrailFilter) (string, []any) {
	bounds := (LogFilter{Since: filter.Since, Until: filter.Until}).normalized()
	where, args := s.indexedTimeRange("g.timestamp", bounds.Since, bounds.Until)
	for _, pair := range [][2]string{{"l.api_key_id", filter.APIKeyID}, {"g.guardrail_type", filter.Type}, {"g.action", filter.Action}, {"l.trace_id", filter.TraceID}} {
		if pair[1] != "" {
			where += " AND " + pair[0] + " = ?"
			args = append(args, pair[1])
		}
	}
	if len(filter.RequestIDs) > 0 {
		where += " AND g.request_id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(filter.RequestIDs)), ",") + ")"
		for _, id := range filter.RequestIDs {
			args = append(args, id)
		}
	}
	return where, args
}

func (s *Store) ListGuardrailEvents(ctx context.Context, filter GuardrailFilter) ([]GuardrailEventRow, int, error) {
	where, args := s.guardrailWhere(filter)
	join := " FROM guardrail_events g JOIN request_logs l ON l.id = g.request_id WHERE " + where
	var total int
	if err := s.queryRow(ctx, "SELECT COUNT(*)"+join, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	page := (LogFilter{Limit: filter.Limit, Offset: filter.Offset}).normalized()
	if page.Limit > 100 {
		page.Limit = 100
	}
	// SQL truncation bounds the data returned by the driver, even for very
	// large webhook details. The API additionally redacts sensitive patterns.
	rows, err := s.query(ctx, `SELECT g.id, g.request_id, g.timestamp, COALESCE(g.guardrail_type, ''), COALESCE(g.action, ''), COALESCE(g.matched, 0),
		substr(COALESCE(g.details, ''), 1, 256), COALESCE(l.trace_id, ''), COALESCE(l.model, ''), COALESCE(l.provider, ''), COALESCE(l.api_key_id, '')`+join+
		" ORDER BY "+timestampOrderExpression("g.timestamp")+" DESC, g.id DESC LIMIT ? OFFSET ?", append(args, page.Limit, page.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	events := []GuardrailEventRow{}
	for rows.Next() {
		var event GuardrailEventRow
		var timestamp string
		var matched int
		if err := rows.Scan(&event.ID, &event.RequestID, &timestamp, &event.Type, &event.Action, &matched, &event.Details, &event.TraceID, &event.Model, &event.Provider, &event.APIKeyID); err != nil {
			return nil, 0, err
		}
		event.Timestamp, err = time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			return nil, 0, err
		}
		event.Timestamp = event.Timestamp.UTC()
		event.Matched = matched == 1
		events = append(events, event)
	}
	return events, total, rows.Err()
}

// GuardrailCounts includes every supported action, even when its count is zero.
type GuardrailCounts struct {
	Matched  int                  `json:"matched"`
	ByAction map[string]int       `json:"by_action"`
	ByType   []GuardrailTypeCount `json:"by_type"`
}

type GuardrailTypeCount struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

func (s *Store) GuardrailSummary(ctx context.Context, filter GuardrailFilter) (GuardrailCounts, error) {
	out := GuardrailCounts{
		ByAction: map[string]int{"deny": 0, "redact": 0, "warn": 0, "log": 0, "fallback": 0, "reroute": 0, "retry": 0},
		ByType:   []GuardrailTypeCount{},
	}
	where, args := s.guardrailWhere(filter)
	join := " FROM guardrail_events g JOIN request_logs l ON l.id = g.request_id WHERE " + where + " AND g.matched = 1"
	actions := []string{"deny", "redact", "warn", "log", "fallback", "reroute", "retry"}
	selects := "SELECT COUNT(*)"
	counts := make([]int, len(actions))
	destinations := []any{&out.Matched}
	for i, action := range actions {
		selects += ", COALESCE(SUM(CASE WHEN g.action = '" + action + "' THEN 1 ELSE 0 END), 0)"
		destinations = append(destinations, &counts[i])
	}
	if err := s.queryRow(ctx, selects+join, args...).Scan(destinations...); err != nil {
		return out, err
	}
	for i, action := range actions {
		out.ByAction[action] = counts[i]
	}
	rows, err := s.query(ctx, "SELECT g.guardrail_type, COUNT(*)"+join+" GROUP BY g.guardrail_type ORDER BY COUNT(*) DESC, g.guardrail_type ASC LIMIT 200", args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var row GuardrailTypeCount
		if err := rows.Scan(&row.Type, &row.Count); err != nil {
			return out, err
		}
		out.ByType = append(out.ByType, row)
	}
	return out, rows.Err()
}

// MCPDecisionRow is a metadata-only audit record. Older rows with no MCP
// metadata keep empty fields rather than guessing a principal or rule.
type MCPDecisionRow struct {
	Timestamp  time.Time `json:"timestamp"`
	DecisionID string    `json:"decision_id"`
	AgentID    string    `json:"agent_id"`
	Tool       string    `json:"tool"`
	Action     string    `json:"action"`
	Target     string    `json:"target"`
	Decision   string    `json:"decision"`
	Rule       string    `json:"rule"`
	Reason     string    `json:"reason"`
	LatencyMs  float64   `json:"latency_ms"`
	TraceID    string    `json:"trace_id"`
}

func (s *Store) ListMCPDecisions(ctx context.Context, filter LogFilter) ([]MCPDecisionRow, int, error) {
	filter.Provider = "mcp"
	filter.OmitPayloads = true
	if filter.Limit > 100 {
		filter.Limit = 100
	}
	logs, total, err := s.ListLogs(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	decisions := make([]MCPDecisionRow, 0, len(logs))
	for _, log := range logs {
		var metadata struct {
			MCP struct {
				AgentID   string  `json:"agent_id"`
				Action    string  `json:"action"`
				Target    string  `json:"target"`
				Rule      string  `json:"rule"`
				LatencyMs float64 `json:"latency_ms"`
			} `json:"mcp"`
		}
		_ = json.Unmarshal([]byte(log.Metadata), &metadata)
		decisions = append(decisions, MCPDecisionRow{
			Timestamp: log.Timestamp, DecisionID: log.ID, AgentID: metadata.MCP.AgentID,
			Tool: log.Model, Action: metadata.MCP.Action, Target: metadata.MCP.Target,
			Decision: log.Decision, Rule: metadata.MCP.Rule, Reason: log.DecisionReason,
			LatencyMs: metadata.MCP.LatencyMs, TraceID: log.TraceID,
		})
	}
	return decisions, total, nil
}
