package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// GetTraceInRange bounds every trace query by time and row count. Request-ID
// joins also isolate shared caller-supplied trace IDs between workload keys.
func (s *Store) GetTraceInRange(ctx context.Context, traceID, apiKeyID string, since, until time.Time) (TraceDetail, error) {
	detail := TraceDetail{TraceID: traceID, ShadowLogs: []ShadowLog{}, Feedback: []Feedback{}, GuardrailEvents: []GuardrailEvent{}}
	logs, _, err := s.ListLogs(ctx, LogFilter{TraceID: traceID, APIKeyID: apiKeyID, Since: since, Until: until, Sort: "time_asc", Limit: 500})
	if err != nil {
		return detail, err
	}
	detail.Logs = logs
	if len(logs) == 0 {
		return detail, sql.ErrNoRows
	}
	ids := make([]string, 0, len(logs))
	for _, entry := range logs {
		ids = append(ids, entry.ID)
	}
	events, _, err := s.ListGuardrailEvents(ctx, GuardrailFilter{
		Since: since, Until: until, APIKeyID: apiKeyID, TraceID: traceID, RequestIDs: ids, Limit: 100,
	})
	if err != nil {
		return detail, err
	}
	for _, event := range events {
		detail.GuardrailEvents = append(detail.GuardrailEvents, event.GuardrailEvent)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	where, args := s.indexedTimeRange("timestamp", since, until)
	where += " AND trace_id = ? AND request_id IN (" + placeholders + ")"
	args = append(args, traceID)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.query(ctx, `SELECT id, request_id, trace_id, timestamp, COALESCE(primary_provider, ''), COALESCE(shadow_provider, ''), COALESCE(model, ''), COALESCE(latency_ms, 0), COALESCE(cost, 0), COALESCE(status, ''), COALESCE(error_message, '') FROM shadow_requests WHERE `+where+` ORDER BY timestamp ASC, id ASC LIMIT 100`, args...)
	if err != nil {
		return detail, err
	}
	for rows.Next() {
		var entry ShadowLog
		var stamp string
		if err := rows.Scan(&entry.ID, &entry.RequestID, &entry.TraceID, &stamp, &entry.PrimaryProvider, &entry.ShadowProvider, &entry.Model, &entry.LatencyMs, &entry.Cost, &entry.Status, &entry.ErrorMessage); err != nil {
			_ = rows.Close()
			return detail, err
		}
		entry.Timestamp, err = time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			_ = rows.Close()
			return detail, err
		}
		entry.Timestamp = entry.Timestamp.UTC()
		detail.ShadowLogs = append(detail.ShadowLogs, entry)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return detail, err
	}
	if err := rows.Close(); err != nil {
		return detail, err
	}
	where, args = s.indexedTimeRange("timestamp", since, until)
	where += " AND trace_id = ? AND (request_id IN (" + placeholders + ")"
	args = append(args, traceID)
	for _, id := range ids {
		args = append(args, id)
	}
	if apiKeyID == "" {
		// Only administrators can see legacy feedback without a request ID;
		// a trace ID alone cannot establish a workload key's ownership.
		where += " OR COALESCE(request_id, '') = ''"
	}
	where += ")"
	rows, err = s.query(ctx, `SELECT id, COALESCE(request_id, ''), trace_id, timestamp, COALESCE(score, 0), COALESCE(rating, ''), COALESCE(comment, ''), COALESCE(user_id, ''), COALESCE(metadata, '{}') FROM feedback WHERE `+where+` ORDER BY timestamp ASC, id ASC LIMIT 100`, args...)
	if err != nil {
		return detail, err
	}
	defer rows.Close()
	for rows.Next() {
		var entry Feedback
		var stamp, metadata string
		if err := rows.Scan(&entry.ID, &entry.RequestID, &entry.TraceID, &stamp, &entry.Score, &entry.Rating, &entry.Comment, &entry.UserID, &metadata); err != nil {
			return detail, err
		}
		entry.Timestamp, err = time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			_ = rows.Close()
			return detail, err
		}
		entry.Timestamp = entry.Timestamp.UTC()
		_ = json.Unmarshal([]byte(metadata), &entry.Metadata)
		detail.Feedback = append(detail.Feedback, entry)
	}
	return detail, rows.Err()
}
