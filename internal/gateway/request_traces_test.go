package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/guardrail"
	"github.com/omniswitch-dev/omniswitch/internal/provider"
	"github.com/omniswitch-dev/omniswitch/internal/router"
	"github.com/omniswitch-dev/omniswitch/internal/store"
)

type measuredTraceProvider struct {
	gatewayProvider
	fail bool
}

func (p measuredTraceProvider) ChatCompletion(ctx context.Context, req provider.ChatRequest) (provider.ChatResponse, provider.ProviderMeta, error) {
	time.Sleep(2 * time.Millisecond)
	if p.fail {
		return provider.ChatResponse{}, provider.ProviderMeta{Provider: p.Name(), Latency: 24 * time.Hour}, fmt.Errorf("upstream status 503")
	}
	return p.gatewayProvider.ChatCompletion(ctx, req)
}

func TestPersistedRequestSpansAreMeasuredAndOrdered(t *testing.T) {
	st := newGatewayTestStore(t)
	secret := "trace-test-secret"
	sum := sha256.Sum256([]byte(secret))
	if err := st.InsertAPIKey(context.Background(), store.APIKey{ID: "workload", Name: "workload", KeyHash: hex.EncodeToString(sum[:]), CreatedAt: time.Now().UTC(), Enabled: true, Role: "member"}); err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry()
	registry.Register(measuredTraceProvider{gatewayProvider: gatewayProvider{name: "primary", model: "model"}, fail: true})
	registry.Register(measuredTraceProvider{gatewayProvider: gatewayProvider{name: "fallback", model: "model", content: "safe"}})
	rtr := router.New(registry)
	rtr.SetRoute("model", router.Route{Provider: "primary", Fallbacks: []string{"fallback"}, MaxRetries: 1, RetryBackoff: "2ms"})
	h := New(registry, rtr, st, guardrail.NewEngine())
	handler := NewAuthMiddleware(st, true).Wrap(NewAuthorizationMiddleware(true).Wrap(http.HandlerFunc(h.ChatCompletions)))
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[{"role":"user","content":"hello"}]}`))
	r.Header.Set("Authorization", "Bearer "+secret)
	r.Header.Set("x-omniswitch-trace-id", "ordered-trace")
	r.Header.Set("x-omniswitch-metadata", `{"marker":"preserved","_spans":[{"name":"fabricated","duration_ms":999999}]}`)
	started := time.Now()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	finished := time.Now()
	if w.Code != 200 {
		t.Fatalf("request=%d/%s", w.Code, w.Body.String())
	}
	logs, _, err := st.ListLogs(context.Background(), store.LogFilter{Since: started.Add(-time.Second), Until: finished.Add(time.Second), APIKeyID: "workload", TraceID: "ordered-trace"})
	if err != nil || len(logs) != 1 {
		t.Fatalf("logs=%v/%v", logs, err)
	}
	var metadata struct {
		Marker string            `json:"marker"`
		Spans  []store.TraceSpan `json:"_spans"`
	}
	if err := json.Unmarshal([]byte(logs[0].Metadata), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Marker != "preserved" {
		t.Fatal("caller metadata discarded")
	}
	seen := map[string]int{}
	attempts := []int{}
	for i, span := range metadata.Spans {
		seen[span.Name]++
		if span.Name == "fabricated" || span.Start.Before(started) || span.Start.After(finished) || span.DurationMs < 0 || span.DurationMs > float64(finished.Sub(started))/float64(time.Millisecond)+5 || span.Start.Location() != time.UTC {
			t.Fatalf("fabricated timing: %+v", span)
		}
		if i > 0 && span.Start.Before(metadata.Spans[i-1].Start) {
			t.Fatal("unordered persisted spans")
		}
		if span.Name == "provider_attempt" {
			attempts = append(attempts, int(span.Metadata["attempt"].(float64)))
			if span.DurationMs < 1 {
				t.Fatalf("attempt duration did not include real I/O: %+v", span)
			}
			if span.Provider == "primary" && (span.Status != "error" || span.Metadata["http_status"] != float64(503)) {
				t.Fatalf("attempt metadata=%+v", span)
			}
		}
	}
	for _, name := range []string{"auth", "guardrail_input", "route", "provider_attempt", "retry_backoff", "guardrail_fallback", "cache", "guardrail_output"} {
		if seen[name] == 0 {
			t.Fatalf("missing %s in %v", name, seen)
		}
	}
	if seen["provider_attempt"] != 3 || fmt.Sprint(attempts) != "[1 2 1]" {
		t.Fatalf("attempts=%v/%v", seen, attempts)
	}
}

func TestPersistedStreamingAttemptFinishesBeforeLog(t *testing.T) {
	st := newGatewayTestStore(t)
	registry := provider.NewRegistry()
	registry.Register(streamCostProvider{})
	h := New(registry, router.New(registry), st, guardrail.NewEngine())
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"stream":true}`))
	r.Header.Set("x-omniswitch-metadata", "null")
	NewAuthMiddleware(st, false).Wrap(http.HandlerFunc(h.ChatCompletions)).ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("stream=%d/%s", w.Code, w.Body.String())
	}
	logs, _, err := st.ListLogs(context.Background(), store.LogFilter{Limit: 10})
	if err != nil || len(logs) != 1 {
		t.Fatalf("stream logs=%v/%v", logs, err)
	}
	var metadata struct {
		Spans []store.TraceSpan `json:"_spans"`
	}
	if err := json.Unmarshal([]byte(logs[0].Metadata), &metadata); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, span := range metadata.Spans {
		seen[span.Name] = true
		if span.Name == "provider_attempt" && (span.Status != "ok" || span.Metadata["stream"] != true) {
			t.Fatalf("stream attempt=%+v", span)
		}
	}
	if !seen["auth"] || !seen["provider_attempt"] || !seen["guardrail_output"] {
		t.Fatalf("incomplete persisted streaming spans=%v", seen)
	}
}

func TestRecorderBoundAndUnknownOIDCScope(t *testing.T) {
	clean := metadataWithSpans(`{"_spans":[{"name":"fabricated"}],"_spans_truncated":true}`, nil, false)
	if strings.Contains(clean, "_spans") {
		t.Fatal("caller timings survived an empty measured recorder")
	}
	ctx := WithTraceRecorder(context.Background(), newTraceRecorder())
	for i := 0; i < 300; i++ {
		recordTraceSpan(ctx, "attempt", "p", "ok", time.Now(), nil)
	}
	if len(spansFromContext(ctx)) != 256 || !traceSpansTruncated(ctx) {
		t.Fatal("recorder is unbounded or hides truncation")
	}
	metadata := metadataWithSpans("null", spansFromContext(ctx), traceSpansTruncated(ctx))
	if !strings.Contains(metadata, `"_spans_truncated":true`) {
		t.Fatal("truncation not persisted")
	}
	for _, identity := range []Identity{{Role: "viewer", AuthMethod: "oidc"}, {Role: "member", Subject: "subject", AuthMethod: "oidc"}} {
		r := httptest.NewRequest(http.MethodGet, "/api/logs", nil).WithContext(withIdentity(context.Background(), identity))
		r.Header.Set("x-omniswitch-role", "owner")
		r.Header.Set("x-omniswitch-key-id", "other")
		if IsAdmin(r) || ScopedAPIKeyID(r) == "" || ScopedAPIKeyID(r) == "other" {
			t.Fatal("unknown OIDC principal gained global or forged scope")
		}
	}
}
