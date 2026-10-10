package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/limits"
	"github.com/omniswitch-dev/omniswitch/internal/provider"
	"github.com/omniswitch-dev/omniswitch/internal/router"
	"github.com/omniswitch-dev/omniswitch/internal/store"
)

func TestGatewayUSDLimitAndLogOnlyPolicies(t *testing.T) {
	h := newLimitsHarness(t, store.APIKey{USDPerDay: .0002}, provider.Usage{PromptTokens: 500, CompletionTokens: 100, TotalTokens: 600}, limits.Policy{Name: "observe", Scope: limits.ScopeKey, Unit: limits.TotalTokens, Limit: 1, Window: "1m", Action: limits.Log})
	body := limitsChatBody(t, false)
	first := h.request("/v1/chat/completions", body)
	if first.Code != 200 || first.Header().Get("x-omniswitch-limit-usd") != "0.0002" || first.Header().Get("x-ratelimit-limit-tokens") != "0" {
		t.Fatalf("USD/log first = %d/%s/%v", first.Code, first.Body.String(), first.Header())
	}
	if got := h.snapshot(t, "observe"); got.Used != 600 || got.Remaining != 0 {
		t.Fatalf("log policy = %+v", got)
	}
	second := h.request("/v1/chat/completions", body)
	if second.Code != 429 || !strings.Contains(second.Body.String(), `"code":"usd_limit_exceeded"`) || h.provider.calls.Load() != 1 {
		t.Fatalf("USD second = %d/%s", second.Code, second.Body.String())
	}
}

func TestGatewayUnknownUsageRetainsReservationAndFailuresRelease(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		h := newLimitsHarness(t, store.APIKey{TokensPerMinute: 1000, USDPerDay: 1}, provider.Usage{})
		h.gateway.SetStreamGuardrailBuffer(false)
		response := h.request("/v1/chat/completions", limitsChatBody(t, streaming))
		if response.Code != 200 || h.snapshot(t, "key-tokens-per-minute").Used != 600 || h.snapshot(t, "key-usd-per-day").Used <= 0 {
			t.Fatalf("unknown usage = %d/%s", response.Code, response.Body.String())
		}
		logs, _, err := h.st.ListLogs(context.Background(), 10, 0, "", "success")
		if err != nil || logs[0].Cost != 0 || logs[0].CostSource != "none" {
			t.Fatalf("unknown usage log = %+v/%v", logs, err)
		}
	}
	h := newLimitsHarness(t, store.APIKey{TokensPerMinute: 1000}, provider.Usage{})
	h.provider.fail = true
	if response := h.request("/v1/chat/completions", limitsChatBody(t, false)); response.Code != 502 {
		t.Fatalf("upstream failure = %d/%s", response.Code, response.Body.String())
	}
	if got := h.snapshot(t, "key-tokens-per-minute"); got.Used != 0 {
		t.Fatalf("failed attempt retained reservation = %+v", got)
	}
}

func TestLimitEnvelopesPreserveCompatibilityEndpoints(t *testing.T) {
	h := newLimitsHarness(t, store.APIKey{RequestsPerMinute: 1}, provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2})
	if response := h.request("/v1/chat/completions", limitsChatBody(t, false)); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	for _, path := range []string{"/v1/messages", "/v1/responses"} {
		response := h.request(path, limitsChatBody(t, false))
		requireLimitHeaders(t, response)
		if response.Code != 429 || response.Header().Get("Retry-After") != "60" {
			t.Fatalf("%s = %d/%s", path, response.Code, response.Body.String())
		}
		if path == "/v1/messages" && (!strings.Contains(response.Body.String(), `"type":"error"`) || !strings.Contains(response.Body.String(), `"type":"rate_limit_error"`)) {
			t.Fatal("Messages error is not Anthropic-compatible")
		}
		if path == "/v1/responses" && !strings.Contains(response.Body.String(), `"code":"requests_limit_exceeded"`) {
			t.Fatal("Responses error is not OpenAI-compatible")
		}
	}
	logs, total, err := h.st.ListLogs(context.Background(), 10, 0, "", "limited")
	endpoints := map[string]bool{}
	for _, log := range logs {
		endpoints[log.Endpoint] = true
	}
	if err != nil || total != 2 || !endpoints["messages"] || !endpoints["responses"] {
		t.Fatalf("compatibility audits = %+v/%v", logs, err)
	}
}

func TestLimitHeadersOnAuthenticationAndMethodErrors(t *testing.T) {
	h := newLimitsHarness(t, store.APIKey{}, provider.Usage{})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, request)
	if response.Code != 401 {
		t.Fatalf("auth = %d", response.Code)
	}
	requireLimitHeaders(t, response)
	request = httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	request.Header.Set("Authorization", "Bearer "+limitsTestToken)
	response = httptest.NewRecorder()
	h.handler.ServeHTTP(response, request)
	if response.Code != 405 {
		t.Fatalf("method = %d", response.Code)
	}
	requireLimitHeaders(t, response)
}

func TestEmbeddingAndRerankReconcileInputLimits(t *testing.T) {
	for _, tc := range []struct {
		path, body    string
		limit, actual float64
	}{
		{"/v1/embeddings", `{"model":"gpt-4o-mini","input":"abcdefghijklmnopqrst"}`, 5, 1},
		{"/v1/rerank", `{"model":"gpt-4o-mini","query":"abcdefghijklmnopqrst","documents":["abcdefghijklmnopqrst"]}`, 10, 3},
	} {
		t.Run(tc.path, func(t *testing.T) {
			h := newLimitsHarness(t, store.APIKey{}, provider.Usage{}, limits.Policy{Name: "input", Scope: limits.ScopeKey, Unit: limits.InputTokens, Limit: tc.limit, Window: "1m"})
			first := h.request(tc.path, tc.body)
			if first.Code != 200 || h.snapshot(t, "input").Used != tc.actual {
				t.Fatalf("first = %d/%s", first.Code, first.Body.String())
			}
			second := h.request(tc.path, tc.body)
			if second.Code != 429 {
				t.Fatalf("second = %d/%s", second.Code, second.Body.String())
			}
			requireLimitHeaders(t, second)
		})
	}
}

type unavailableLimitsBackend struct{ limits.Backend }

func (b unavailableLimitsBackend) Consume(context.Context, []limits.Charge) ([]int64, int, error) {
	return nil, -1, errors.New("private backend failure")
}

func TestGatewayLimitBackendFailureDefaultsClosed(t *testing.T) {
	for _, failOpen := range []bool{false, true} {
		h := newLimitsHarness(t, store.APIKey{}, provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2})
		enforcer, err := limits.NewEnforcer(unavailableLimitsBackend{limits.NewMemoryBackend()}, limits.Options{Requests: 1000, Window: time.Minute, FailOpen: failOpen})
		if err != nil {
			t.Fatal(err)
		}
		h.gateway.SetLimits(enforcer)
		// Call directly to exercise both the request and provider-reservation checks.
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(limitsChatBody(t, false)))
		response := httptest.NewRecorder()
		h.gateway.ChatCompletions(response, request)
		if !failOpen && (response.Code != 503 || strings.Contains(response.Body.String(), "private backend failure") || h.provider.calls.Load() != 0) {
			t.Fatalf("closed = %d/%s", response.Code, response.Body.String())
		}
		if failOpen && (response.Code != 200 || h.provider.calls.Load() != 1) {
			t.Fatalf("open = %d/%s", response.Code, response.Body.String())
		}
	}
}

func TestDeferredRequestModelCheckCannotBypassRoutedQuota(t *testing.T) {
	h := newLimitsHarness(t, store.APIKey{}, provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}, limits.Policy{Name: "submitted-model", Scope: limits.ScopeModel, Unit: limits.Requests, Limit: 1, Window: "1m", Match: limits.Match{Model: "production"}})
	h.gateway.router.SetRoute("production", router.Route{Variants: []router.Variant{{Provider: "openai", Model: "gpt-4o-mini", Weight: 100}}})
	body := strings.Replace(limitsChatBody(t, false), `"model":"gpt-4o-mini"`, `"model":"production"`, 1)
	first := h.request("/v1/chat/completions", body)
	if first.Code != 200 || first.Header().Get("x-ratelimit-limit-requests") != "1" || first.Header().Get("x-ratelimit-remaining-requests") != "0" {
		t.Fatalf("routed request headers = %d/%s/%v", first.Code, first.Body.String(), first.Header())
	}
	large := `{"padding":"` + strings.Repeat("x", 64<<10) + `",` + strings.TrimPrefix(body, "{")
	second := h.request("/v1/chat/completions", large)
	if second.Code != 429 || h.provider.calls.Load() != 1 {
		t.Fatalf("deferred quota = %d/%s", second.Code, second.Body.String())
	}
}

func TestLimitIdentityDoesNotTrustSpoofedQuotaHeaders(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.Header.Set("x-omniswitch-key-id", "spoofed-key")
	request.Header.Set("x-omniswitch-workspace-id", "spoofed-workspace")
	request.Header.Set("x-omniswitch-rate-limit", "99999")
	request = request.WithContext(withIdentity(request.Context(), Identity{APIKeyID: "trusted-key", WorkspaceID: "trusted-workspace", OrganizationID: "trusted-org", RateLimit: 5, TokensPerMinute: 1000, USDPerDay: 2}))
	got := LimitIdentity(request)
	if got.KeyID != "trusted-key" || got.WorkspaceID != "trusted-workspace" || got.OrganizationID != "trusted-org" || got.RateLimit != 5 || got.TokensPerMinute != 1000 || got.USDPerDay != 2 {
		t.Fatalf("spoofed identity = %+v", got)
	}
}

func TestSemanticCacheCountsLogicalTokensButNotNewUSD(t *testing.T) {
	h := newLimitsHarness(t, store.APIKey{TokensPerMinute: 2000, USDPerDay: .0002}, provider.Usage{PromptTokens: 500, CompletionTokens: 100, TotalTokens: 600})
	h.gateway.SetSemanticCache(.95)
	body := limitsChatBody(t, false)
	if first := h.request("/v1/chat/completions", body); first.Code != 200 {
		t.Fatal(first.Body.String())
	}
	before := h.snapshot(t, "key-usd-per-day").Used
	second := h.request("/v1/chat/completions", body)
	if second.Code != 200 || h.provider.calls.Load() != 1 || h.snapshot(t, "key-tokens-per-minute").Used != 1200 || h.snapshot(t, "key-usd-per-day").Used != before {
		t.Fatalf("cache limits = %d/%s", second.Code, second.Body.String())
	}
	logs, _, err := h.st.ListLogs(context.Background(), 10, 0, "", "success")
	if err != nil || len(logs) != 2 {
		t.Fatal(err)
	}
	var cached bool
	for _, log := range logs {
		if log.Cached {
			cached = true
			if log.CostSource != "none" || log.Cost != 0 {
				t.Fatal("cached USD was billed")
			}
		}
	}
	if !cached {
		t.Fatal("semantic cache was not exercised")
	}
}
