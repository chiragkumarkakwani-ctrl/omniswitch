package gateway

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/router"
	"github.com/omniswitch-dev/omniswitch/internal/store"
)

const (
	userIDContextKey       contextKey = "user_id"
	metadataJSONContextKey contextKey = "metadata_json"
	tagsContextKey         contextKey = "tags"
	traceRecorderKey       contextKey = "trace_recorder"
	authStartKey           contextKey = "auth_start"
)

func WithUserID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, userIDContextKey, strings.TrimSpace(id))
}

func UserIDFromContext(ctx context.Context) string {
	value, _ := ctx.Value(userIDContextKey).(string)
	return value
}

func WithMetadataJSON(ctx context.Context, metadata string) context.Context {
	return context.WithValue(ctx, metadataJSONContextKey, normalizedMetadataJSON(metadata))
}

func MetadataJSONFromContext(ctx context.Context) string {
	value, _ := ctx.Value(metadataJSONContextKey).(string)
	if strings.TrimSpace(value) == "" {
		return "{}"
	}
	return value
}

func WithTags(ctx context.Context, tags []string) context.Context {
	return context.WithValue(ctx, tagsContextKey, normalizedTags(tags))
}

func TagsFromContext(ctx context.Context) []string {
	tags, _ := ctx.Value(tagsContextKey).([]string)
	return append([]string(nil), tags...)
}

func tagsHeader(value string) []string {
	var tags []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			tags = append(tags, part)
		}
	}
	return tags
}

func normalizedTags(tags []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
	}
	return out
}

func normalizedMetadataJSON(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "{}"
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		fallback, _ := json.Marshal(map[string]any{"_invalid_metadata": value})
		return string(fallback)
	}
	if decoded == nil {
		return "{}"
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

type traceRecorder struct {
	mu        sync.Mutex
	spans     []store.TraceSpan
	truncated bool
}

func newTraceRecorder() *traceRecorder {
	return &traceRecorder{}
}

func WithTraceRecorder(ctx context.Context, recorder *traceRecorder) context.Context {
	ctx = context.WithValue(ctx, traceRecorderKey, recorder)
	return router.WithSpanRecorder(ctx, func(name, providerName, status string, start time.Time, metadata map[string]any) {
		recordTraceSpan(ctx, name, providerName, status, start, metadata)
	})
}

func TraceRecorderFromContext(ctx context.Context) *traceRecorder {
	recorder, _ := ctx.Value(traceRecorderKey).(*traceRecorder)
	return recorder
}

func recordTraceSpan(ctx context.Context, name, providerName, status string, start time.Time, metadata map[string]any) {
	recorder := TraceRecorderFromContext(ctx)
	if recorder == nil || name == "" {
		return
	}
	if status == "" {
		status = "ok"
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.spans) >= 256 {
		recorder.truncated = true
		return
	}
	recorder.spans = append(recorder.spans, store.TraceSpan{
		Name:       name,
		Provider:   providerName,
		Start:      start.UTC(),
		DurationMs: float64(time.Since(start)) / float64(time.Millisecond),
		Status:     status,
		Metadata:   metadata,
	})
}

func spansFromContext(ctx context.Context) []store.TraceSpan {
	recorder := TraceRecorderFromContext(ctx)
	if recorder == nil {
		return nil
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	spans := append([]store.TraceSpan(nil), recorder.spans...)
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].Start.Before(spans[j].Start) })
	return spans
}

func metadataWithSpans(metadataJSON string, spans []store.TraceSpan, truncated bool) string {
	var metadata map[string]any
	if err := json.Unmarshal([]byte(normalizedMetadataJSON(metadataJSON)), &metadata); err != nil {
		metadata = map[string]any{}
	}
	if len(spans) > 0 {
		metadata["_spans"] = spans
	}
	if truncated {
		metadata["_spans_truncated"] = true
	} else {
		delete(metadata, "_spans_truncated")
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return normalizedMetadataJSON(metadataJSON)
	}
	return string(encoded)
}

func traceSpansTruncated(ctx context.Context) bool {
	recorder := TraceRecorderFromContext(ctx)
	if recorder == nil {
		return false
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.truncated
}
