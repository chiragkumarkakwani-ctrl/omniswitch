package gatewayconfig

import (
	"bytes"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The supported-key inventory uses full paths: [] means a struct-list item,
// and * means a named map entry. Matching bare leaf names would miss fields
// with identical names in different sections (for example timeout or enabled).
func TestConfigurationDocsCoverYAMLFields(t *testing.T) {
	doc := configurationDoc(t)
	fields := configYAMLFields(reflect.TypeOf(Config{}), "")
	documented := documentedConfigKeys(t, doc)
	for path := range fields {
		if !documented[path] {
			t.Errorf("missing documented YAML field %s", path)
		}
	}
	// Reverse guard: removed fields or typos must not remain supported claims.
	for path := range documented {
		typ, ok := fields[path]
		if !ok {
			t.Errorf("documented YAML key %s does not exist in Config", path)
			continue
		}
		t.Run(path, func(t *testing.T) {
			value := sampleYAMLValue(typ)
			parts := strings.Split(path, ".")
			for i := len(parts) - 1; i >= 0; i-- {
				part := parts[i]
				if part == "*" {
					part = "example"
				}
				if strings.HasSuffix(part, "[]") {
					part = strings.TrimSuffix(part, "[]")
					value = []any{value}
				}
				value = map[string]any{part: value}
			}
			data, err := yaml.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			decoder := yaml.NewDecoder(bytes.NewReader(data))
			decoder.KnownFields(true)
			var cfg Config
			if err := decoder.Decode(&cfg); err != nil {
				t.Fatalf("documented key does not parse: %v\n%s", err, data)
			}
		})
	}
}

func TestConfigurationYAMLExamplesParse(t *testing.T) {
	blocks := regexp.MustCompile("(?s)```yaml\\n(.*?)\\n```").FindAllStringSubmatch(configurationDoc(t), -1)
	for index, block := range blocks {
		var header struct {
			Kind string `yaml:"kind"`
		}
		if err := yaml.Unmarshal([]byte(block[1]), &header); err != nil {
			t.Fatalf("YAML example %d: %v", index+1, err)
		}
		if header.Kind != "" && header.Kind != Kind {
			continue // ModelCatalog and other separately typed documents.
		}
		decoder := yaml.NewDecoder(strings.NewReader(block[1]))
		decoder.KnownFields(true)
		var cfg Config
		if err := decoder.Decode(&cfg); err != nil {
			t.Fatalf("gateway YAML example %d does not parse: %v", index+1, err)
		}
		if cfg.APIVersion == "" {
			cfg.APIVersion = APIVersion
		}
		if cfg.Kind == "" {
			cfg.Kind = Kind
		}
		if err := Validate(cfg); err != nil {
			t.Fatalf("gateway YAML example %d fails validation: %v", index+1, err)
		}
	}
}

func configurationDoc(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../docs/CONFIGURATION.md")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func documentedConfigKeys(t *testing.T, doc string) map[string]bool {
	t.Helper()
	_, inventory, ok := strings.Cut(doc, "## Supported YAML keys\n")
	if !ok {
		t.Fatal("CONFIGURATION.md needs a Supported YAML keys inventory")
	}
	inventory, _, _ = strings.Cut(inventory, "\n## ")
	rows := regexp.MustCompile("(?m)^\\| `([^`]+)` \\|").FindAllStringSubmatch(inventory, -1)
	keys := make(map[string]bool, len(rows))
	for _, row := range rows {
		if keys[row[1]] {
			t.Errorf("duplicate documented key %s", row[1])
		}
		keys[row[1]] = true
	}
	if len(keys) == 0 {
		t.Fatal("supported-key inventory is empty")
	}
	return keys
}

func configYAMLFields(typ reflect.Type, prefix string) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Slice, reflect.Array:
		return configYAMLFields(typ.Elem(), prefix+"[]")
	case reflect.Map:
		return configYAMLFields(typ.Elem(), prefix+".*")
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name := strings.Split(field.Tag.Get("yaml"), ",")[0]
			if !field.IsExported() || name == "" || name == "-" {
				continue
			}
			path := name
			if prefix != "" {
				path = prefix + "." + name
			}
			fields[path] = field.Type
			for child, childType := range configYAMLFields(field.Type, path) {
				fields[child] = childType
			}
		}
	}
	return fields
}

func sampleYAMLValue(typ reflect.Type) any {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeOf(Duration{}) {
		return "1s"
	}
	switch typ.Kind() {
	case reflect.Struct:
		return map[string]any{}
	case reflect.Slice, reflect.Array:
		return []any{sampleYAMLValue(typ.Elem())}
	case reflect.Map:
		return map[string]any{"example": sampleYAMLValue(typ.Elem())}
	case reflect.Bool:
		return true
	case reflect.Int, reflect.Int32, reflect.Int64:
		return 1
	case reflect.Float32, reflect.Float64:
		return 0.5
	default:
		return "example"
	}
}
