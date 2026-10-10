package limits

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/provider"
)

func TestEstimators(t *testing.T) {
	maxTokens, maxCompletion := 100, 200
	request := provider.ChatRequest{Messages: []provider.Message{{Role: "user", Content: strings.Repeat("a", 1988)}}, MaxTokens: &maxTokens}
	usage := EstimateChat(request)
	if usage.InputTokens != 500 || usage.OutputTokens != 100 {
		t.Fatalf("estimate = %+v", usage)
	}
	request.MaxTokens = nil
	if EstimateChat(request).OutputTokens != 256 {
		t.Fatal("default output reservation must be 256")
	}
	request.MaxCompletionTokens = &maxCompletion
	if EstimateChat(request).OutputTokens != 200 {
		t.Fatal("max_completion_tokens was ignored")
	}
	request.Messages[0].ToolCalls = []provider.ToolCall{{Function: provider.FunctionCall{Name: "query", Arguments: `{"text":"hello"}`}}}
	request.Tools = []provider.Tool{{Function: provider.ToolFunction{Name: "query", Description: "search", Parameters: map[string]any{"type": "object"}}}}
	if EstimateChat(request).InputTokens <= 500 {
		t.Fatal("tool text must contribute to input")
	}

	for _, test := range []struct {
		input any
		want  int64
	}{
		{"abcdefgh", 2}, {[]string{"abcd", "efgh"}, 2}, {[]int{1, 2, 3}, 3}, {[][]int{{1, 2}, {3}}, 3}, {map[string]any{"text": "other"}, 4},
	} {
		got := EstimateEmbedding(provider.EmbeddingRequest{Input: test.input})
		if got.InputTokens != test.want || got.OutputTokens != 0 {
			t.Fatalf("embedding %v = %+v, want %d", test.input, got, test.want)
		}
	}
	rerank := EstimateRerank(provider.RerankRequest{Query: "abcd", Documents: []any{"efgh", map[string]any{"text": "ijkl"}, map[string]any{"content": "mnop"}, 1234}})
	if rerank.InputTokens != 5 || rerank.OutputTokens != 0 {
		t.Fatalf("rerank estimate = %+v", rerank)
	}
	actual := FromProvider(provider.Usage{PromptTokens: 7, CompletionTokens: 2, TotalTokens: 9}, .25)
	if actual.InputTokens != 7 || actual.OutputTokens != 2 || actual.TotalTokens != 9 || actual.USD != .25 {
		t.Fatalf("actual = %+v", actual)
	}
}

func TestOpenAIHeaders(t *testing.T) {
	header := http.Header{}
	SetHeaders(header, []Snapshot{
		{Unit: Requests, Limit: 120, Remaining: 10, ResetsInSeconds: 1},
		{Unit: Requests, Limit: 20, Remaining: 10, ResetsInSeconds: 2},
		{Unit: Requests, Limit: 20, Remaining: 10, ResetsInSeconds: 3},
		{Unit: TotalTokens, Limit: 1000, Remaining: 400, ResetsInSeconds: 60},
		{Unit: InputTokens, Limit: 800, Remaining: 200, ResetsInSeconds: 360},
		{Unit: OutputTokens, Limit: 100, Remaining: 20, ResetsInSeconds: 60},
		{Unit: USD, Limit: .1, Remaining: .05, ResetsInSeconds: .001},
	})
	for key, want := range map[string]string{
		"x-ratelimit-limit-requests": "20", "x-ratelimit-remaining-requests": "10", "x-ratelimit-reset-requests": "3s",
		"x-ratelimit-limit-tokens": "100", "x-ratelimit-remaining-tokens": "20", "x-ratelimit-reset-tokens": "1m0s",
		"x-omniswitch-limit-usd": "0.1", "x-omniswitch-remaining-usd": "0.05", "x-omniswitch-reset-usd": "1ms",
	} {
		if got := header.Get(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	SetHeaders(header, nil)
	for _, unit := range []string{"requests", "tokens"} {
		if header.Get("x-ratelimit-limit-"+unit) != "0" || header.Get("x-ratelimit-remaining-"+unit) != "0" || header.Get("x-ratelimit-reset-"+unit) != "0s" {
			t.Fatal("unlimited headers missing")
		}
	}
	if header.Get("x-omniswitch-limit-usd") != "" {
		t.Fatal("stale USD header")
	}
	if ResetValue(360) != "6m0s" || RetryAfter(1001*time.Millisecond) != "2" || RetryAfter(0) != "1" {
		t.Fatal("duration format mismatch")
	}
	var decoded map[string]any
	data, _ := json.Marshal(Snapshot{Unit: USD, Limit: 1})
	if json.Unmarshal(data, &decoded) != nil || decoded["limit"] != float64(1) {
		t.Fatal("snapshot numeric shape")
	}
}
