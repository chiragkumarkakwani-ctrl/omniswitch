package provider

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/omniswitch-dev/omniswitch/internal/provider/catalog"
)

const detailedUsageJSON = `{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":80},"completion_tokens_details":{"reasoning_tokens":10}}`

type usageTransport func(*http.Request) (*http.Response, error)

func (fn usageTransport) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func usageClient(body string) *http.Client {
	return &http.Client{Transport: usageTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
}

func TestUsageDetailsRoundTripAndCost(t *testing.T) {
	var usage Usage
	if err := json.Unmarshal([]byte(detailedUsageJSON), &usage); err != nil {
		t.Fatal(err)
	}
	if !usage.Known || usage.CachedPromptTokens != 80 || usage.ReasoningTokens != 10 || usage.TotalTokens != 120 {
		t.Fatalf("normalized usage = %+v", usage)
	}
	data, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["prompt_tokens_details"].(map[string]any)["cached_tokens"] != float64(80) || decoded["completion_tokens_details"].(map[string]any)["reasoning_tokens"] != float64(10) {
		t.Fatalf("wire details = %s", data)
	}
	cost, source := EstimateCostWithSource("openai", "gpt-4o-mini", usage)
	if math.Abs(cost-.000021) > 1e-12 || source != "catalog" {
		t.Fatalf("cached cost = %g/%s", cost, source)
	}
	if cost := (ModelPricing{InputPerMillion: 2, OutputPerMillion: 3}).CostUsage(usage); math.Abs(cost-.00026) > 1e-12 {
		t.Fatal("missing cached price must use input rate")
	}
	if cost, source := EstimateCostWithSource("local", "unpriced", usage); cost != 0 || source != "none" {
		t.Fatal("unknown cost guessed")
	}
	if err := json.Unmarshal([]byte(`{"prompt_tokens":5,"completion_tokens":3,"prompt_cache_hit_tokens":4}`), &usage); err != nil || usage.TotalTokens != 8 || usage.CachedPromptTokens != 4 {
		t.Fatal("DeepSeek usage or total normalization")
	}
	if err := json.Unmarshal([]byte(`{"prompt_tokens":"bad"}`), &usage); err == nil {
		t.Fatal("bad usage accepted")
	}
}

func TestOpenAICompatibleAdaptersParseDetailedUsage(t *testing.T) {
	body := `{"id":"chat","model":"gpt-4o-mini","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":` + detailedUsageJSON + `}`
	openai := NewOpenAI("")
	openai.client = usageClient(body)
	azure := NewAzureOpenAI("azure", "https://azure.example", "", "", nil)
	azure.client = usageClient(body)
	groq := NewGroq("")
	groq.client = usageClient(body)
	custom := NewCustom("openai", "https://custom.example/v1", "")
	custom.client = usageClient(body)
	for _, current := range []Provider{openai, azure, groq, custom} {
		t.Run(current.Name(), func(t *testing.T) {
			response, _, err := current.ChatCompletion(context.Background(), ChatRequest{Model: "gpt-4o-mini"})
			if err != nil || response.Usage.CachedPromptTokens != 80 || response.Usage.ReasoningTokens != 10 {
				t.Fatalf("usage = %+v/%v", response.Usage, err)
			}
		})
	}
	custom.client = usageClient("data: " + `{"id":"chat","model":"gpt-4o-mini","choices":[],"usage":` + detailedUsageJSON + "}\n\ndata: [DONE]\n\n")
	chunks, _, err := custom.ChatCompletionStream(context.Background(), ChatRequest{Model: "gpt-4o-mini"})
	if err != nil {
		t.Fatal(err)
	}
	got := collectChunks(chunks)
	if len(got) != 1 || got[0].Usage == nil || got[0].Usage.CachedPromptTokens != 80 || got[0].Usage.ReasoningTokens != 10 {
		t.Fatalf("stream usage = %+v", got)
	}
}

func TestAnthropicAndGeminiUsageBlockingAndStreaming(t *testing.T) {
	anthropic := NewAnthropic("")
	anthropic.client = usageClient(`{"id":"msg","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":10,"cache_read_input_tokens":4,"cache_creation_input_tokens":6,"output_tokens":5}}`)
	response, meta, err := anthropic.ChatCompletion(context.Background(), ChatRequest{Model: "claude-3-5-haiku-20241022"})
	if err != nil || response.Usage.PromptTokens != 20 || response.Usage.CachedPromptTokens != 4 || response.Usage.CacheWriteTokens != 6 || response.Usage.TotalTokens != 25 {
		t.Fatalf("Anthropic = %+v/%v", response.Usage, err)
	}
	if math.Abs(meta.Cost-.00003432) > 1e-12 {
		t.Fatalf("cache read/write price = %g", meta.Cost)
	}
	stream := "data: " + `{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":4,"cache_creation_input_tokens":6}}}` + "\n\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\ndata: " + `{"type":"message_stop"}` + "\n\n"
	got := collectChunks(streamAnthropic(context.Background(), io.NopCloser(strings.NewReader(stream)), "claude", true))
	last := got[len(got)-1].Usage
	if last == nil || last.PromptTokens != 20 || last.CacheWriteTokens != 6 || last.CachedPromptTokens != 4 {
		t.Fatalf("Anthropic stream = %+v", last)
	}
	google := NewGoogle("")
	geminiJSON := `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"thoughtsTokenCount":2,"cachedContentTokenCount":4,"totalTokenCount":15}}`
	google.client = usageClient(geminiJSON)
	response, _, err = google.ChatCompletion(context.Background(), ChatRequest{Model: "gemini-2.5-flash"})
	if err != nil || response.Usage.CompletionTokens != 5 || response.Usage.ReasoningTokens != 2 || response.Usage.CachedPromptTokens != 4 || response.Usage.TotalTokens != 15 {
		t.Fatalf("Gemini = %+v/%v", response.Usage, err)
	}
	got = collectChunks(streamGemini(context.Background(), io.NopCloser(strings.NewReader("data: "+geminiJSON+"\n\n")), "gemini", true))
	last = got[len(got)-1].Usage
	if last == nil || last.ReasoningTokens != 2 || last.CachedPromptTokens != 4 || last.CompletionTokens != 5 {
		t.Fatalf("Gemini stream = %+v", last)
	}
	if (geminiUsage{PromptTokenCount: 3, CandidatesTokenCount: 2, ThoughtsTokenCount: 1}).normalized().TotalTokens != 6 {
		t.Fatal("Gemini total fallback")
	}
}

func TestPricingOverridesAliasesAndMedia(t *testing.T) {
	cached := .5
	effective, err := catalog.Load("", []catalog.Override{{Provider: "local", Model: "model", InputPerMillion: 2, OutputPerMillion: 4, CachedInputPerMillion: &cached}, {Provider: "local", Model: "free"}, {Provider: "billing-account", Model: "gpt-4o-mini", InputPerMillion: 2, OutputPerMillion: 4, CachedInputPerMillion: &cached}})
	if err != nil {
		t.Fatal(err)
	}
	old := catalog.Activate(effective)
	t.Cleanup(func() { catalog.Activate(old) })
	usage := Usage{PromptTokens: 1000, CompletionTokens: 100, CachedPromptTokens: 800, ReasoningTokens: 50}
	cost, source := EstimateCostWithSource("local", "model", usage)
	if math.Abs(cost-.0012) > 1e-12 || source != "override" {
		t.Fatalf("override = %g/%s", cost, source)
	}
	if cost, source := EstimateCostWithSource("local", "free", usage); cost != 0 || source != "override" {
		t.Fatal("free override source lost")
	}
	alias := NewAlias("account", NewOpenAI(""))
	meta, model := PricingTarget(alias, "@account/gpt-4o-mini")
	if model != "gpt-4o-mini" || meta.ProviderType != "openai" {
		t.Fatalf("alias target = %+v/%s", meta, model)
	}
	RepriceUsage(&meta, "@account/gpt-4o-mini", usage)
	if meta.CostSource != "catalog" || meta.Cost == 0 {
		t.Fatal("alias lost pricing")
	}
	account, _ := PricingTarget(NewAlias("billing-account", NewOpenAI("")), "@billing-account/gpt-4o-mini")
	RepriceUsage(&account, "@billing-account/gpt-4o-mini", usage)
	if account.CostSource != "override" || math.Abs(account.Cost-.0012) > 1e-12 {
		t.Fatalf("bare account-name override = %+v", account)
	}
	if cost, source := EstimateImageCost(meta, "gpt-image-1.5", "1024x1024", "medium", 2); cost != .068 || source != "catalog" {
		t.Fatal("image cost")
	}
	if cost, source := EstimateImageCost(meta, "gpt-image-1.5", "1024x1024", "unknown", 1); cost != 0 || source != "none" {
		t.Fatal("uncited image price guessed")
	}
	if cost, source := EstimateAudioCost(meta, "whisper-1", 120); cost != .012 || source != "catalog" {
		t.Fatal("audio cost")
	}
	if _, source := EstimateAudioCost(meta, "whisper-1", 0); source != "none" {
		t.Fatal("unknown duration must be unpriced")
	}
	if _, source := EstimateCostWithSource("openai", "whisper-1", usage); source != "none" {
		t.Fatal("audio pricing must not imply a known token price")
	}
	if cost, source := EstimateSpeechCost(meta, "tts-1", 1000); cost != .015 || source != "catalog" {
		t.Fatal("speech cost")
	}
}

func TestMissingUsageIsNotReportedAsZero(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{"prompt_tokens":null}`} {
		var usage Usage
		if err := json.Unmarshal([]byte(body), &usage); err != nil || usage.Known {
			t.Fatalf("missing usage %s = %+v/%v", body, usage, err)
		}
	}
	var zero Usage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":0,"completion_tokens":0}`), &zero); err != nil || !zero.Known {
		t.Fatal("explicit zero usage is known")
	}
	for _, body := range []string{`{}`, `null`} {
		var anthropic anthropicUsage
		var gemini geminiUsage
		if err := json.Unmarshal([]byte(body), &anthropic); err != nil || anthropic.normalized().Known {
			t.Fatal("missing Anthropic usage became known")
		}
		if err := json.Unmarshal([]byte(body), &gemini); err != nil || gemini.normalized().Known {
			t.Fatal("missing Gemini usage became known")
		}
	}
	meta := ProviderMeta{Provider: "openai", Cost: 1}
	RepriceUsage(&meta, "gpt-4o-mini", Usage{})
	if meta.Cost != 0 || meta.CostSource != "none" {
		t.Fatal("unknown usage invented a known cost")
	}
	if _, source := EstimateCostWithSource("cohere", "rerank-v3.5", Usage{PromptTokens: 100}); source != "none" {
		t.Fatal("search-unit billing is not a known zero token cost")
	}
}

func TestBedrockCacheUsageIsIncludedOnce(t *testing.T) {
	current := NewBedrock("bedrock", "us-east-1", "fixture", "fixture", "", nil)
	current.client = usageClient(`{"output":{"message":{"role":"assistant","content":[{"text":"ok"}]}},"usage":{"inputTokens":10,"outputTokens":3,"totalTokens":13,"cacheReadInputTokens":20,"cacheWriteInputTokens":2}}`)
	response, meta, err := current.ChatCompletion(context.Background(), ChatRequest{Model: "anthropic.claude-3-5-sonnet-20241022-v2:0", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil || !response.Usage.Known || response.Usage.PromptTokens != 32 || response.Usage.TotalTokens != 35 || response.Usage.CachedPromptTokens != 20 || response.Usage.CacheWriteTokens != 2 || meta.CostSource != "catalog" {
		t.Fatalf("Bedrock = %+v/%+v/%v", response.Usage, meta, err)
	}
	if (bedrockUsage{InputTokens: 10, OutputTokens: 3, CacheReadInputTokens: 20}).normalized().TotalTokens != 33 {
		t.Fatal("Bedrock total fallback")
	}
}

func TestAliasKeepsPresetPricingType(t *testing.T) {
	current := NewCustom("preset", "https://example.test/v1", "", WithCustomType("xai"))
	current.client = usageClient(`{"model":"grok-4.3","usage":` + detailedUsageJSON + `}`)
	alias := NewAlias("xai-account", current)
	response, meta, err := alias.ChatCompletion(context.Background(), ChatRequest{Model: "@xai-account/grok-4.3"})
	if err != nil || meta.ProviderType != "xai" || meta.CostSource != "catalog" || response.Usage.ReasoningTokens != 10 {
		t.Fatalf("alias preset = %+v/%v", meta, err)
	}
}

func TestUnknownRateIsNotInferredFromAnotherDimension(t *testing.T) {
	model := catalog.Model{Priced: true, InputPriced: true}
	if !knownTokenRates(model, Usage{PromptTokens: 100}) || knownTokenRates(model, Usage{PromptTokens: 100, CompletionTokens: 1}) {
		t.Fatal("missing output rate treated as free")
	}
	zero := 0.0
	model = catalog.Model{Priced: true, CachedInputPerMillion: &zero}
	if !knownTokenRates(model, Usage{PromptTokens: 100, CachedPromptTokens: 100}) || knownTokenRates(model, Usage{PromptTokens: 101, CachedPromptTokens: 100}) {
		t.Fatal("missing uncached rate treated as free")
	}
}
