package provider

import (
	"strings"

	"github.com/omniswitch-dev/omniswitch/internal/provider/catalog"
)

func catalogModelInfos(providerName, providerLabel string) []ModelInfo {
	entries := catalog.ForProvider(providerName)
	out := make([]ModelInfo, len(entries))
	for index, model := range entries {
		out[index] = ModelInfo{ID: model.ID, Object: "model", OwnedBy: model.OwnedBy, Provider: providerLabel}
	}
	return out
}

func canonicalPricingProvider(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "azure", "azure-openai":
		return "openai"
	case "gemini":
		return "google"
	default:
		return name
	}
}

func pricingFor(providerName, model string) ModelPricing {
	if strings.EqualFold(providerName, "bedrock") {
		return bedrockPricing(model)
	}
	entry, found := pricingCatalogModel(providerName, model)
	if !found {
		return ModelPricing{}
	}
	return ModelPricing{InputPerMillion: entry.InputPerMillion, OutputPerMillion: entry.OutputPerMillion, CachedInputPerMillion: entry.CachedInputPerMillion, CacheWritePerMillion: entry.CacheWritePerMillion}
}

// EstimateCost bills cached/write tokens as subsets of input. Reasoning tokens
// are already included in completion tokens by the adapter that returns them.
func EstimateCost(providerName, model string, usage Usage) float64 {
	cost, _ := EstimateCostWithSource(providerName, model, usage)
	return cost
}

func EstimateCostWithSource(providerName, model string, usage Usage) (float64, string) {
	entry, found := pricingCatalogModel(providerName, model)
	if !found && strings.EqualFold(providerName, "bedrock") {
		for _, candidate := range catalog.ForProvider("bedrock") {
			if strings.Contains(strings.ToLower(model), strings.ToLower(candidate.ID)) {
				entry, found = candidate, true
				break
			}
		}
	}
	if !found || !knownTokenRates(entry, usage) {
		return 0, "none"
	}
	price := ModelPricing{InputPerMillion: entry.InputPerMillion, OutputPerMillion: entry.OutputPerMillion, CachedInputPerMillion: entry.CachedInputPerMillion, CacheWritePerMillion: entry.CacheWritePerMillion}
	source := "catalog"
	if entry.Source == "override" {
		source = "override"
	}
	return price.CostUsage(usage), source
}

func knownTokenRates(model catalog.Model, usage Usage) bool {
	input := max(0, usage.PromptTokens)
	cached := min(input, max(0, usage.CachedPromptTokens))
	written := min(input-cached, max(0, usage.CacheWriteTokens))
	if input-cached-written > 0 && !model.InputPriced ||
		cached > 0 && model.CachedInputPerMillion == nil && !model.InputPriced ||
		written > 0 && model.CacheWritePerMillion == nil && !model.InputPriced ||
		usage.CompletionTokens > 0 && !model.OutputPriced {
		return false
	}
	return model.Priced
}

func pricingCatalogModel(providerName, model string) (catalog.Model, bool) {
	if entry, found := catalog.Lookup(providerName, model); found {
		return entry, true
	}
	providerName = strings.TrimPrefix(providerName, "@")
	if entry, found := catalog.Lookup(providerName, model); found {
		return entry, true
	}
	return catalog.Lookup(canonicalPricingProvider(providerName), model)
}

// RepriceUsage prefers an account-name override, then the provider's canonical
// type. Aliased preset, Azure, and Bedrock accounts retain correct provenance.
func RepriceUsage(meta *ProviderMeta, model string, usage Usage) {
	if !usage.Known && usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.TotalTokens == 0 {
		meta.Cost, meta.CostSource = 0, "none"
		return
	}
	cost, source := EstimateCostWithSource(meta.Provider, model, usage)
	if strings.HasPrefix(meta.Provider, "@") {
		model = strings.TrimPrefix(model, meta.Provider+"/")
		if source == "none" {
			cost, source = EstimateCostWithSource(meta.Provider, model, usage)
		}
	}
	if source == "none" && meta.ProviderType != "" {
		cost, source = EstimateCostWithSource(meta.ProviderType, model, usage)
	}
	meta.Cost, meta.CostSource = cost, source
}

// PricingTarget exposes the final canonical model/type without making a call.
// The account name is retained so local overrides take precedence.
func PricingTarget(current Provider, model string) (ProviderMeta, string) {
	meta := ProviderMeta{Provider: current.Name(), ProviderType: current.Name(), Model: model}
	switch typed := current.(type) {
	case *AliasProvider:
		inner, resolved := PricingTarget(typed.inner, typed.stripModel(model))
		meta.ProviderType = inner.ProviderType
		return meta, resolved
	case *Custom:
		meta.ProviderType = typed.pricingType()
	case *AzureOpenAI:
		meta.ProviderType = "openai"
	case *Bedrock:
		meta.ProviderType = "bedrock"
	}
	return meta, model
}

func InputPricePerMillion(providerName, model string) float64 {
	return pricingFor(providerName, model).InputPerMillion
}

func catalogPricingOrZero(providerName, model string) ModelPricing {
	entry, found := catalog.Lookup(providerName, model)
	if !found {
		return ModelPricing{}
	}
	return ModelPricing{InputPerMillion: entry.InputPerMillion, OutputPerMillion: entry.OutputPerMillion, CachedInputPerMillion: entry.CachedInputPerMillion, CacheWritePerMillion: entry.CacheWritePerMillion}
}
