package provider

import (
	"math"
	"strings"

	"github.com/omniswitch-dev/omniswitch/internal/provider/catalog"
)

func mediaPricingModel(meta ProviderMeta, model string) (catalog.Model, bool) {
	if entry, found := pricingCatalogModel(meta.Provider, model); found {
		return entry, true
	}
	if strings.HasPrefix(meta.Provider, "@") {
		model = strings.TrimPrefix(model, meta.Provider+"/")
		if entry, found := pricingCatalogModel(meta.Provider, model); found {
			return entry, true
		}
	}
	return catalog.Lookup(canonicalPricingProvider(firstPricingType(meta)), model)
}

func firstPricingType(meta ProviderMeta) string {
	if meta.ProviderType != "" {
		return meta.ProviderType
	}
	return meta.Provider
}

func mediaCostSource(model catalog.Model) string {
	if model.Source == "override" {
		return "override"
	}
	return "catalog"
}

// EstimateImageCost does not invent prices for missing dimensions/qualities.
// It first uses size/quality, then a size-only price if one was published.
func EstimateImageCost(meta ProviderMeta, model, size, quality string, count int) (float64, string) {
	entry, found := mediaPricingModel(meta, model)
	if !found || count <= 0 {
		return 0, "none"
	}
	price, priced := entry.PerImage[size+"/"+quality]
	if !priced {
		price, priced = entry.PerImage[size]
	}
	if !priced {
		return 0, "none"
	}
	return price * float64(count), mediaCostSource(entry)
}

// EstimateAudioCost requires reported duration; unknown duration is unpriced.
func EstimateAudioCost(meta ProviderMeta, model string, seconds float64) (float64, string) {
	entry, found := mediaPricingModel(meta, model)
	if !found || entry.PerAudioMinute <= 0 || seconds <= 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return 0, "none"
	}
	return entry.PerAudioMinute * seconds / 60, mediaCostSource(entry)
}

// EstimateSpeechCost takes Unicode character count, not UTF-8 byte length.
func EstimateSpeechCost(meta ProviderMeta, model string, characters int) (float64, string) {
	entry, found := mediaPricingModel(meta, model)
	if !found || entry.PerMillionCharacters <= 0 || characters <= 0 {
		return 0, "none"
	}
	return entry.PerMillionCharacters * float64(characters) / 1_000_000, mediaCostSource(entry)
}
