package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/omniswitch-dev/omniswitch/internal/dashboard"
	"github.com/omniswitch-dev/omniswitch/internal/guardrail"
	"github.com/omniswitch-dev/omniswitch/internal/provider"
	"github.com/omniswitch-dev/omniswitch/internal/router"
	"github.com/omniswitch-dev/omniswitch/internal/telemetry"
)

const metricsChatBody = `{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`

func assertMetricSamples(t *testing.T, metrics *telemetry.Metrics, expected ...string) {
	t.Helper()
	var text strings.Builder
	if err := metrics.WritePrometheus(&text); err != nil {
		t.Fatal(err)
	}
	for _, want := range expected {
		if !strings.Contains(text.String(), want+"\n") {
			t.Errorf("missing metric sample %s in:\n%s", want, text.String())
		}
	}
}

// TestPrometheusScrape exercises real request handlers and /metrics without a
// log store. CI can persist this actual handler output for promtool validation.
func TestPrometheusScrape(t *testing.T) {
	metrics := telemetry.NewMetrics()
	metrics.SetBuildInfo("v0.2.0", "test-commit")
	registry := provider.NewRegistry()
	registry.Register(metricGatewayProvider{gatewayProvider: gatewayProvider{name: "test", model: "test-model"}})
	registry.Register(embeddingGatewayProvider{})
	registry.Register(rerankGatewayProvider{})
	h := New(registry, router.New(registry), nil, nil)
	h.SetMetrics(metrics)
	mux := http.NewServeMux()
	for _, endpoint := range []struct {
		path string
		body string
		fn   http.HandlerFunc
	}{
		{path: "/v1/chat/completions", body: metricsChatBody, fn: h.ChatCompletions},
		{path: "/v1/responses", body: `{"model":"test-model","input":"hello"}`, fn: h.Responses},
		{path: "/v1/messages", body: metricsChatBody, fn: h.Messages},
		{path: "/v1/embeddings", body: `{"model":"embedding-model","input":"hello"}`, fn: h.Embeddings},
		{path: "/v1/moderations", body: `{"input":"hello"}`, fn: h.Moderations},
		{path: "/v1/rerank", body: `{"model":"rerank-model","query":"hello","documents":["world"]}`, fn: h.Rerank},
	} {
		mux.HandleFunc(endpoint.path, endpoint.fn)
		rec := httptest.NewRecorder()
		h.MetricsMiddleware(mux).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, endpoint.path, strings.NewReader(endpoint.body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s returned %d: %s", endpoint.path, rec.Code, rec.Body.String())
		}
	}
	// Exercise native chat streaming as well as all blocking inference APIs.
	body := strings.TrimSuffix(metricsChatBody, "}") + `,"stream":true}`
	stream := httptest.NewRecorder()
	h.MetricsMiddleware(mux).ServeHTTP(stream, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if stream.Code != http.StatusOK || !strings.Contains(stream.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream returned %d: %s", stream.Code, stream.Body.String())
	}
	// Use the production event path and the two integrator-owned hooks so all
	// families, including escaped label values, have samples in the scrape.
	h.recordGuardrailResults(context.Background(), "", []guardrail.Result{{Triggered: true, Type: "pii", Action: "redact"}})
	metrics.RecordRateLimitRejection("policy\"with\nnewline\\slash")
	metrics.RegisterCircuitStateSource(func() map[string]int { return map[string]int{"test": 0, "half-open": 1, "open": 2} })

	for _, endpoint := range []string{"chat_completions", "responses", "messages"} {
		count := 1
		if endpoint == "chat_completions" {
			count = 2
		}
		assertMetricSamples(t, metrics,
			fmt.Sprintf(`omniswitch_requests_total{endpoint="%s",provider="test",model="test-model",status="success"} %d`, endpoint, count),
			fmt.Sprintf(`omniswitch_request_duration_seconds_count{endpoint="%s",provider="test"} %d`, endpoint, count),
		)
	}
	assertMetricSamples(t, metrics,
		`omniswitch_requests_total{endpoint="embeddings",provider="embeddings",model="embedding-model",status="success"} 1`,
		`omniswitch_requests_total{endpoint="moderations",provider="omniswitch",model="omniswitch-guardrails",status="success"} 1`,
		`omniswitch_requests_total{endpoint="rerank",provider="rerank",model="rerank-model",status="success"} 1`,
		`omniswitch_tokens_total{provider="test",model="test-model",direction="input"} 8`,
		`omniswitch_tokens_total{provider="test",model="test-model",direction="output"} 12`,
		`omniswitch_tokens_total{provider="embeddings",model="embedding-model",direction="input"} 1`,
		`omniswitch_tokens_total{provider="rerank",model="rerank-model",direction="input"} 3`,
		`omniswitch_cost_usd_total{provider="test",model="test-model"} 0.375`,
		`omniswitch_guardrail_events_total{type="pii",action="redact"} 1`,
	)
	dash := dashboard.New(nil)
	dash.SetMetrics(metrics)
	dash.RegisterPrometheus(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "version=0.0.4") {
		t.Fatal("dashboard /metrics did not expose the registry")
	}
	if path := os.Getenv("OMNISWITCH_METRICS_OUTPUT"); path != "" {
		if err := os.WriteFile(path, rec.Body.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMetricRequestsIncludeEarlyEndpointFailures(t *testing.T) {
	metrics := telemetry.NewMetrics()
	registry := provider.NewRegistry()
	h := New(registry, router.New(registry), nil, nil)
	h.SetMetrics(metrics)
	for _, endpoint := range []struct {
		path string
		name string
		fn   http.HandlerFunc
	}{
		{path: "/v1/chat/completions", name: "chat_completions", fn: h.ChatCompletions},
		{path: "/v1/responses", name: "responses", fn: h.Responses},
		{path: "/v1/messages", name: "messages", fn: h.Messages},
		{path: "/v1/embeddings", name: "embeddings", fn: h.Embeddings},
		{path: "/v1/moderations", name: "moderations", fn: h.Moderations},
		{path: "/v1/rerank", name: "rerank", fn: h.Rerank},
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			rec := httptest.NewRecorder()
			endpoint.fn(rec, httptest.NewRequest(method, endpoint.path, strings.NewReader("{")))
			if rec.Code != http.StatusBadRequest && rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s failure returned %d", endpoint.path, rec.Code)
			}
		}
		assertMetricSamples(t, metrics, fmt.Sprintf(`omniswitch_requests_total{endpoint="%s",provider="",model="",status="error"} 2`, endpoint.name))
	}
}

func TestMetricsCacheDoesNotCountUpstreamUsageTwice(t *testing.T) {
	metrics := telemetry.NewMetrics()
	registry := provider.NewRegistry()
	registry.Register(metricGatewayProvider{gatewayProvider: gatewayProvider{name: "test", model: "test-model"}})
	h := New(registry, router.New(registry), newGatewayTestStore(t), nil)
	h.SetMetrics(metrics)
	for attempt := 0; attempt < 2; attempt++ {
		rec := httptest.NewRecorder()
		h.ChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(metricsChatBody)))
		if rec.Code != http.StatusOK {
			t.Fatal(rec.Body.String())
		}
	}
	assertMetricSamples(t, metrics,
		`omniswitch_requests_total{endpoint="chat_completions",provider="test",model="test-model",status="success"} 1`,
		`omniswitch_requests_total{endpoint="chat_completions",provider="semantic-cache",model="test-model",status="success"} 1`,
		`omniswitch_tokens_total{provider="test",model="test-model",direction="input"} 2`,
		`omniswitch_tokens_total{provider="test",model="test-model",direction="output"} 3`,
		`omniswitch_cost_usd_total{provider="test",model="test-model"} 0.125`,
	)
	var text strings.Builder
	if err := metrics.WritePrometheus(&text); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text.String(), `omniswitch_tokens_total{provider="semantic-cache"`) {
		t.Fatal("cache hits must not record upstream usage")
	}
}

func TestMetricsGuardrailAttemptsCountUsageButOneRequest(t *testing.T) {
	for _, action := range []string{"retry", "deny"} {
		t.Run(action, func(t *testing.T) {
			gr := guardrail.NewEngineWithConfig(guardrail.Config{Rules: []guardrail.Rule{{Name: "output-rule", Stage: "output", Pattern: "forbidden", Action: action, MaxRetries: 1}}})
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
					metrics := telemetry.NewMetrics()
					registry := provider.NewRegistry()
					calls := 0
					registry.Register(metricGatewayProvider{gatewayProvider: gatewayProvider{name: "test", model: "test-model", calls: &calls}, forbiddenFirst: true})
					h := New(registry, router.New(registry), nil, gr)
					h.SetMetrics(metrics)
					body := strings.TrimSuffix(metricsChatBody, "}") + fmt.Sprintf(`,"stream":%v}`, stream)
					rec := httptest.NewRecorder()
					h.ChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
					status, attempts, httpStatus := "success", 2, http.StatusOK
					if action == "deny" {
						status, attempts, httpStatus = "denied", 1, http.StatusForbidden
					}
					if rec.Code != httpStatus || calls != attempts {
						t.Fatalf("status/calls = %d/%d, want %d/%d", rec.Code, calls, httpStatus, attempts)
					}
					assertMetricSamples(t, metrics,
						fmt.Sprintf(`omniswitch_requests_total{endpoint="chat_completions",provider="test",model="test-model",status="%s"} 1`, status),
						`omniswitch_request_duration_seconds_count{endpoint="chat_completions",provider="test"} 1`,
						fmt.Sprintf(`omniswitch_tokens_total{provider="test",model="test-model",direction="input"} %d`, 2*attempts),
						fmt.Sprintf(`omniswitch_tokens_total{provider="test",model="test-model",direction="output"} %d`, 3*attempts),
						fmt.Sprintf(`omniswitch_guardrail_events_total{type="output-rule",action="%s"} 1`, action),
					)
				})
			}
		})
	}
}

func TestMetricHTTPWriterPreservesFlusherSupport(t *testing.T) {
	metrics := telemetry.NewMetrics()
	registry := provider.NewRegistry()
	h := New(registry, router.New(registry), nil, nil)
	h.SetMetrics(metrics)
	rec := &bufferedResponseWriter{header: make(http.Header)} // intentionally no http.Flusher
	h.ChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(strings.TrimSuffix(metricsChatBody, "}")+`,"stream":true}`)))
	if rec.status != http.StatusInternalServerError {
		t.Fatal("metrics wrapper falsely advertised http.Flusher")
	}
	assertMetricSamples(t, metrics, `omniswitch_requests_total{endpoint="chat_completions",provider="",model="test-model",status="error"} 1`)
}

func TestMetricsDisabledAndUntrackedPaths(t *testing.T) {
	metrics := telemetry.NewMetrics()
	registry := provider.NewRegistry()
	registry.Register(metricGatewayProvider{gatewayProvider: gatewayProvider{name: "test", model: "test-model"}})
	h := New(registry, router.New(registry), nil, nil)
	h.SetMetrics(metrics)
	rec := httptest.NewRecorder()
	h.ChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/a2a", strings.NewReader(metricsChatBody)))
	if rec.Code != http.StatusOK {
		t.Fatal("untracked path changed handler behavior")
	}
	h.SetMetrics(nil)
	h.ChatCompletions(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(metricsChatBody)))
	h.recordGuardrailResults(context.Background(), "", []guardrail.Result{{Triggered: true, Type: "pii", Action: "deny"}})
	var text strings.Builder
	if err := metrics.WritePrometheus(&text); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text.String(), "omniswitch_requests_total{") || strings.Contains(text.String(), "omniswitch_tokens_total{") || strings.Contains(text.String(), "omniswitch_guardrail_events_total{") {
		t.Fatal("disabled or untracked handlers must not record request/usage/event samples")
	}
}

func TestMetricsConcurrentHTTPRequests(t *testing.T) {
	metrics := telemetry.NewMetrics()
	registry := provider.NewRegistry()
	registry.Register(metricGatewayProvider{gatewayProvider: gatewayProvider{name: "test", model: "test-model"}})
	h := New(registry, router.New(registry), nil, nil)
	h.SetMetrics(metrics)
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for iteration := 0; iteration < 10; iteration++ {
				rec := httptest.NewRecorder()
				h.ChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(metricsChatBody)))
				if rec.Code != http.StatusOK {
					t.Error(rec.Body.String())
				}
			}
		}()
	}
	workers.Wait()
	assertMetricSamples(t, metrics,
		`omniswitch_requests_total{endpoint="chat_completions",provider="test",model="test-model",status="success"} 80`,
		`omniswitch_request_duration_seconds_count{endpoint="chat_completions",provider="test"} 80`,
		`omniswitch_tokens_total{provider="test",model="test-model",direction="input"} 160`,
		`omniswitch_cost_usd_total{provider="test",model="test-model"} 10`,
	)
}

type metricGatewayProvider struct {
	gatewayProvider
	forbiddenFirst bool
}

func (p metricGatewayProvider) ChatCompletion(ctx context.Context, req provider.ChatRequest) (provider.ChatResponse, provider.ProviderMeta, error) {
	resp, meta, err := p.gatewayProvider.ChatCompletion(ctx, req)
	meta.Cost = 0.125
	if p.forbiddenFirst && p.calls != nil && *p.calls == 1 {
		resp.Choices[0].Message.Content = "forbidden output"
	}
	return resp, meta, err
}
