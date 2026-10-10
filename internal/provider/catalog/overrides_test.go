package catalog

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileAndOverridesMergeInOrder(t *testing.T) {
	file := filepath.Join(t.TempDir(), "prices.json")
	body := `{"models":[{"provider":"openai","id":"gpt-4o-mini","input_per_million":2,"per_image":{"1024x1024/high":0.04}},{"provider":"local","id":"small","input_per_million":0,"output_per_million":0}]}`
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cached := .5
	effective, err := Load(file, []Override{{Provider: "openai", Model: "gpt-4o-mini", InputPerMillion: 3, OutputPerMillion: 4, CachedInputPerMillion: &cached}, {Provider: "local", Model: "custom", InputPerMillion: 1, OutputPerMillion: 2}})
	if err != nil {
		t.Fatal(err)
	}
	model, found := effective.Lookup("OPENAI", "GPT-4O-MINI")
	if !found || model.Source != "override" || model.InputPerMillion != 3 || model.OutputPerMillion != 4 || *model.CachedInputPerMillion != .5 || model.PerImage["1024x1024/high"] != .04 {
		t.Fatalf("merged model = %+v", model)
	}
	if model.SourceURL == "" || model.VerifiedOn != "2026-10-09" {
		t.Fatal("file overlay lost provenance")
	}
	free, _ := effective.Lookup("local", "small")
	if !free.Priced || free.Source != "file" || free.InputPerMillion != 0 {
		t.Fatal("explicit free price was lost")
	}
	custom, _ := effective.Lookup("local", "custom")
	if custom.Source != "override" || custom.VerifiedOn != "" || custom.SourceURL != "" {
		t.Fatal("operator override invented provenance")
	}
	if effective.File() != file || effective.OverrideCount() != 2 || effective.VerifiedOn() != "2026-09-11" {
		t.Fatal("catalog summary metadata mismatch")
	}
	model.PerImage["1024x1024/high"] = 100
	*model.CachedInputPerMillion = 100
	again, _ := effective.Lookup("openai", "gpt-4o-mini")
	if again.PerImage["1024x1024/high"] != .04 || *again.CachedInputPerMillion != .5 {
		t.Fatal("lookup leaked mutable catalog data")
	}
	all := effective.All()
	all[0].ID = "mutated"
	if effective.All()[0].ID == "mutated" {
		t.Fatal("all leaked models")
	}
	old := Activate(effective)
	t.Cleanup(func() { Activate(old) })
	if Active() != effective || Activate(nil) != effective || len(ForProvider("local")) != 2 {
		t.Fatal("catalog activation mismatch")
	}
}

func TestCatalogValidation(t *testing.T) {
	for _, body := range []string{
		`invalid`, `{}`, `{"models":null}`, `{"models":[3]}`, `{"models":[{}]}`,
		`{"models":[{"provider":"local","id":"m","input_per_million":-1}]}`,
		`{"models":[{"provider":"local","id":"m","input_per_million":null}]}`,
		`{"models":[{"provider":"local","id":"m","per_image":{"":1}}]}`,
		`{"models":[{"provider":"local","id":"m","per_audio_minute":-1}]}`,
		`{"models":[{"provider":"local","id":"m","context_window":-1}]}`,
		`{"models":[{"provider":"local","id":"m"},{"provider":"local","id":"m"}]}`,
	} {
		file := filepath.Join(t.TempDir(), "prices.json")
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(file, nil); err == nil || !strings.Contains(err.Error(), "pricing.catalog_file") {
			t.Fatalf("Load(%s) = %v", body, err)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing"), nil); err == nil {
		t.Fatal("missing file accepted")
	}
	for _, override := range []Override{
		{}, {Provider: "local", Model: "m", InputPerMillion: -1}, {Provider: "local", Model: "m", OutputPerMillion: math.Inf(1)},
	} {
		if _, err := Load("", []Override{override}); err == nil {
			t.Fatalf("invalid override = %+v", override)
		}
	}
	negative := -1.0
	if err := ValidateOverrides([]Override{{Provider: "local", Model: "m", CachedInputPerMillion: &negative}}); err == nil {
		t.Fatal("negative cached price accepted")
	}
}

func TestCheckedPresetRowsAndDates(t *testing.T) {
	effective, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"mistral", "xai", "deepseek", "together", "fireworks", "perplexity", "cerebras", "openrouter"} {
		models := effective.ForProvider(provider)
		if len(models) == 0 {
			t.Fatalf("no real catalog rows for %s", provider)
		}
		for _, model := range models {
			if model.SourceURL == "" || model.VerifiedOn != "2026-10-09" || !model.Priced {
				t.Fatalf("unchecked preset price: %+v", model)
			}
		}
	}
	if len(effective.ForProvider("custom-preset-reference")) != 0 {
		t.Fatal("pseudo rows remain")
	}
	untouched, _ := effective.Lookup("openai", "gpt-4-turbo")
	if untouched.VerifiedOn != "2026-09-11" {
		t.Fatal("unchecked verification date was changed")
	}
}
