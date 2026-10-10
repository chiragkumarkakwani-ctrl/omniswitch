// Package catalog contains the embedded model catalog and operator-supplied
// pricing overlays. Unknown prices stay unknown rather than being guessed.
package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"sync/atomic"
)

//go:embed models.json
var modelsJSON []byte

// Model records published prices and capabilities. Cached/write prices are
// optional: when absent, token costing falls back to the ordinary input price.
type Model struct {
	Provider              string             `json:"provider"`
	ID                    string             `json:"id"`
	OwnedBy               string             `json:"owned_by,omitempty"`
	InputPerMillion       float64            `json:"input_per_million,omitempty"`
	OutputPerMillion      float64            `json:"output_per_million,omitempty"`
	CachedInputPerMillion *float64           `json:"cached_input_per_million,omitempty"`
	CacheWritePerMillion  *float64           `json:"cache_write_per_million,omitempty"`
	PerImage              map[string]float64 `json:"per_image,omitempty"`
	PerAudioMinute        float64            `json:"per_audio_minute,omitempty"`
	PerMillionCharacters  float64            `json:"per_million_characters,omitempty"`
	ContextWindow         int                `json:"context_window,omitempty"`
	MaxOutputTokens       int                `json:"max_output_tokens,omitempty"`
	Modalities            []string           `json:"modalities,omitempty"`
	SupportsTools         bool               `json:"supports_tools,omitempty"`
	DeprecatedOn          string             `json:"deprecated_on,omitempty"`
	SourceURL             string             `json:"source_url"`
	VerifiedOn            string             `json:"verified_on"`
	Notes                 string             `json:"notes,omitempty"`
	Source                string             `json:"source,omitempty"`
	Priced                bool               `json:"-"` // whether any token rate (including explicit zero) is known
	InputPriced           bool               `json:"-"`
	OutputPriced          bool               `json:"-"`
}

func (m *Model) UnmarshalJSON(data []byte) error {
	type plain Model
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*m = Model(decoded)
	for _, field := range []string{"input_per_million", "output_per_million", "cached_input_per_million", "cache_write_per_million"} {
		if value, exists := fields[field]; exists && string(value) != "null" {
			m.Priced = true
			m.InputPriced = m.InputPriced || field == "input_per_million"
			m.OutputPriced = m.OutputPriced || field == "output_per_million"
		} else if exists && (field == "input_per_million" || field == "output_per_million") {
			return fmt.Errorf("%s must be a number, not null", field)
		}
	}
	return nil
}

// Override is applied last; explicit zero prices allow free self-hosted models.
type Override struct {
	Provider              string   `json:"provider" yaml:"provider"`
	Model                 string   `json:"model" yaml:"model"`
	InputPerMillion       float64  `json:"input_per_million" yaml:"input_per_million"`
	OutputPerMillion      float64  `json:"output_per_million" yaml:"output_per_million"`
	CachedInputPerMillion *float64 `json:"cached_input_per_million,omitempty" yaml:"cached_input_per_million,omitempty"`
}

type Catalog struct {
	models        []Model
	file          string
	overrideCount int
	byKey         map[string]int
}

var active atomic.Pointer[Catalog]

func init() {
	catalog, err := Load("", nil)
	if err != nil {
		panic("provider/catalog: " + err.Error())
	}
	active.Store(catalog)
}

func modelKey(provider, id string) string {
	return strings.ToLower(strings.TrimSpace(provider)) + "/" + strings.ToLower(strings.TrimSpace(id))
}

// Load merges a file's fields over embedded rows by provider/id, then applies
// overrides. It never fetches prices or changes verification dates.
func Load(file string, overrides []Override) (*Catalog, error) {
	catalog := &Catalog{byKey: make(map[string]int), file: file, overrideCount: len(overrides)}
	if err := catalog.merge(modelsJSON, "embedded"); err != nil {
		return nil, fmt.Errorf("embedded catalog: %w", err)
	}
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("pricing.catalog_file: %w", err)
		}
		if err := catalog.merge(data, "file"); err != nil {
			return nil, fmt.Errorf("pricing.catalog_file: %w", err)
		}
	}
	if err := ValidateOverrides(overrides); err != nil {
		return nil, err
	}
	for _, override := range overrides {
		model, found := catalog.Lookup(override.Provider, override.Model)
		if !found {
			model = Model{Provider: override.Provider, ID: override.Model, OwnedBy: override.Provider}
		}
		model.InputPerMillion, model.OutputPerMillion = override.InputPerMillion, override.OutputPerMillion
		if override.CachedInputPerMillion != nil {
			value := *override.CachedInputPerMillion
			model.CachedInputPerMillion = &value
		}
		model.Source, model.Priced = "override", true
		model.InputPriced, model.OutputPriced = true, true
		catalog.put(model)
	}
	return catalog, nil
}

func (c *Catalog) put(model Model) {
	model.Provider = strings.ToLower(strings.TrimSpace(model.Provider))
	key := modelKey(model.Provider, model.ID)
	if index, exists := c.byKey[key]; exists {
		c.models[index] = model
	} else {
		c.byKey[key] = len(c.models)
		c.models = append(c.models, model)
	}
}

func (c *Catalog) merge(data []byte, source string) error {
	var document struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return err
	}
	if document.Models == nil {
		return fmt.Errorf("models array is required")
	}
	seen := make(map[string]bool)
	for index, row := range document.Models {
		var model Model
		if err := json.Unmarshal(row, &model); err != nil {
			return fmt.Errorf("models[%d]: %w", index, err)
		}
		key := modelKey(model.Provider, model.ID)
		if seen[key] {
			return fmt.Errorf("models[%d]: duplicate provider/id %s", index, key)
		}
		seen[key] = true
		if existing, found := c.Lookup(model.Provider, model.ID); found {
			base, _ := json.Marshal(existing)
			var fields, overlay map[string]json.RawMessage
			if err := json.Unmarshal(base, &fields); err != nil {
				return err
			}
			if err := json.Unmarshal(row, &overlay); err != nil {
				return err
			}
			for field, value := range overlay {
				fields[field] = value
			}
			merged, err := json.Marshal(fields)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(merged, &model); err != nil {
				return err
			}
			model.Priced = model.Priced || existing.Priced
			model.InputPriced = model.InputPriced || existing.InputPriced
			model.OutputPriced = model.OutputPriced || existing.OutputPriced
		}
		model.Source = source
		if err := validateModel(model); err != nil {
			return fmt.Errorf("models[%d]: %w", index, err)
		}
		c.put(model)
	}
	return nil
}

func validPrice(price float64) bool { return price >= 0 && !math.IsNaN(price) && !math.IsInf(price, 0) }

func validateModel(model Model) error {
	if strings.TrimSpace(model.Provider) == "" || strings.TrimSpace(model.ID) == "" {
		return fmt.Errorf("provider and id are required")
	}
	prices := []float64{model.InputPerMillion, model.OutputPerMillion, model.PerAudioMinute, model.PerMillionCharacters}
	if model.CachedInputPerMillion != nil {
		prices = append(prices, *model.CachedInputPerMillion)
	}
	if model.CacheWritePerMillion != nil {
		prices = append(prices, *model.CacheWritePerMillion)
	}
	for size, price := range model.PerImage {
		if strings.TrimSpace(size) == "" {
			return fmt.Errorf("per_image size[/quality] is required")
		}
		prices = append(prices, price)
	}
	for _, price := range prices {
		if !validPrice(price) {
			return fmt.Errorf("prices must be finite non-negative USD amounts")
		}
	}
	if model.ContextWindow < 0 || model.MaxOutputTokens < 0 {
		return fmt.Errorf("context_window and max_output_tokens must be non-negative")
	}
	return nil
}

func ValidateOverrides(overrides []Override) error {
	for index, override := range overrides {
		if strings.TrimSpace(override.Provider) == "" || strings.TrimSpace(override.Model) == "" {
			return fmt.Errorf("pricing.overrides[%d]: provider and model are required", index)
		}
		model := Model{Provider: override.Provider, ID: override.Model, InputPerMillion: override.InputPerMillion, OutputPerMillion: override.OutputPerMillion, CachedInputPerMillion: override.CachedInputPerMillion}
		if err := validateModel(model); err != nil {
			return fmt.Errorf("pricing.overrides[%d]: %w", index, err)
		}
	}
	return nil
}

// Activate atomically swaps the pricing catalog for all adapters. The returned
// prior catalog can be restored by embedding callers and tests.
func Activate(catalog *Catalog) *Catalog {
	if catalog == nil {
		return active.Load()
	}
	return active.Swap(catalog)
}

func Active() *Catalog { return active.Load() }

func cloneModel(model Model) Model {
	model.Modalities = append([]string(nil), model.Modalities...)
	if model.CachedInputPerMillion != nil {
		value := *model.CachedInputPerMillion
		model.CachedInputPerMillion = &value
	}
	if model.CacheWritePerMillion != nil {
		value := *model.CacheWritePerMillion
		model.CacheWritePerMillion = &value
	}
	if model.PerImage != nil {
		prices := make(map[string]float64, len(model.PerImage))
		for size, price := range model.PerImage {
			prices[size] = price
		}
		model.PerImage = prices
	}
	return model
}

func (c *Catalog) All() []Model {
	out := make([]Model, len(c.models))
	for index, model := range c.models {
		out[index] = cloneModel(model)
	}
	return out
}

func (c *Catalog) ForProvider(provider string) []Model {
	out := make([]Model, 0)
	for _, model := range c.models {
		if strings.EqualFold(model.Provider, strings.TrimSpace(provider)) {
			out = append(out, cloneModel(model))
		}
	}
	return out
}

func (c *Catalog) Lookup(provider, id string) (Model, bool) {
	index, exists := c.byKey[modelKey(provider, id)]
	if !exists {
		return Model{}, false
	}
	return cloneModel(c.models[index]), true
}

// VerifiedOn is the oldest non-empty row date, so a partial re-check does not
// claim that the entire catalog was verified today. Overrides add no date.
func (c *Catalog) VerifiedOn() string {
	date := ""
	for _, model := range c.models {
		if model.VerifiedOn != "" && (date == "" || model.VerifiedOn < date) {
			date = model.VerifiedOn
		}
	}
	return date
}

func All() []Model                             { return Active().All() }
func ForProvider(provider string) []Model      { return Active().ForProvider(provider) }
func Lookup(provider, id string) (Model, bool) { return Active().Lookup(provider, id) }

func (c *Catalog) File() string       { return c.file }
func (c *Catalog) OverrideCount() int { return c.overrideCount }
