package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/store"
)

func TestRetentionSettingsDefaultsAndEnvironment(t *testing.T) {
	settings := retentionTestSettings(t)
	if settings.retention != store.DefaultRetentionPolicy() {
		t.Fatalf("incorrect retention defaults: %+v", settings.retention)
	}
	for suffix, value := range map[string]string{
		"REQUEST_LOGS": "0", "PAYLOADS": "1d", "GUARDRAIL_EVENTS": "2d", "SHADOW_REQUESTS": "3d",
		"LIMITED_AND_DENIED": "4d", "SWEEP_INTERVAL": "20ms", "BATCH_SIZE": "2",
	} {
		t.Setenv("OMNISWITCH_RETENTION_"+suffix, value)
	}
	settings, err := loadRuntimeSettings()
	want := store.RetentionPolicy{Payloads: 24 * time.Hour, GuardrailEvents: 2 * 24 * time.Hour, ShadowRequests: 3 * 24 * time.Hour, LimitedAndDenied: 4 * 24 * time.Hour, SweepInterval: 20 * time.Millisecond, BatchSize: 2}
	if err != nil || settings.retention != want {
		t.Fatalf("environment policy=%+v want=%+v err=%v", settings.retention, want, err)
	}
}

func TestRetentionSettingsConfigAndEnvironmentPrecedence(t *testing.T) {
	retentionTestSettings(t)
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	body := "apiVersion: omniswitch.dev/v1\nkind: GatewayConfig\nretention:\n  request_logs: 10d\n  payloads: 0\n  guardrail_events: 12h\n  shadow_requests: 2d\n  limited_and_denied: 3d\n  sweep_interval: 5m\n  batch_size: 2\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMNISWITCH_CONFIG", path)
	settings, err := loadRuntimeSettings()
	want := store.RetentionPolicy{RequestLogs: 10 * 24 * time.Hour, GuardrailEvents: 12 * time.Hour, ShadowRequests: 2 * 24 * time.Hour, LimitedAndDenied: 3 * 24 * time.Hour, SweepInterval: 5 * time.Minute, BatchSize: 2}
	if err != nil || settings.retention != want {
		t.Fatalf("file policy=%+v want=%+v err=%v", settings.retention, want, err)
	}
	t.Setenv("OMNISWITCH_RETENTION_REQUEST_LOGS", "0")
	t.Setenv("OMNISWITCH_RETENTION_BATCH_SIZE", "3")
	settings, err = loadRuntimeSettings()
	want.RequestLogs, want.BatchSize = 0, 3
	if err != nil || settings.retention != want {
		t.Fatalf("env did not override file: %+v want=%+v err=%v", settings.retention, want, err)
	}
}

func TestRetentionInvalidEnvironmentFailsStartup(t *testing.T) {
	for _, entry := range []struct{ suffix, value string }{
		{"REQUEST_LOGS", "typo"}, {"PAYLOADS", "-1d"}, {"GUARDRAIL_EVENTS", "-1h"}, {"SHADOW_REQUESTS", "-1d"},
		{"LIMITED_AND_DENIED", "-1d"}, {"SWEEP_INTERVAL", "0"}, {"BATCH_SIZE", "0"}, {"BATCH_SIZE", "invalid"},
	} {
		t.Run(entry.suffix+entry.value, func(t *testing.T) {
			retentionTestSettings(t)
			t.Setenv("OMNISWITCH_RETENTION_"+entry.suffix, entry.value)
			if _, err := loadRuntimeSettings(); err == nil {
				t.Fatalf("invalid %s accepted: %q", entry.suffix, entry.value)
			}
		})
	}
}

func TestRetentionStartupLogPrintsEffectivePolicy(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	logRetentionPolicy(store.DefaultRetentionPolicy())
	for _, text := range []string{"Retention: request logs 30d", "payloads 7d", "guardrail events 30d", "shadow requests 7d", "limited/denied 30d", "batch 5000", "set 0 to keep forever"} {
		if !strings.Contains(output.String(), text) {
			t.Errorf("startup log missing %q: %s", text, output.String())
		}
	}
	if retentionDuration(0) != "0" || retentionDuration(time.Minute) != "1m0s" {
		t.Fatal("effective zero/sub-day policy display is incorrect")
	}
}
