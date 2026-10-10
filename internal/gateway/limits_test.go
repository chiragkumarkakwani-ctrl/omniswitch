package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/guardrail"
	"github.com/omniswitch-dev/omniswitch/internal/limits"
	"github.com/omniswitch-dev/omniswitch/internal/provider"
	"github.com/omniswitch-dev/omniswitch/internal/router"
	"github.com/omniswitch-dev/omniswitch/internal/store"
)

type limitsProvider struct {
	gatewayProvider
	usage        provider.Usage
	calls        atomic.Int64
	includeUsage bool
	fail         bool
}

func (p *limitsProvider) ChatCompletion(ctx context.Context, request provider.ChatRequest) (provider.ChatResponse, provider.ProviderMeta, error) {
	p.calls.Add(1)
	response, meta, _ := p.gatewayProvider.ChatCompletion(ctx, request)
	response.Usage = p.usage
	if p.fail {
		return provider.ChatResponse{}, meta, errors.New("upstream unavailable")
	}
	return response, meta, nil
}

func (p *limitsProvider) ChatCompletionStream(_ context.Context, request provider.ChatRequest) (<-chan provider.ChatResponseChunk, provider.ProviderMeta, error) {
	p.calls.Add(1)
	p.includeUsage = request.StreamOptions != nil && request.StreamOptions.IncludeUsage
	chunks := make(chan provider.ChatResponseChunk, 2)
	chunks <- provider.ChatResponseChunk{ID: "chat_limits", Model: request.Model, Choices: []provider.ChunkChoice{{Index: 0, Delta: provider.Message{Content: "ok"}, FinishReason: "stop"}}}
	usage := p.usage
	chunks <- provider.ChatResponseChunk{ID: "chat_limits", Model: request.Model, Choices: []provider.ChunkChoice{}, Usage: &usage}
	close(chunks)
	return chunks, provider.ProviderMeta{Provider: p.Name(), Model: request.Model}, nil
}

func (p *limitsProvider) Embeddings(ctx context.Context, request provider.EmbeddingRequest) (provider.EmbeddingResponse, provider.ProviderMeta, error) {
	p.calls.Add(1)
	response, meta, err := (embeddingGatewayProvider{}).Embeddings(ctx, request)
	meta.Provider = p.Name()
	return response, meta, err
}

func (p *limitsProvider) Rerank(ctx context.Context, request provider.RerankRequest) (provider.RerankResponse, provider.ProviderMeta, error) {
	p.calls.Add(1)
	response, meta, err := (rerankGatewayProvider{}).Rerank(ctx, request)
	meta.Provider = p.Name()
	return response, meta, err
}

type limitsHarness struct {
	st       *store.Store
	gateway  *Handler
	enforcer *limits.Enforcer
	provider *limitsProvider
	key      store.APIKey
	now      time.Time
	handler  http.Handler
}

const limitsTestToken = "limits-fixture-token"

func newLimitsHarness(t *testing.T, key store.APIKey, usage provider.Usage, policies ...limits.Policy) *limitsHarness {
	t.Helper()
	h := &limitsHarness{st: newGatewayTestStore(t), now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	key.ID, key.Name, key.Enabled, key.Role = "limits-key", "limits test", true, "member"
	hash := sha256.Sum256([]byte(limitsTestToken))
	key.KeyHash = hex.EncodeToString(hash[:])
	key.CreatedAt = h.now
	h.key = key
	if err := h.st.InsertAPIKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry()
	h.provider = &limitsProvider{gatewayProvider: gatewayProvider{name: "openai", model: "gpt-4o-mini"}, usage: usage}
	registry.Register(h.provider)
	h.gateway = New(registry, router.New(registry), h.st, guardrail.NewEngine())
	h.gateway.SetSemanticCache(0)
	clock := func() time.Time { return h.now }
	var err error
	h.enforcer, err = limits.NewEnforcer(limits.NewMemoryBackendWithClock(clock), limits.Options{Requests: 1000, Window: time.Minute, Policies: policies, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	h.gateway.SetLimits(h.enforcer)
	limiter := &RateLimiter{enforcer: h.enforcer, store: h.st}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", h.gateway.ChatCompletions)
	mux.HandleFunc("/v1/responses", h.gateway.Responses)
	mux.HandleFunc("/v1/messages", h.gateway.Messages)
	mux.HandleFunc("/v1/embeddings", h.gateway.Embeddings)
	mux.HandleFunc("/v1/rerank", h.gateway.Rerank)
	h.handler = LoggingMiddleware(limiter.Headers(NewAuthMiddleware(h.st, true).Wrap(limiter.Wrap(mux))))
	return h
}

func (h *limitsHarness) request(path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+limitsTestToken)
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, request)
	return response
}

func limitsChatBody(t *testing.T, stream bool) string {
	t.Helper()
	maxTokens := 100
	body, err := json.Marshal(provider.ChatRequest{Model: "gpt-4o-mini", Stream: stream, MaxTokens: &maxTokens, Messages: []provider.Message{{Role: "user", Content: strings.Repeat("a", 1988)}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func requireLimitHeaders(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	for _, kind := range []string{"limit", "remaining", "reset"} {
		for _, unit := range []string{"requests", "tokens"} {
			if response.Result().Header.Get("x-ratelimit-"+kind+"-"+unit) == "" {
				t.Fatalf("missing %s/%s header on %d", kind, unit, response.Code)
			}
		}
	}
}

func (h *limitsHarness) snapshot(t *testing.T, name string) limits.Snapshot {
	t.Helper()
	identity := limits.Identity{KeyID: h.key.ID, TokensPerMinute: h.key.TokensPerMinute, USDPerDay: h.key.USDPerDay}
	snapshots, err := h.enforcer.Snapshot(context.Background(), identity, "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range snapshots {
		if snapshot.Name == name {
			return snapshot
		}
	}
	t.Fatalf("snapshot %s missing: %+v", name, snapshots)
	return limits.Snapshot{}
}

func TestGatewayTokenLimitAcceptanceResetAndLiveKeyEdit(t *testing.T) {
	h := newLimitsHarness(t, store.APIKey{TokensPerMinute: 1000}, provider.Usage{PromptTokens: 500, CompletionTokens: 100, TotalTokens: 600, CachedPromptTokens: 250, ReasoningTokens: 20})
	body := limitsChatBody(t, false)
	first := h.request("/v1/chat/completions", body)
	requireLimitHeaders(t, first)
	if first.Code != 200 || first.Result().Header.Get("x-ratelimit-remaining-tokens") != "400" {
		t.Fatalf("first = %d/%s/%v", first.Code, first.Body.String(), first.Header())
	}
	second := h.request("/v1/chat/completions", body)
	requireLimitHeaders(t, second)
	if second.Code != 429 || second.Header().Get("Retry-After") != "60" || second.Header().Get("x-ratelimit-remaining-tokens") != "400" || second.Header().Get("x-ratelimit-reset-tokens") != "1m0s" {
		t.Fatalf("second = %d/%s/%v", second.Code, second.Body.String(), second.Header())
	}
	var envelope struct {
		Error struct {
			Type, Code, Message string
			Param               any
		}
	}
	if err := json.Unmarshal(second.Body.Bytes(), &envelope); err != nil || envelope.Error.Type != "rate_limit_exceeded" || envelope.Error.Code != "total_tokens_limit_exceeded" || envelope.Error.Message != "policy key-tokens-per-minute: 1000 total_tokens per 1m exceeded" || envelope.Error.Param != nil {
		t.Fatalf("429 envelope = %s/%v", second.Body.String(), err)
	}
	if h.provider.calls.Load() != 1 {
		t.Fatal("rejected request reached provider")
	}
	logs, total, err := h.st.ListLogs(context.Background(), 10, 0, "", "limited")
	if err != nil || total != 1 || logs[0].Status != "limited" || logs[0].Decision != "DENY" || logs[0].DecisionReason != "rate_limit:key-tokens-per-minute" || logs[0].Endpoint != "chat_completions" || logs[0].APIKeyID != h.key.ID || logs[0].RequestBody != "" || logs[0].ResponseBody != "" || logs[0].CostSource != "none" {
		t.Fatalf("limited audit = %+v/%v", logs, err)
	}
	logs, _, _ = h.st.ListLogs(context.Background(), 10, 0, "", "success")
	if logs[0].CachedTokens != 250 || logs[0].ReasoningTokens != 20 || logs[0].CostSource != "catalog" {
		t.Fatalf("success usage = %+v", logs[0])
	}
	h.key.TokensPerMinute = 2000
	if err := h.st.InsertAPIKey(context.Background(), h.key); err != nil {
		t.Fatal(err)
	}
	third := h.request("/v1/chat/completions", body)
	if third.Code != 200 || third.Header().Get("x-ratelimit-limit-tokens") != "2000" || third.Header().Get("x-ratelimit-remaining-tokens") != "800" {
		t.Fatalf("live edit = %d/%v", third.Code, third.Header())
	}
	h.now = h.now.Add(time.Minute)
	if next := h.request("/v1/chat/completions", body); next.Code != 200 || next.Header().Get("x-ratelimit-remaining-tokens") != "1400" {
		t.Fatalf("next window = %d/%v", next.Code, next.Header())
	}
}

func TestGatewayStreamReconcilesUsageWithoutExposingUnrequestedUsage(t *testing.T) {
	h := newLimitsHarness(t, store.APIKey{TokensPerMinute: 1000}, provider.Usage{PromptTokens: 250, CompletionTokens: 50, TotalTokens: 300, CachedPromptTokens: 100, ReasoningTokens: 20})
	h.gateway.SetStreamGuardrailBuffer(false)
	response := h.request("/v1/chat/completions", limitsChatBody(t, true))
	requireLimitHeaders(t, response)
	if response.Code != 200 || !h.provider.includeUsage || !bytes.Contains(response.Body.Bytes(), []byte("[DONE]")) || bytes.Contains(response.Body.Bytes(), []byte(`"usage"`)) {
		t.Fatalf("stream = %d/%s", response.Code, response.Body.String())
	}
	if response.Result().Header.Get("x-ratelimit-remaining-tokens") != "400" {
		t.Fatal("live stream headers must represent reservation")
	}
	if got := h.snapshot(t, "key-tokens-per-minute"); got.Used != 300 || got.Remaining != 700 {
		t.Fatalf("actual stream snapshot = %+v", got)
	}
	logs, _, err := h.st.ListLogs(context.Background(), 10, 0, "", "success")
	if err != nil || len(logs) != 1 || logs[0].TotalTokens != 300 || logs[0].CachedTokens != 100 || logs[0].ReasoningTokens != 20 || logs[0].CostSource != "catalog" {
		t.Fatalf("stream usage log = %+v/%v", logs, err)
	}
}
