package provider

import (
	"testing"

	"github.com/omniswitch-dev/omniswitch/internal/provider/catalog"
)

// Static adapters and default preset lists must not drift from pricing data.
// Explicitly configured custom models may be unpriced (cost_source: none).
func TestProviderModelsAreSourcedFromCatalog(t *testing.T) {
	cases := []struct {
		name     string
		models   []ModelInfo
		wantID   string
		wantProv string
	}{
		{"openai", NewOpenAI("").Models(), "gpt-4o-mini", "openai"},
		{"anthropic", NewAnthropic("").Models(), "claude-3-5-haiku-20241022", "anthropic"},
		{"google", NewGoogle("").Models(), "gemini-2.5-flash", "google"},
		{"groq", NewGroq("").Models(), "llama-3.1-8b-instant", "groq"},
		{"cohere", NewCohere("").Models(), "rerank-v3.5", "cohere"},
		{"bedrock", NewBedrock("", "", "", "", "", nil).Models(), "anthropic.claude-3-5-sonnet-20241022-v2:0", "bedrock"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.models) == 0 {
				t.Fatalf("%s.Models() returned no models", tc.name)
			}
			var found bool
			for _, model := range tc.models {
				if _, ok := catalog.Lookup(tc.wantProv, model.ID); !ok {
					t.Fatalf("no catalog row for %s/%s", tc.wantProv, model.ID)
				}
				if model.ID == tc.wantID {
					found = true
					if model.Provider != tc.wantProv {
						t.Fatalf("model %q Provider = %q, want %q", model.ID, model.Provider, tc.wantProv)
					}
				}
			}
			if !found {
				t.Fatalf("%s.Models() = %+v, want to include %q", tc.name, tc.models, tc.wantID)
			}
		})
	}
	for _, name := range []string{"mistral", "xai", "deepseek", "together", "fireworks", "perplexity", "cerebras", "openrouter"} {
		t.Run(name, func(t *testing.T) {
			current := NewCustom("account", "https://example.test/v1", "", WithCustomType(name))
			models := current.Models()
			if len(models) == 0 {
				t.Fatalf("empty preset model list for %s", name)
			}
			for _, model := range models {
				row, found := catalog.Lookup(name, model.ID)
				if !found || !row.Priced || row.SourceURL == "" {
					t.Fatalf("missing sourced price for %s/%s", name, model.ID)
				}
			}
		})
	}
}
