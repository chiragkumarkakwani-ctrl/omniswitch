package gatewayconfig

import (
	"encoding/json"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func TestRetentionConfigParsesJSONAndYAML(t *testing.T) {
	for name, decode := range map[string]func(*Config) error{
		"json": func(cfg *Config) error {
			return json.Unmarshal([]byte(`{"kind":"GatewayConfig","retention":{"request_logs":"30d","payloads":0,"guardrail_events":"720h","shadow_requests":"7d","limited_and_denied":"30d","sweep_interval":"1h","batch_size":5000}}`), cfg)
		},
		"yaml": func(cfg *Config) error {
			return yaml.Unmarshal([]byte("kind: GatewayConfig\nretention:\n  request_logs: 30d\n  payloads: 0\n  guardrail_events: 720h\n  shadow_requests: 7d\n  limited_and_denied: 30d\n  sweep_interval: 1h\n  batch_size: 5000\n"), cfg)
		},
	} {
		t.Run(name, func(t *testing.T) {
			var cfg Config
			if err := decode(&cfg); err != nil {
				t.Fatal(err)
			}
			if err := Validate(cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Retention.RequestLogs.Duration != 30*24*time.Hour || cfg.Retention.Payloads == nil || cfg.Retention.Payloads.Duration != 0 || cfg.Retention.GuardrailEvents.Duration != 30*24*time.Hour || cfg.Retention.ShadowRequests.Duration != 7*24*time.Hour || cfg.Retention.LimitedAndDenied.Duration != 30*24*time.Hour || cfg.Retention.SweepInterval.Duration != time.Hour || *cfg.Retention.BatchSize != 5000 {
				t.Fatalf("incorrect retention config: %+v", cfg.Retention)
			}
		})
	}
}

func TestRetentionDurationParsing(t *testing.T) {
	for value, want := range map[string]time.Duration{"30d": 30 * 24 * time.Hour, " 7d ": 7 * 24 * time.Hour, "0": 0, "0d": 0, "1h30m": 90 * time.Minute, "500ms": 500 * time.Millisecond, "-1d": -24 * time.Hour} {
		got, err := ParseRetentionDuration(value)
		if err != nil || got != want {
			t.Fatalf("parse %q=%v want=%v err=%v", value, got, want, err)
		}
	}
	for _, value := range []string{"", "forever", "30days", "1.5d", "9999999999999999999999d", "106752d", "-106752d"} {
		if _, err := ParseRetentionDuration(value); err == nil {
			t.Fatalf("invalid duration accepted: %q", value)
		}
	}
}

func TestRetentionConfigValidation(t *testing.T) {
	for _, field := range []string{"request_logs", "payloads", "guardrail_events", "shadow_requests", "limited_and_denied", "sweep_interval"} {
		var cfg Config
		body := `{"kind":"GatewayConfig","retention":{"` + field + `":"-1h"}}`
		if err := json.Unmarshal([]byte(body), &cfg); err != nil {
			t.Fatal(err)
		}
		if err := Validate(cfg); err == nil {
			t.Fatalf("negative %s accepted", field)
		}
	}
	for _, body := range []string{`{"kind":"GatewayConfig","retention":{"sweep_interval":"0"}}`, `{"kind":"GatewayConfig","retention":{"batch_size":0}}`, `{"kind":"GatewayConfig","retention":{"batch_size":-1}}`} {
		var cfg Config
		if err := json.Unmarshal([]byte(body), &cfg); err != nil {
			t.Fatal(err)
		}
		if err := Validate(cfg); err == nil {
			t.Fatalf("invalid retention config accepted: %s", body)
		}
	}
}
